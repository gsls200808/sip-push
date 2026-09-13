// Package notify 定义推送渠道的公共消息结构与接口。
// bark（通知条推送）与 yakphone（VoIP 来电唤醒推送）各自实现 Pusher，
// monitor 对所有已配置渠道扇出推送。
package notify

import "context"

// Info 一次离线来电通知的全部信息，由 monitor 组装。
// Title/Body 为面向通知条渠道（Bark）的展示文案；其余字段供 VoIP 唤醒类
// 渠道（yakphone）构造来电载荷。
type Info struct {
	Tech string // 被叫通道技术（PJSIP/IAX2）
	Ext  string // 被叫分机

	CallerNum  string // 主叫号码原始值（可能为空）
	CallerName string // 主叫显示名原始值（可能为空）
	Caller     string // 规整后的主叫展示文案（"未知号码" 兜底），Bark 正文用

	Title string // 通知标题（Bark 用）
	Body  string // 通知正文（Bark 用）
}

// Pusher 推送渠道能力
type Pusher interface {
	// Name 渠道名，用于日志区分（如 "bark"、"yakphone"）
	Name() string
	// Push 推送一条离线来电通知
	Push(ctx context.Context, info Info) error
}
