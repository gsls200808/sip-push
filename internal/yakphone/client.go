// Package yakphone 实现 yakphone 软电话的 VoIP 来电推送客户端。
// 接口：POST {base_url}/v1/notify，Content-Type: application/json，载荷
//
//	{
//	  "token": "<设备令牌>",
//	  "caller_uri": "sip:1000@pbx.example.com",
//	  "caller_name": "Alice",
//	  "type": "voip"
//	}
//
// 用途是唤醒 yakphone App 弹出来电界面（CallKit/ConnectionService 级别的
// VoIP 推送），而非仅展示一条通知。
package yakphone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sip-push/internal/notify"
)

// Config yakphone 推送配置
type Config struct {
	// BaseURL 推送服务地址，默认官方 https://push.yakteam.com
	BaseURL string
	// Token 设备令牌（yakphone 分配）
	Token string
	// Domain 构造 caller_uri 的 SIP 域（PBX 的 SIP 域名或 IP）
	Domain string
	// PushTimeout 单次推送超时
	PushTimeout time.Duration
	// Extensions 绑定的分机号；留空或 ["*"]/["all"] 表示全部
	Extensions []string
}

// Logger 最小日志接口（*log.Logger 天然满足）
type Logger interface {
	Printf(format string, v ...interface{})
}

// Client yakphone 推送客户端
type Client struct {
	cfg    Config
	filter notify.ExtFilter
	logger Logger
	http   *http.Client
}

// New 创建 yakphone 推送客户端
func New(cfg Config, logger Logger) *Client {
	return &Client{
		cfg:    cfg,
		filter: notify.NewExtFilter(cfg.Extensions),
		logger: logger,
		http:   &http.Client{Timeout: cfg.PushTimeout},
	}
}

// Bindings 返回绑定分机的可读描述，用于启动日志
func (c *Client) Bindings() string { return c.filter.String() }

// Name 实现 notify.Pusher
func (c *Client) Name() string { return "yakphone" }

type payload struct {
	Token      string `json:"token"`
	CallerURI  string `json:"caller_uri"`
	CallerName string `json:"caller_name"`
	Type       string `json:"type"`
}

// Push 实现 notify.Pusher：向 yakphone 推送 VoIP 来电唤醒。
// 网络错误 / 5xx 重试一次，4xx 等确定性错误不重试。
func (c *Client) Push(ctx context.Context, info notify.Info) error {
	if !c.filter.Match(info.Ext) {
		return notify.ErrSkipped
	}
	p := payload{
		Token:      c.cfg.Token,
		CallerURI:  buildCallerURI(info.CallerNum, c.cfg.Domain),
		CallerName: buildCallerName(info),
		Type:       "voip",
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/notify"

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		retryable, err := c.once(ctx, url, raw)
		lastErr = err
		if err == nil {
			return nil
		}
		if !retryable {
			return err
		}
		c.logger.Printf("yakphone 推送第 %d 次失败: %v", attempt, err)
	}
	return lastErr
}

// once 执行一次请求，retryable 表示该错误是否值得重试（网络错误 / 5xx）。
func (c *Client) once(ctx context.Context, url string, raw []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.http.Do(req)
	if err != nil {
		return true, err // 网络错误可重试
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("yakphone 服务端错误 HTTP %d", resp.StatusCode)
	default:
		return false, fmt.Errorf("yakphone 请求失败 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
}

// buildCallerURI 构造 caller_uri：sip:<号码>@<域>。主叫号码缺失时用
// anonymous（与 SIP 匿名呼叫惯例一致）。
func buildCallerURI(callerNum, domain string) string {
	num := strings.TrimSpace(callerNum)
	if num == "" {
		num = "anonymous"
	}
	return "sip:" + num + "@" + domain
}

// buildCallerName 主叫显示名：优先 CallerIDName，回退号码原始值，最终兜底
// 规整后的展示文案（如"未知号码"）。
func buildCallerName(info notify.Info) string {
	if s := strings.TrimSpace(info.CallerName); s != "" && s != "<unknown>" {
		return s
	}
	if s := strings.TrimSpace(info.CallerNum); s != "" {
		return s
	}
	return info.Caller
}
