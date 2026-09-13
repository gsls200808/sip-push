// Package ami 实现 Asterisk Manager Interface (AMI) 1.x/2.x/5.x 文本协议客户端：
// TCP 长连接、登录认证、帧读写、ActionID 请求/响应关联、事件回调、心跳与断线自动重连。
//
// 协议帧格式：若干 "Key: Value" 行，以一个空行结束：
//
//	Action: Login\r\n
//	Username: admin\r\n
//	Secret: xxx\r\n
//	\r\n
package ami

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Frame AMI 帧（键全部按原样大小写保存，取值时用 Get 忽略大小写）
type Frame map[string]string

// Get 大小写不敏感地取值，缺省返回空串
func (f Frame) Get(key string) string {
	if v, ok := f[key]; ok {
		return v
	}
	for k, v := range f {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// EventHandler 异步事件回调（Newchannel / DialBegin / ...），在读取协程上执行，
// 实现方不得阻塞；耗时处理应自行转交其他协程。
type EventHandler func(event string, f Frame)

// Logger 最小日志接口（*log.Logger 天然满足）
type Logger interface {
	Printf(format string, v ...interface{})
}

// Config AMI 连接配置
type Config struct {
	Addr              string
	Username          string
	Secret            string
	ReconnectInterval time.Duration
	PingInterval      time.Duration
	DialTimeout       time.Duration
	ActionTimeout     time.Duration
}

// Client AMI 客户端，零值不可用，必须用 New 创建
type Client struct {
	cfg    Config
	logger Logger

	connMu sync.Mutex
	conn   net.Conn
	br     *bufio.Reader

	pendingMu sync.Mutex
	pending   map[string]chan Frame

	seq      uint64
	onEvent  EventHandler
	readySig chan struct{} // 每次成功登录后重新置位
}

// New 创建客户端
func New(cfg Config, logger Logger, onEvent EventHandler) *Client {
	if onEvent == nil {
		onEvent = func(string, Frame) {}
	}
	return &Client{
		cfg:      cfg,
		logger:   logger,
		pending:  make(map[string]chan Frame),
		onEvent:  onEvent,
		readySig: make(chan struct{}),
	}
}

// Ready 在登录成功后关闭返回的 channel；断线重连成功后会再次关闭新的 channel。
func (c *Client) Ready() <-chan struct{} {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.readySig
}

func (c *Client) setConn(conn net.Conn) {
	c.connMu.Lock()
	c.conn = conn
	if conn != nil {
		c.br = bufio.NewReaderSize(conn, 64*1024)
	} else {
		c.br = nil
		// 断线时为下一次登录准备新的就绪信号；
		// 旧的信号此前已被关闭，持有旧 channel 的等待方不会被遗漏。
		c.readySig = make(chan struct{})
	}
	c.connMu.Unlock()
}

func (c *Client) reader() *bufio.Reader {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.br
}

// Run 阻塞运行，反复连接直到 ctx 取消
func (c *Client) Run(ctx context.Context) {
	wait := time.Duration(0)
	for {
		if ctx.Err() != nil {
			return
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return
		}
		wait = c.cfg.ReconnectInterval
		if err != nil {
			c.logger.Printf("AMI 连接断开: %v，%s 后重连", err, wait)
		} else {
			c.logger.Printf("AMI 连接关闭，%s 后重连", wait)
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	dialer := &net.Dialer{Timeout: c.cfg.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.cfg.Addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.cfg.Addr, err)
	}
	c.setConn(conn)
	c.failAllPending(errors.New("连接重建中"))

	defer func() {
		conn.Close()
		c.setConn(nil)
		c.failAllPending(errors.New("连接已断开"))
	}()

	if err := c.login(conn); err != nil {
		return err
	}
	c.logger.Printf("AMI 已连接并登录成功: %s（%s）", c.cfg.Addr, c.cfg.Username)

	// 登录完成，放行 Ready
	c.connMu.Lock()
	ch := c.readySig
	c.connMu.Unlock()
	close(ch)

	return c.readLoop(ctx)
}

func (c *Client) login(conn net.Conn) error {
	br := c.reader()
	// 服务端首行：Asterisk Call Manager/x.y.z
	_ = conn.SetReadDeadline(time.Now().Add(c.cfg.DialTimeout))
	banner, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取 AMI banner: %w", err)
	}
	if !strings.Contains(banner, "Asterisk Call Manager") {
		return fmt.Errorf("不是 AMI 服务，banner=%q", strings.TrimSpace(banner))
	}
	if err := c.write(Frame{
		"Action":   "Login",
		"Username": c.cfg.Username,
		"Secret":   c.cfg.Secret,
		"Events":   "on",
	}); err != nil {
		return fmt.Errorf("发送 Login: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	resp, err := readFrame(br)
	if err != nil {
		return fmt.Errorf("读取 Login 响应: %w", err)
	}
	if resp.Get("Response") != "Success" {
		return fmt.Errorf("AMI 登录失败: %s", resp.Get("Message"))
	}
	return nil
}

func (c *Client) readLoop(ctx context.Context) error {
	br := c.reader()
	ping := time.NewTicker(c.cfg.PingInterval)
	defer ping.Stop()

	frameCh := make(chan Frame, 8)
	errCh := make(chan error, 1)
	go func() {
		for {
			f, err := readFrame(br)
			if err != nil {
				select {
				case errCh <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case frameCh <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-ping.C:
			cctx, cancel := context.WithTimeout(ctx, c.cfg.ActionTimeout)
			_, _ = c.DoAction(cctx, Frame{"Action": "Ping"})
			cancel()
		case f := <-frameCh:
			c.dispatch(f)
		}
	}
}

func (c *Client) dispatch(f Frame) {
	if aid := f.Get("ActionID"); aid != "" {
		c.pendingMu.Lock()
		ch, ok := c.pending[aid]
		c.pendingMu.Unlock()
		if ok {
			select {
			case ch <- f:
			default:
				// 接收方已超时，丢弃
			}
		}
		return
	}
	if ev := f.Get("Event"); ev != "" {
		c.onEvent(ev, f)
	}
}

// DoAction 发送一个 action 并关联响应。
// 普通动作返回首帧；列表动作（首帧带 EventList: start）持续收集，
// 直到收到 EventList: Complete（不含 Complete 帧本身）。
func (c *Client) DoAction(ctx context.Context, action Frame) ([]Frame, error) {
	br := c.reader()
	if br == nil {
		return nil, errors.New("AMI 未连接")
	}
	aid := strconv.FormatUint(uint64(time.Now().UnixNano()), 36) + "-" +
		strconv.FormatUint(atomic.AddUint64(&c.seq, 1), 36)
	ch := make(chan Frame, 32)
	c.pendingMu.Lock()
	c.pending[aid] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, aid)
		c.pendingMu.Unlock()
	}()

	action["ActionID"] = aid
	if err := c.write(action); err != nil {
		return nil, fmt.Errorf("发送动作: %w", err)
	}

	first, err := c.recvActionFrame(ctx, ch)
	if err != nil {
		return nil, err
	}
	if first.Get("Response") == "Error" {
		return nil, fmt.Errorf("AMI 动作 %s 被拒绝: %s", action.Get("Action"), first.Get("Message"))
	}
	if first.Get("EventList") != "start" {
		return []Frame{first}, nil
	}

	var items []Frame
	for {
		fr, err := c.recvActionFrame(ctx, ch)
		if err != nil {
			return nil, err
		}
		if fr.Get("EventList") == "Complete" {
			return items, nil
		}
		items = append(items, fr)
	}
}

func (c *Client) recvActionFrame(ctx context.Context, ch chan Frame) (Frame, error) {
	if c.cfg.ActionTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.cfg.ActionTimeout)
		defer cancel()
	}
	select {
	case f, ok := <-ch:
		if !ok {
			return nil, errors.New("等待 AMI 响应时连接断开")
		}
		return f, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("等待 AMI 响应超时: %w", ctx.Err())
	}
}

func (c *Client) failAllPending(err error) {
	c.pendingMu.Lock()
	m := c.pending
	c.pending = make(map[string]chan Frame)
	c.pendingMu.Unlock()
	for _, ch := range m {
		close(ch)
	}
}

func (c *Client) write(f Frame) error {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return errors.New("连接不存在")
	}
	var b strings.Builder
	// Action 行优先写，其余字段按字母序，ActionID 最后
	if a := f.Get("Action"); a != "" {
		fmt.Fprintf(&b, "Action: %s\r\n", a)
	}
	rest := make([]string, 0, len(f))
	for k := range f {
		if strings.EqualFold(k, "Action") || strings.EqualFold(k, "ActionID") {
			continue
		}
		rest = append(rest, k)
	}
	for _, k := range rest {
		fmt.Fprintf(&b, "%s: %s\r\n", k, f[k])
	}
	if aid := f.Get("ActionID"); aid != "" {
		fmt.Fprintf(&b, "ActionID: %s\r\n", aid)
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(conn, b.String())
	return err
}

// readFrame 读取一个以空行结束的 AMI 帧
func readFrame(br *bufio.Reader) (Frame, error) {
	f := Frame{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(f) == 0 {
				continue // 帧前多余空行
			}
			return f, nil
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		if _, exists := f[key]; !exists {
			f[key] = val
		}
	}
}
