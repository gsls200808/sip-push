// Package bark 实现 Bark 推送客户端（官方 https://api.day.app 或自建实例均可）。
// 文档：POST {baseURL}/push，JSON 载荷 {"device_key","title","body","group"}。
package bark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config Bark 配置
type Config struct {
	BaseURL     string
	DeviceKey   string
	Group       string
	PushTimeout time.Duration
}

// Logger 最小日志接口（*log.Logger 天然满足）
type Logger interface {
	Printf(format string, v ...interface{})
}

// Client Bark 客户端
type Client struct {
	cfg    Config
	logger Logger
	http   *http.Client
}

// New 创建 Bark 客户端
func New(cfg Config, logger Logger) *Client {
	return &Client{
		cfg:    cfg,
		logger: logger,
		http:   &http.Client{Timeout: cfg.PushTimeout},
	}
}

type payload struct {
	DeviceKey string `json:"device_key"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Group     string `json:"group,omitempty"`
}

type result struct {
	Code int    `json:"code"`
	Msg  string `json:"message"`
}

// Push 发送一条通知；网络错误 / 5xx 会重试一次，4xx 等确定性错误不重试。
func (c *Client) Push(ctx context.Context, title, body string) error {
	p := payload{DeviceKey: c.cfg.DeviceKey, Title: title, Body: body, Group: c.cfg.Group}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/push"

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
		c.logger.Printf("Bark 推送第 %d 次失败: %v", attempt, err)
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

	if resp.StatusCode == http.StatusOK {
		var r result
		if json.Unmarshal(data, &r) == nil && r.Code != 0 && r.Code != 200 {
			return false, fmt.Errorf("bark 返回错误码 %d: %s", r.Code, r.Msg)
		}
		return false, nil
	}
	if resp.StatusCode >= 500 {
		return true, fmt.Errorf("bark 服务端错误 HTTP %d", resp.StatusCode)
	}
	return false, fmt.Errorf("bark 请求失败 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}
