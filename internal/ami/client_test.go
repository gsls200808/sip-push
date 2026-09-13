package ami

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAMIServer 极简 AMI 测试服务器
type fakeAMIServer struct {
	t        *testing.T
	ln       net.Listener
	mu       sync.Mutex
	gotLogin Frame
}

func startFakeAMI(t *testing.T) *fakeAMIServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeAMIServer{t: t, ln: ln}
	return s
}

func (s *fakeAMIServer) addr() string { return s.ln.Addr().String() }

func (s *fakeAMIServer) close() { _ = s.ln.Close() }

// serve 处理一次连接：banner → 校验登录 → 发送一个 DialBegin 事件 →
// 响应后续的 PJSIPShowAors（AorList 帧 Contacts 非空即在线）。
func (s *fakeAMIServer) serve(contacts int, found bool) {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	fmt.Fprint(conn, "Asterisk Call Manager/5.0.4\r\n")
	login, err := readFrame(br)
	if err != nil {
		s.t.Errorf("read login: %v", err)
		return
	}
	s.mu.Lock()
	s.gotLogin = login
	s.mu.Unlock()
	if login.Get("Action") != "Login" {
		s.t.Errorf("期望 Login，得到 %v", login)
	}
	fmt.Fprint(conn, "Response: Success\r\nMessage: Authentication accepted\r\n\r\n")

	// 主动推送一个 DialBegin 异步事件（Asterisk 16 字段：DestChannel）
	fmt.Fprint(conn, strings.Join([]string{
		"Event: DialBegin",
		"Privilege: call,all",
		"Channel: PJSIP/200-00000abc",
		"DestChannel: PJSIP/210",
		"CallerIDNum: 13800001111",
		"UniqueID: 1520000000.1",
		"LinkedID: 1520000000.1",
		"", "",
	}, "\r\n"))

	// 等待 PJSIPShowAors
	act, err := readFrame(br)
	if err != nil {
		s.t.Errorf("read action: %v", err)
		return
	}
	if act.Get("Action") != "PJSIPShowAors" {
		s.t.Errorf("意外的动作帧: %v", act)
	}
	aid := act.Get("ActionID")
	fmt.Fprintf(conn, "Response: Success\r\nActionID: %s\r\nEventList: start\r\nMessage: AOR objects found\r\n\r\n", aid)
	if found {
		// Asterisk 16 的 AorList 帧没有 TotalContacts，靠 Contacts 非空判在线
		contactLine := "Contacts: "
		if contacts > 0 {
			contactLine = "Contacts: 210/sip:210@1.2.3.4:5060"
		}
		fmt.Fprintf(conn, strings.Join([]string{
			"Event: AorList",
			"ActionID: %s",
			"ObjectType: aor",
			"ObjectName: 210",
			contactLine,
			"", "",
		}, "\r\n"), aid)
	}
	listItems := 0
	if found {
		listItems = 1
	}
	fmt.Fprintf(conn, "Event: AorListComplete\r\nActionID: %s\r\nEventList: Complete\r\nListItems: %d\r\n\r\n", aid, listItems)

	// 保持连接一段时间，等待客户端取完
	time.Sleep(300 * time.Millisecond)
}

func TestClientLoginEventAndShowAOR(t *testing.T) {
	srv := startFakeAMI(t)
	defer srv.close()
	go srv.serve(1, true)

	events := make(chan Frame, 4)
	client := New(Config{
		Addr:              srv.addr(),
		Username:          "u",
		Secret:            "p",
		ReconnectInterval: time.Second,
		PingInterval:      time.Hour,
		DialTimeout:       2 * time.Second,
		ActionTimeout:     2 * time.Second,
	}, testLogger(), func(event string, f Frame) {
		if event == "DialBegin" {
			events <- f
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go client.Run(ctx)

	select {
	case <-client.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("等待登录成功超时")
	}
	if srv.gotLogin.Get("Username") != "u" {
		t.Fatalf("登录帧异常: %v", srv.gotLogin)
	}

	select {
	case ev := <-events:
		if ev.Get("DestChannel") != "PJSIP/210" || ev.Get("CallerIDNum") != "13800001111" {
			t.Fatalf("DialBegin 事件内容异常: %v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 DialBegin 事件")
	}

	st, err := client.ShowAOR(ctx, "210")
	if err != nil {
		t.Fatalf("ShowAOR: %v", err)
	}
	if !st.Found || st.TotalContacts != 1 {
		t.Fatalf("AOR 状态异常: %+v", st)
	}
}

func TestShowAORNotFound(t *testing.T) {
	srv := startFakeAMI(t)
	defer srv.close()
	go srv.serve(0, false)

	client := New(Config{
		Addr:              srv.addr(),
		Username:          "u",
		Secret:            "p",
		ReconnectInterval: time.Second,
		PingInterval:      time.Hour,
		DialTimeout:       2 * time.Second,
		ActionTimeout:     2 * time.Second,
	}, testLogger(), func(string, Frame) {})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go client.Run(ctx)
	<-client.Ready()

	// 服务器会先等 DialBegin？不会：该用例的 fake 仍发 DialBegin，
	// 但客户端无事件处理影响；直接消费事件由空回调丢弃。
	st, err := client.ShowAOR(ctx, "210")
	if err != nil {
		t.Fatalf("ShowAOR: %v", err)
	}
	if st.Found || st.TotalContacts != 0 {
		t.Fatalf("期望 AOR 不存在，得到 %+v", st)
	}
}

func testLogger() *testLog { return &testLog{} }

type testLog struct{}

func (l *testLog) Printf(format string, v ...interface{}) {}
