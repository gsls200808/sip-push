package yakphone

import (
	"context"
	"encoding/json"
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
		Token:       "tok123",
		Domain:      "pbx.example.com",
		PushTimeout: 2 * time.Second,
	}, nopLogger{})
}

func testInfo() notify.Info {
	return notify.Info{
		Tech:       "PJSIP",
		Ext:        "210",
		CallerNum:  "13800001111",
		CallerName: "张三",
		Caller:     "13800001111",
		Title:      "分机 210 有未接来电",
		Body:       "13800001111 呼叫分机 210",
	}
}

func TestPushSuccess(t *testing.T) {
	var mu sync.Mutex
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/notify" {
			t.Errorf("路径异常: %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct == "" {
			t.Error("缺少 Content-Type")
		}
		raw, _ := io.ReadAll(r.Body)
		var p payload
		_ = json.Unmarshal(raw, &p)
		mu.Lock()
		got = p
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Push(context.Background(), testInfo()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	want := payload{
		Token:      "tok123",
		CallerURI:  "sip:13800001111@pbx.example.com",
		CallerName: "张三",
		Type:       "voip",
	}
	if got != want {
		t.Fatalf("载荷异常:\n got %+v\nwant %+v", got, want)
	}
}

func TestPushCallerNameFallbacks(t *testing.T) {
	cases := []struct {
		name       string
		info       notify.Info
		wantURI    string
		wantCaller string
	}{
		{
			name:       "无显示名回退号码",
			info:       notify.Info{CallerNum: "138", Caller: "138"},
			wantURI:    "sip:138@pbx.example.com",
			wantCaller: "138",
		},
		{
			name:       "完全未知回退展示文案与 anonymous",
			info:       notify.Info{Caller: "未知号码"},
			wantURI:    "sip:anonymous@pbx.example.com",
			wantCaller: "未知号码",
		},
		{
			name:       "CallerIDName 为 unknown 占位时回退号码",
			info:       notify.Info{CallerNum: "1000", CallerName: "<unknown>", Caller: "1000"},
			wantURI:    "sip:1000@pbx.example.com",
			wantCaller: "1000",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var got payload
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var p payload
				_ = json.Unmarshal(raw, &p)
				mu.Lock()
				got = p
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			if err := newTestClient(srv.URL).Push(context.Background(), c.info); err != nil {
				t.Fatalf("Push: %v", err)
			}
			if got.CallerURI != c.wantURI || got.CallerName != c.wantCaller {
				t.Fatalf("uri/name 异常: got %q/%q want %q/%q",
					got.CallerURI, got.CallerName, c.wantURI, c.wantCaller)
			}
		})
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
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Push(context.Background(), testInfo()); err != nil {
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
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`bad token`))
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL).Push(context.Background(), testInfo()); err == nil {
		t.Fatal("401 应返回错误")
	}
	if n != 1 {
		t.Fatalf("4xx 不应重试，期望 1 次，实际 %d", n)
	}
}

func TestName(t *testing.T) {
	var c Client
	if got := c.Name(); got != "yakphone" {
		t.Fatalf("Name() = %q", got)
	}
}
