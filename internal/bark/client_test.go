package bark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"sip-push/internal/notify"
)

type nopLogger struct{}

func (nopLogger) Printf(string, ...interface{}) {}

func newTestClient(base string) *Client {
	return New(Config{
		BaseURL:     base,
		DeviceKey:   "devkey123",
		Group:       "sip-push",
		PushTimeout: 2 * time.Second,
	}, nopLogger{})
}

func TestPushSuccess(t *testing.T) {
	var mu sync.Mutex
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/push" {
			t.Errorf("路径异常: %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var p payload
		_ = json.Unmarshal(raw, &p)
		mu.Lock()
		got = p
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Push(context.Background(), notify.Info{Title: "标题", Body: "内容"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got.DeviceKey != "devkey123" || got.Title != "标题" || got.Body != "内容" || got.Group != "sip-push" {
		t.Fatalf("载荷异常: %+v", got)
	}
}

func TestPushSkippedWhenNotBound(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{
		BaseURL:     srv.URL,
		DeviceKey:   "k",
		PushTimeout: 2 * time.Second,
		Extensions:  []string{"210"},
	}, nopLogger{})

	// 未绑定分机：返回 ErrSkipped 且不应发出 HTTP 请求
	err := c.Push(context.Background(), notify.Info{Ext: "220", Title: "t", Body: "b"})
	if !errors.Is(err, notify.ErrSkipped) {
		t.Fatalf("期望 ErrSkipped，实际 %v", err)
	}
	// 绑定的分机正常推送
	if err := c.Push(context.Background(), notify.Info{Ext: "210", Title: "t", Body: "b"}); err != nil {
		t.Fatalf("已绑定分机应推送成功: %v", err)
	}
	if n != 1 {
		t.Fatalf("期望只发出 1 次 HTTP 请求，实际 %d", n)
	}
	if got := c.Bindings(); got != "210" {
		t.Fatalf("Bindings() = %q", got)
	}
}

func TestPushRetryOn5xx(t *testing.T) {
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if cur == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Push(context.Background(), notify.Info{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("5xx 后重试应成功: %v", err)
	}
	if n != 2 {
		t.Fatalf("期望请求 2 次，实际 %d", n)
	}
}

func TestPushNoRetryOn4xx(t *testing.T) {
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad`))
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Push(context.Background(), notify.Info{Title: "t", Body: "b"}); err == nil {
		t.Fatal("400 应返回错误")
	}
	if n != 1 {
		t.Fatalf("4xx 不应重试，期望 1 次，实际 %d", n)
	}
}
