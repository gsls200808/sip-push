package bark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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

	if err := newTestClient(srv.URL).Push(context.Background(), "标题", "内容"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got.DeviceKey != "devkey123" || got.Title != "标题" || got.Body != "内容" || got.Group != "sip-push" {
		t.Fatalf("载荷异常: %+v", got)
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

	if err := newTestClient(srv.URL).Push(context.Background(), "t", "b"); err != nil {
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

	if err := newTestClient(srv.URL).Push(context.Background(), "t", "b"); err == nil {
		t.Fatal("400 应返回错误")
	}
	if n != 1 {
		t.Fatalf("4xx 不应重试，期望 1 次，实际 %d", n)
	}
}
