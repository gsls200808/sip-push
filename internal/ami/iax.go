package ami

import (
	"context"
	"strings"
)

// IAXPeerStatus IAX2 peer 查询结果
type IAXPeerStatus struct {
	Name   string
	Found  bool // peer 是否存在于 iax2 show peers
	Online bool // peer 当前已注册/可达
}

// ShowIAXPeer 通过 IAXpeerlist 查询指定 IAX2 peer 是否在线。
//
// IAX2 是 peer 注册模型，没有 PJSIP 的 AOR/contact 概念，AMI 也没有按名查询
// 单条 peer 的动作，因此先拉全量再按 peer 名过滤。
//
// 实测响应帧序列（Asterisk 16 / chan_iax2）：
//
//	Response: Success / EventList: start
//	Event: PeerEntry / Channeltype: IAX / ChanObjectType: peer
//	    ObjectName: <名字>
//	    IPaddress: <ip:port 或 (null)>
//	    Status: OK | LAGGED (xx ms) | UNREACHABLE | UNMONITORED | UNKNOWN
//	...
//	Event: PeerlistComplete / EventList: Complete
//
// 未注册的动态 peer：IPaddress=(null)、Status=UNKNOWN。
// 旧版本 Asterisk 使用事件名 IAXpeerlistEntry、字段 IP/ObjectType，一并兼容。
// 需要 AMI write 权限包含 system 或 reporting。
func (c *Client) ShowIAXPeer(ctx context.Context, name string) (*IAXPeerStatus, error) {
	frames, err := c.DoAction(ctx, Frame{
		"Action": "IAXpeerlist",
	})
	if err != nil {
		return nil, err
	}
	return parseIAXPeerStatus(frames, name), nil
}

// parseIAXPeerStatus 从 IAXpeerlist 条目帧中解析指定 peer 的状态（纯函数，便于测试）
func parseIAXPeerStatus(frames []Frame, name string) *IAXPeerStatus {
	st := &IAXPeerStatus{Name: name}
	for _, f := range frames {
		if !isIAXPeerEntry(f) {
			continue
		}
		peerName := f.Get("ObjectName")
		if peerName == "" {
			peerName = f.Get("Name") // 兼容旧版本
		}
		if peerName != name {
			continue
		}
		st.Found = true
		ip := f.Get("IPaddress")
		if ip == "" {
			ip = f.Get("IP") // 兼容旧版本
		}
		if iaxReachable(f.Get("Status"), ip) {
			st.Online = true
		}
	}
	return st
}

// isIAXPeerEntry 判断帧是否为 IAX peer 列表条目。
// Asterisk 16 为 PeerEntry+Channeltype=IAX；旧版本为 IAXpeerlistEntry。
func isIAXPeerEntry(f Frame) bool {
	ev := f.Get("Event")
	if ev == "IAXpeerlistEntry" {
		return true
	}
	if ev != "PeerEntry" {
		return false
	}
	ct := f.Get("Channeltype")
	return ct == "" || strings.EqualFold(ct, "IAX") || strings.EqualFold(ct, "IAX2")
}

// iaxReachable 依据 peer 条目的 Status/IP 判断 peer 是否可达。
//
//	OK / LAGGED (xx ms)：开启了 qualify 且探测正常（LAGGED 只是延迟高，仍可通话）
//	UNMONITORED：未开启 qualify —— 静态 peer 恒有配置地址；动态 peer 注册后才有地址，
//	             注册过期后地址会被清空，因此有真实 IP 即视为在线
//	UNREACHABLE / UNKNOWN / 其它/空状态：不可达（UNKNOWN 是未注册动态 peer 的典型状态）
func iaxReachable(status, ip string) bool {
	real := hasRealIAXAddr(ip)
	s := strings.ToUpper(strings.TrimSpace(status))
	switch {
	case strings.HasPrefix(s, "OK"), strings.HasPrefix(s, "LAGGED"):
		return real
	case s == "UNMONITORED":
		return real
	default:
		return false
	}
}

// hasRealIAXAddr 判断 peer 地址字段是否是真实注册地址。
// 未注册动态 peer 在 16.x 为 (null)，旧版本可能是 (Unspecified)/-/0.0.0.0。
func hasRealIAXAddr(ip string) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}
	switch strings.ToUpper(ip) {
	case "(NULL)", "(UNSPECIFIED)", "-", "0.0.0.0":
		return false
	}
	return true
}
