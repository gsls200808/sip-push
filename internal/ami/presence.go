package ami

import (
	"context"
	"fmt"
	"strings"
)

// PresenceStatus 与通道技术无关的统一分机在线状态
type PresenceStatus struct {
	Found  bool // 该分机对象是否存在（PJSIP AOR / IAX2 peer）
	Online bool // 当前是否有可达终端（contacts>0 或 peer 已注册/可达）
}

// CheckOnline 按通道技术查询分机在线状态。
// tech 大小写不敏感，目前支持 "PJSIP"（PJSIPShowAors）和 "IAX2"（IAXpeerlist）。
func (c *Client) CheckOnline(ctx context.Context, tech, ext string) (*PresenceStatus, error) {
	switch strings.ToUpper(strings.TrimSpace(tech)) {
	case "PJSIP":
		st, err := c.ShowAOR(ctx, ext)
		if err != nil {
			return nil, err
		}
		return &PresenceStatus{Found: st.Found, Online: st.TotalContacts > 0}, nil
	case "IAX2":
		st, err := c.ShowIAXPeer(ctx, ext)
		if err != nil {
			return nil, err
		}
		return &PresenceStatus{Found: st.Found, Online: st.Online}, nil
	default:
		return nil, fmt.Errorf("不支持的通道技术 %q（仅支持 PJSIP/IAX2）", tech)
	}
}
