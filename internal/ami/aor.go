package ami

import (
	"context"
	"strconv"
	"strings"
)

// AORStatus PJSIP AOR 查询结果
type AORStatus struct {
	Name          string
	Found         bool // AOR 是否存在
	TotalContacts int  // 当前绑定的 contact 数量；>0 即视为已注册/在线
}

// ShowAOR 执行 PJSIPShowAors 查询指定 AOR 的在线 contact 数。
//
// 注意：Asterisk 各版本的实际动作名是 PJSIPShowAors（列表动作，不支持按名
// 过滤），这里拉全量后在客户端按 ObjectName 过滤。
//
// 实测响应帧序列（Asterisk 16 / res_pjsip）：
//
//	Response: Success / EventList: start
//	Event: AorList / ObjectType: aor / ObjectName: <名字>
//	    Contacts: <非空表示有在线 contact，多个用逗号分隔>
//	...
//	Event: AorListComplete / EventList: Complete
//
// 较新版本（Asterisk 18+）的 AorList 帧还会带 TotalContacts，优先采用；
// 16.x 没有该字段，回退判断 Contacts 是否非空。
// 列表中找不到该名字表示 AOR 不存在（不推送，只记日志）。
// 需要 AMI write 权限包含 system。
func (c *Client) ShowAOR(ctx context.Context, name string) (*AORStatus, error) {
	frames, err := c.DoAction(ctx, Frame{
		"Action": "PJSIPShowAors",
	})
	if err != nil {
		return nil, err
	}
	st := &AORStatus{Name: name}
	for _, f := range frames {
		isAOR := f.Get("Event") == "AorList" ||
			strings.EqualFold(f.Get("ObjectType"), "aor")
		if !isAOR || f.Get("ObjectName") != name {
			continue
		}
		st.Found = true
		if n, err := strconv.Atoi(f.Get("TotalContacts")); err == nil && n > st.TotalContacts {
			st.TotalContacts = n
		}
		// 兼容没有 TotalContacts 的版本：Contacts 非空也算在线
		if st.TotalContacts == 0 && strings.TrimSpace(f.Get("Contacts")) != "" {
			st.TotalContacts = 1
		}
	}
	return st, nil
}
