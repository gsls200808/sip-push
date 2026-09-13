package ami

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// iaxEntry16 构造实测的 Asterisk 16 IAXpeerlist 条目帧：
// Event: PeerEntry / Channeltype: IAX / IPaddress
func iaxEntry16(name, ip, status string) Frame {
	return Frame{
		"Event":          "PeerEntry",
		"Channeltype":    "IAX",
		"ChanObjectType": "peer",
		"ObjectName":     name,
		"IPaddress":      ip,
		"Status":         status,
		"Dynamic":        "yes",
	}
}

// iaxEntryLegacy 构造旧版本 Asterisk 的条目帧：IAXpeerlistEntry/IP/Name
func iaxEntryLegacy(name, ip, status string) Frame {
	return Frame{
		"Event":      "IAXpeerlistEntry",
		"ObjectType": "peer",
		"Name":       name,
		"IP":         ip,
		"Status":     status,
		"Dynamic":    "yes",
	}
}

func TestParseIAXPeerStatus(t *testing.T) {
	frames := []Frame{
		iaxEntry16("3001", "10.0.0.2:4569", "OK (12 ms)"),
		iaxEntry16("3002", "(null)", "UNKNOWN"),                // 16.x 动态 peer 未注册
		iaxEntry16("3003", "10.0.0.3:4569", "LAGGED (250 ms)"), // LAGGED 仍可通话
		iaxEntry16("3004", "10.0.0.4:4569", "UNMONITORED"),     // 静态/未开 qualify
		iaxEntry16("3006", "(null)", "UNMONITORED"),            // 无地址仍离线
		iaxEntryLegacy("3005", "(Unspecified)", "UNREACHABLE"), // 旧版本字段
	}

	cases := []struct {
		name       string
		wantFound  bool
		wantOnline bool
	}{
		{"3001", true, true},  // OK
		{"3002", true, false}, // UNKNOWN + (null)：未注册
		{"3003", true, true},  // LAGGED
		{"3004", true, true},  // UNMONITORED 有地址
		{"3005", true, false}, // 旧版本 UNREACHABLE
		{"3006", true, false}, // UNMONITORED 但无地址
		{"3999", false, false},
	}
	for _, c := range cases {
		st := parseIAXPeerStatus(frames, c.name)
		if st.Found != c.wantFound || st.Online != c.wantOnline {
			t.Errorf("peer %s: got %+v, want Found=%v Online=%v",
				c.name, st, c.wantFound, c.wantOnline)
		}
	}
}

func TestIAXReachableEdgeCases(t *testing.T) {
	if iaxReachable("OK", "(null)") {
		t.Error("OK 但无真实 IP 不应判在线")
	}
	if iaxReachable("", "10.0.0.2:4569") {
		t.Error("空状态不应判在线")
	}
	if iaxReachable("UNKNOWN", "10.0.0.2:4569") {
		t.Error("UNKNOWN 状态不应判在线")
	}
	if !iaxReachable("ok (5 ms)", "  10.0.0.2:4569  ") {
		t.Error("小写 ok + 真实 IP 应判在线（大小写/空白需容错）")
	}
}

// serveIAXPeerlist 处理一次连接：登录后响应 IAXpeerlist，返回两个 peer。
// 帧格式与实测 Asterisk 16.30 一致（PeerEntry/IPaddress/PeerlistComplete）。
func serveIAXPeerlist(t *testing.T, ln net.Listener) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	fmt.Fprint(conn, "Asterisk Call Manager/5.0.5\r\n")
	login, err := readFrame(br)
	if err != nil {
		t.Errorf("read login: %v", err)
		return
	}
	if login.Get("Action") != "Login" {
		t.Errorf("期望 Login，得到 %v", login)
	}
	fmt.Fprint(conn, "Response: Success\r\nMessage: Authentication accepted\r\n\r\n")

	entry := func(aid, name, ip, status string) {
		fmt.Fprintf(conn, strings.Join([]string{
			"Event: PeerEntry",
			"ActionID: %s",
			"Channeltype: IAX",
			"ChanObjectType: peer",
			"ObjectName: %s",
			"IPaddress: %s",
			"Status: %s",
			"Dynamic: yes",
			"", "",
		}, "\r\n"), aid, name, ip, status)
	}
	// 测试会连续查询 3 次（在线/离线/不存在），逐次响应
	for i := 0; i < 3; i++ {
		act, err := readFrame(br)
		if err != nil {
			t.Errorf("read action: %v", err)
			return
		}
		if act.Get("Action") != "IAXpeerlist" {
			t.Errorf("期望 IAXpeerlist，得到 %v", act)
		}
		aid := act.Get("ActionID")
		fmt.Fprintf(conn, "Response: Success\r\nActionID: %s\r\nEventList: start\r\nMessage: IAX Peer status list will follow\r\n\r\n", aid)
		entry(aid, "3001", "10.0.0.2:4569", "OK (12 ms)")
		entry(aid, "3002", "(null)", "UNKNOWN")
		fmt.Fprintf(conn, "Event: PeerlistComplete\r\nActionID: %s\r\nEventList: Complete\r\nListItems: 2\r\n\r\n", aid)
	}

	time.Sleep(300 * time.Millisecond)
}

func TestCheckOnlineRouting(t *testing.T) {
	st, err := (&Client{}).CheckOnline(context.Background(), "DAHDI", "1")
	if err == nil || st != nil {
		t.Fatalf("不支持的技术应报错，得到 st=%v err=%v", st, err)
	}
}
