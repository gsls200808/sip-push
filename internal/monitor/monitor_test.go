package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sip-push/internal/ami"
	"sip-push/internal/notify"
)

type queryCall struct {
	tech string
	ext  string
}

type fakeQuerier struct {
	mu       sync.Mutex
	calls    []queryCall
	statuses map[string]*ami.PresenceStatus // key: "TECH/ext"
	err      error
	queried  chan queryCall
}

func (f *fakeQuerier) CheckOnline(ctx context.Context, tech, ext string) (*ami.PresenceStatus, error) {
	key := strings.ToUpper(tech) + "/" + ext
	f.mu.Lock()
	f.calls = append(f.calls, queryCall{strings.ToUpper(tech), ext})
	st := f.statuses[key]
	f.mu.Unlock()
	select {
	case f.queried <- queryCall{strings.ToUpper(tech), ext}:
	default:
	}
	if f.err != nil {
		return nil, f.err
	}
	if st == nil {
		return &ami.PresenceStatus{Found: false}, nil
	}
	return st, nil
}

func (f *fakeQuerier) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakePusher struct {
	name   string
	mu     sync.Mutex
	pushes []pushMsg
	got    chan pushMsg
	err    error
}

type pushMsg struct{ info notify.Info }

func (p *fakePusher) Name() string {
	if p.name == "" {
		return "fake"
	}
	return p.name
}

func (p *fakePusher) Push(ctx context.Context, info notify.Info) error {
	p.mu.Lock()
	p.pushes = append(p.pushes, pushMsg{info})
	p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.got <- pushMsg{info}
	return nil
}

func (p *fakePusher) snapshot() []pushMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]pushMsg, len(p.pushes))
	copy(out, p.pushes)
	return out
}

func newTestMonitor(q PresenceQuerier, p *fakePusher) *Monitor {
	return newTestMonitorMulti(q, p)
}

// newTestMonitorMulti 用多个渠道创建监控器
func newTestMonitorMulti(q PresenceQuerier, ps ...*fakePusher) *Monitor {
	pushers := make([]notify.Pusher, len(ps))
	for i, p := range ps {
		pushers[i] = p
	}
	m, err := New(q, pushers, Config{
		Technologies: []string{"PJSIP", "IAX2"},
		ExtPattern:   `^[0-9*#]{2,8}$`,
		DedupWindow:  time.Minute,
	}, nopLogger{})
	if err != nil {
		panic(err)
	}
	return m
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...interface{}) {}

// dialBegin 构造 Asterisk 12+（含 16）真实字段的 DialBegin：被叫 leg 用 DestChannel
func dialBegin(dest, caller, linked string) ami.Frame {
	return ami.Frame{
		"Event":       "DialBegin",
		"Channel":     "PJSIP/200-00000abc",
		"DestChannel": dest,
		"CallerIDNum": caller,
		"UniqueID":    linked,
		"LinkedID":    linked,
	}
}

// varSet 构造 Asterisk VarSet 事件帧（read=dialplan 权限下接收）
func varSet(variable, value, caller, linked string) ami.Frame {
	return varSetCtx(variable, value, "macro-dial-one", caller, linked)
}

// varSetCtx 构造带 Context 的 VarSet 帧（区分 macro-dial 与 macro-dial-one）
func varSetCtx(variable, value, ctx, caller, linked string) ami.Frame {
	return ami.Frame{
		"Event":       "VarSet",
		"Channel":     "PJSIP/200-0000002b",
		"Context":     ctx,
		"CallerIDNum": caller,
		"UniqueID":    linked,
		"LinkedID":    linked,
		"Variable":    variable,
		"Value":       value,
	}
}

// newexten 构造 Newexten 事件帧
func newexten(appData, ctx, caller, linked string) ami.Frame {
	return ami.Frame{
		"Event":       "Newexten",
		"Channel":     "PJSIP/200-0000002b",
		"Context":     ctx,
		"Application": "Macro",
		"AppData":     appData,
		"CallerIDNum": caller,
		"UniqueID":    linked,
		"LinkedID":    linked,
	}
}

// dialBeginLegacy 构造极旧版本 Asterisk 字段（Destination）的 DialBegin
func dialBeginLegacy(dest, caller, linked string) ami.Frame {
	return ami.Frame{
		"Event":       "DialBegin",
		"Destination": dest,
		"CallerIDNum": caller,
		"UniqueID":    linked,
		"LinkedID":    linked,
	}
}

func waitQuery(t *testing.T, ch <-chan queryCall) queryCall {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("等待查询超时")
		return queryCall{}
	}
}

func waitPush(t *testing.T, ch chan pushMsg) notify.Info {
	t.Helper()
	select {
	case m := <-ch:
		return m.info
	case <-time.After(2 * time.Second):
		t.Fatal("等待推送超时")
		return notify.Info{}
	}
}

func TestMultiChannelFanOut(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 8),
	}
	bark := &fakePusher{name: "bark", got: make(chan pushMsg, 8)}
	yak := &fakePusher{name: "yakphone", got: make(chan pushMsg, 8)}
	fail := &fakePusher{name: "bad", got: make(chan pushMsg, 8), err: context.DeadlineExceeded}
	m := newTestMonitorMulti(q, bark, yak, fail)

	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "13800001111", "L1"))

	// 每个渠道各收到一次推送；单个渠道失败不影响其他渠道
	_ = waitPush(t, bark.got)
	_ = waitPush(t, yak.got)
	time.Sleep(100 * time.Millisecond)
	if n := len(bark.snapshot()); n != 1 || len(yak.snapshot()) != 1 {
		t.Fatalf("两渠道应各推送一次: bark=%d yak=%d", n, len(yak.snapshot()))
	}
	if info := bark.snapshot()[0].info; info.Ext != "210" || info.CallerNum != "13800001111" ||
		info.Title == "" || info.Body == "" {
		t.Fatalf("通知信息异常: %+v", info)
	}
}

func TestOfflineExtensionTriggersPush(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 4),
	}
	p := &fakePusher{got: make(chan pushMsg, 4)}
	m := newTestMonitor(q, p)

	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "13800001111", "L1"))

	msg := waitPush(t, p.got)
	if !strings.Contains(msg.Title, "210") || !strings.Contains(msg.Body, "13800001111") ||
		!strings.Contains(msg.Body, "未注册") {
		t.Fatalf("推送内容异常: %+v", msg)
	}
}

func TestOnlineExtensionNoPush(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: true}},
		queried:  make(chan queryCall, 4),
	}
	p := &fakePusher{got: make(chan pushMsg, 4)}
	m := newTestMonitor(q, p)

	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "138", "L1"))

	if c := waitQuery(t, q.queried); c.tech != "PJSIP" || c.ext != "210" {
		t.Fatalf("查询参数异常: %+v", c)
	}
	// 给 goroutine 一点时间确认它没有推送
	time.Sleep(100 * time.Millisecond)
	if len(p.snapshot()) != 0 {
		t.Fatalf("在线分机不应推送: %+v", p.snapshot())
	}
}

func TestExtensionNotFoundNoPush(t *testing.T) {
	q := &fakeQuerier{queried: make(chan queryCall, 4)} // 无任何 status
	p := &fakePusher{got: make(chan pushMsg, 4)}
	m := newTestMonitor(q, p)

	m.OnEvent("DialBegin", dialBegin("PJSIP/999", "138", "L1"))
	waitQuery(t, q.queried)
	time.Sleep(100 * time.Millisecond)
	if len(p.snapshot()) != 0 {
		t.Fatalf("分机不存在不应推送: %+v", p.snapshot())
	}
}

func TestDedupSameCall(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	// 同一 LinkedID 多次 DialBegin（振铃组/重试），只应推一次
	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "138", "L1"))
	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "138", "L1"))
	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "138", "L1"))
	_ = waitPush(t, p.got)
	time.Sleep(100 * time.Millisecond)
	if n := q.queryCount(); n != 1 {
		t.Fatalf("同一通呼叫只应查询一次，实际 %d", n)
	}
	if len(p.snapshot()) != 1 {
		t.Fatalf("同一通呼叫只应推送一次，实际 %d", len(p.snapshot()))
	}
}

func TestDifferentCallsPushSeparately(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "138", "L1"))
	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "139", "L2"))
	_ = waitPush(t, p.got)
	_ = waitPush(t, p.got)
	if len(p.snapshot()) != 2 {
		t.Fatalf("两通呼叫应推送两次，实际 %d", len(p.snapshot()))
	}
}

func TestIgnoreUnknownTechAndInvalidExt(t *testing.T) {
	q := &fakeQuerier{queried: make(chan queryCall, 8)}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	// 未启用的 SIP 通道、1 位号码、9 位号码（外线）都应被忽略
	m.OnEvent("DialBegin", dialBegin("SIP/trunk-out/13800001111", "210", "L1"))
	m.OnEvent("DialBegin", dialBegin("PJSIP/9", "138", "L2"))
	m.OnEvent("DialBegin", dialBegin("PJSIP/13800001111", "139", "L3"))
	// 非 DialBegin 事件
	m.OnEvent("Newchannel", dialBegin("PJSIP/210", "138", "L4"))
	time.Sleep(100 * time.Millisecond)
	if q.queryCount() != 0 {
		t.Fatalf("不应发起任何查询，实际 %d 次: %v", q.queryCount(), q.calls)
	}
	if len(p.snapshot()) != 0 {
		t.Fatalf("不应推送，实际 %+v", p.snapshot())
	}
}

func TestUnknownCallerShown(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 4),
	}
	p := &fakePusher{got: make(chan pushMsg, 4)}
	m := newTestMonitor(q, p)

	f := dialBegin("PJSIP/210", "", "L1")
	delete(f, "CallerIDNum")
	m.OnEvent("DialBegin", f)
	msg := waitPush(t, p.got)
	if !strings.Contains(msg.Body, "未知号码") {
		t.Fatalf("未知主叫文案异常: %+v", msg)
	}
}

func TestIAX2OfflinePeerTriggersPush(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"IAX2/3001": {Found: true, Online: false}},
		queried:  make(chan queryCall, 4),
	}
	p := &fakePusher{got: make(chan pushMsg, 4)}
	m := newTestMonitor(q, p)

	// 已建链的 IAX2 通道名带 call 序号（-7），查询时必须剥掉
	m.OnEvent("DialBegin", dialBegin("IAX2/3001-7", "13800001111", "L1"))

	c := waitQuery(t, q.queried)
	if c.tech != "IAX2" || c.ext != "3001" {
		t.Fatalf("IAX2 查询参数异常: %+v", c)
	}
	msg := waitPush(t, p.got)
	if !strings.Contains(msg.Title, "3001") {
		t.Fatalf("推送内容异常: %+v", msg)
	}
}

func TestIAX2OnlinePeerNoPush(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"IAX2/3001": {Found: true, Online: true}},
		queried:  make(chan queryCall, 4),
	}
	p := &fakePusher{got: make(chan pushMsg, 4)}
	m := newTestMonitor(q, p)

	m.OnEvent("DialBegin", dialBegin("iax2/3001", "138", "L1")) // 小写技术名也应识别
	if c := waitQuery(t, q.queried); c.tech != "IAX2" || c.ext != "3001" {
		t.Fatalf("IAX2 查询参数异常: %+v", c)
	}
	time.Sleep(100 * time.Millisecond)
	if len(p.snapshot()) != 0 {
		t.Fatalf("在线 IAX2 peer 不应推送: %+v", p.snapshot())
	}
}

func TestDedupAcrossTechnologies(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{
			"PJSIP/210": {Found: true, Online: false},
			"IAX2/210":  {Found: true, Online: false},
		},
		queried: make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	// 同号不同技术应被视为两个目标
	m.OnEvent("DialBegin", dialBegin("PJSIP/210", "138", "L1"))
	m.OnEvent("DialBegin", dialBegin("IAX2/210-3", "138", "L1"))
	_ = waitPush(t, p.got)
	_ = waitPush(t, p.got)
	if q.queryCount() != 2 {
		t.Fatalf("不同技术应各查询一次，实际 %d", q.queryCount())
	}
}

// 模拟 FreePBX 对离线分机的真实事件序列：
// THISDIAL=<tech>/<ext> → THISDIAL 清空 → DIALSTATUS=CHANUNAVAIL（可能重复多次）
func freePBXOfflineSequence(techExt, caller, linked string) []ami.Frame {
	return []ami.Frame{
		varSet("THISDIAL", techExt, caller, linked),
		varSet("THISDIAL", "", caller, linked), // contacts 为空，FreePBX 清空 THISDIAL
		varSet("DIALSTATUS", "CHANUNAVAIL", caller, linked),
		varSet("DIALSTATUS", "CHANUNAVAIL", caller, linked), // exten-vm 再置一次
	}
}

func TestVarSetChanUnavailPJSIPTriggersPush(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	for _, f := range freePBXOfflineSequence("PJSIP/210", "200", "L10") {
		m.OnEvent("VarSet", f)
	}

	msg := waitPush(t, p.got)
	if !strings.Contains(msg.Title, "210") || !strings.Contains(msg.Body, "200") {
		t.Fatalf("推送内容异常: %+v", msg)
	}
	// 两次 CHANUNAVAIL 只应查询/推送一次
	time.Sleep(100 * time.Millisecond)
	if q.queryCount() != 1 {
		t.Fatalf("应只查询一次，实际 %d", q.queryCount())
	}
	if len(p.snapshot()) != 1 {
		t.Fatalf("应只推送一次，实际 %d", len(p.snapshot()))
	}
}

func TestVarSetChanUnavailIAX2TriggersPush(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"IAX2/220": {Found: true, Online: false}},
		queried:  make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	for _, f := range freePBXOfflineSequence("IAX2/220", "200", "L11") {
		m.OnEvent("VarSet", f)
	}

	msg := waitPush(t, p.got)
	if !strings.Contains(msg.Title, "220") || !strings.Contains(msg.Body, "200") {
		t.Fatalf("IAX2 推送内容异常: %+v", msg)
	}
}

func TestVarSetChanUnavailWithoutThisDialNoPush(t *testing.T) {
	q := &fakeQuerier{queried: make(chan queryCall, 8)}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	// 只有 CHANUNAVAIL、此前没有 THISDIAL（如外线中继故障），不应触发
	m.OnEvent("VarSet", varSet("DIALSTATUS", "CHANUNAVAIL", "200", "L12"))
	time.Sleep(100 * time.Millisecond)
	if q.queryCount() != 0 || len(p.snapshot()) != 0 {
		t.Fatalf("无 THISDIAL 不应触发: queries=%d pushes=%d", q.queryCount(), len(p.snapshot()))
	}
}

func TestVarSetNonUnavailStatusNoPush(t *testing.T) {
	q := &fakeQuerier{queried: make(chan queryCall, 8)}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	m.OnEvent("VarSet", varSet("THISDIAL", "PJSIP/210", "200", "L13"))
	// ANSWER/NOANSWER/BUSY 等状态与“离线不可达”无关，必须忽略
	for _, st := range []string{"ANSWER", "NOANSWER", "BUSY", "CANCEL"} {
		m.OnEvent("VarSet", varSet("DIALSTATUS", st, "200", "L13"))
	}
	time.Sleep(100 * time.Millisecond)
	if q.queryCount() != 0 || len(p.snapshot()) != 0 {
		t.Fatalf("非 CHANUNAVAIL 状态不应触发: queries=%d pushes=%d", q.queryCount(), len(p.snapshot()))
	}
}

func TestHangupFallbackEvaluatesCandidates(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	// macro-dial-one “无可拨成员”路径：有 THISDIAL 候选但无 DIALSTATUS，
	// Hangup 兜底评估仍应触发一次推送
	m.OnEvent("VarSet", varSet("THISDIAL", "PJSIP/210", "200", "L14"))
	m.OnEvent("Hangup", ami.Frame{"Event": "Hangup", "UniqueID": "x", "LinkedID": "L14"})
	_ = waitPush(t, p.got)

	// 挂断后状态已清理：迟到的 CHANUNAVAIL 不应再触发
	m.OnEvent("VarSet", varSet("DIALSTATUS", "CHANUNAVAIL", "200", "L14"))
	time.Sleep(100 * time.Millisecond)
	if q.queryCount() != 1 || len(p.snapshot()) != 1 {
		t.Fatalf("Hangup 兜底应只评估一次: queries=%d pushes=%d", q.queryCount(), len(p.snapshot()))
	}
}

// ---- 响铃组（macro-dial）信号链 ----

// 模拟 FreePBX 响铃组的真实事件序列（2026-09-13 在 192.168.11.21 实测抓帧）：
// 1. Newexten AppData: dial,20,HhTtrQ(NO_ANSWER),201-202-203-210-220 （全量成员）
// 2. Newexten AppData: ds= IAX2/220,20,... （dialparties 剔除离线 PJSIP 后的拨号串）
// 3. Dial 只对 IAX2/220 尝试，失败 → VarSet DIALSTATUS=CHANUNAVAIL (Context: macro-dial)
func TestRingGroupOfflinePJSIPImmediatePush(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{
			"PJSIP/201": {Found: true, Online: false},
			"PJSIP/202": {Found: true, Online: false},
			"PJSIP/203": {Found: true, Online: false},
			"PJSIP/210": {Found: true, Online: false},
			"IAX2/220":  {Found: true, Online: false},
		},
		queried: make(chan queryCall, 16),
	}
	p := &fakePusher{got: make(chan pushMsg, 16)}
	m := newTestMonitor(q, p)

	m.OnEvent("Newexten", newexten("dial,20,HhTtrQ(NO_ANSWER),201-202-203-210-220", "from-internal", "138", "L20"))
	m.OnEvent("Newexten", newexten("ds= IAX2/220,20,HhTtrQ(NO_ANSWER)M(auto-blkvm) ", "macro-dial", "138", "L20"))

	// 被 dialparties 剔除的 4 个 PJSIP 成员应立即推送（不等 20s 拨号超时）；
	// 推送顺序由并发判活决定，按集合断言
	pushed := map[string]bool{}
	for i := 0; i < 4; i++ {
		msg := waitPush(t, p.got)
		pushed[msg.Title] = true
	}
	for _, ext := range []string{"201", "202", "203", "210"} {
		if !pushed["分机 "+ext+" 有未接来电"] {
			t.Fatalf("缺少分机 %s 的推送: %+v", ext, pushed)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(p.snapshot()); n != 4 {
		t.Fatalf("剔除成员应推送 4 次，实际 %d: %+v", n, p.snapshot())
	}

	// ds 中但通道建不起来的 IAX2 成员，由 DIALSTATUS 补推
	m.OnEvent("VarSet", varSetCtx("DIALSTATUS", "CHANUNAVAIL", "macro-dial", "138", "L20"))
	msg := waitPush(t, p.got)
	if !strings.Contains(msg.Title, "220") {
		t.Fatalf("IAX2 成员应补推: %+v", msg)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(p.snapshot()); n != 5 {
		t.Fatalf("总共应推送 5 次，实际 %d", n)
	}
	// 二次 DIALSTATUS（macro-dial 的显式 Set）不应再推
	m.OnEvent("VarSet", varSetCtx("DIALSTATUS", "CHANUNAVAIL", "macro-dial", "138", "L20"))
	time.Sleep(100 * time.Millisecond)
	if n := len(p.snapshot()); n != 5 {
		t.Fatalf("重复评估不应重复推送，实际 %d", n)
	}
}

func TestRingGroupOnlineMemberWithLegNoPush(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/201": {Found: true, Online: true}},
		queried:  make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	m.OnEvent("Newexten", newexten("dial,20,HhTtrQ(NO_ANSWER),201", "from-internal", "138", "L21"))
	m.OnEvent("Newexten", newexten("ds=PJSIP/201,20,HhTtrQ(NO_ANSWER)M(auto-blkvm)", "macro-dial", "138", "L21"))
	// 201 在线振铃：有 DialBegin leg（DialBegin 信号路径会查询一次，在线不推送）
	m.OnEvent("DialBegin", dialBegin("PJSIP/201-0000002c", "138", "L21"))
	m.OnEvent("VarSet", varSetCtx("DIALSTATUS", "NOANSWER", "macro-dial", "138", "L21"))
	m.OnEvent("Hangup", ami.Frame{"Event": "Hangup", "UniqueID": "x", "LinkedID": "L21"})
	time.Sleep(100 * time.Millisecond)
	// 唯一一次查询来自 DialBegin 信号路径；响铃组评估因该成员有 leg 不应再查询
	if q.queryCount() != 1 || len(p.snapshot()) != 0 {
		t.Fatalf("在线成员不应推送: queries=%d pushes=%d", q.queryCount(), len(p.snapshot()))
	}
}

func TestRingGroupDialConfirmMembers(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{
			"PJSIP/210": {Found: true, Online: false},
			"PJSIP/201": {Found: true, Online: false},
		},
		queried: make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	// dial-confirm 的成员列表在倒数第二个字段（最后是组号）
	m.OnEvent("Newexten", newexten("dial-confirm,20,HhTtrQ(NO_ANSWER),201-210,299", "from-internal", "138", "L22"))
	m.OnEvent("Hangup", ami.Frame{"Event": "Hangup", "UniqueID": "x", "LinkedID": "L22"})
	// 推送顺序由并发判活决定，按集合断言
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		msg := waitPush(t, p.got)
		got[msg.Title] = true
	}
	for _, ext := range []string{"201", "210"} {
		if !got["分机 "+ext+" 有未接来电"] {
			t.Fatalf("缺少分机 %s 的推送: %+v", ext, got)
		}
	}
}

func TestRingGroupEmptyDialstringHangupFallback(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 8),
	}
	p := &fakePusher{got: make(chan pushMsg, 8)}
	m := newTestMonitor(q, p)

	// 全员被剔除（ds 为空）：无 ds= 帧、无 DIALSTATUS VarSet，仅 Hangup 兜底
	m.OnEvent("Newexten", newexten("dial,20,HhTtrQ(NO_ANSWER),210", "from-internal", "138", "L23"))
	m.OnEvent("Hangup", ami.Frame{"Event": "Hangup", "UniqueID": "x", "LinkedID": "L23"})
	msg := waitPush(t, p.got)
	if !strings.Contains(msg.Title, "210") {
		t.Fatalf("兜底推送异常: %+v", msg)
	}
}

func TestGroupMembers(t *testing.T) {
	cases := []struct {
		appData string
		want    []string
	}{
		{"dial,20,HhTtrQ(NO_ANSWER),201-202-203-210-220", []string{"201", "202", "203", "210", "220"}},
		{"dial-confirm,20,HhTtrQ(NO_ANSWER),201-210,299", []string{"201", "210"}},
		{"dial,20,HhTtrQ(NO_ANSWER),", nil},    // 空成员列表
		{"dial-one,20,HhTtrb(...),210", nil},   // 直呼宏不应匹配
		{"IAX2/220,20,HhTtrQ(NO_ANSWER)", nil}, // Dial 应用实参不应匹配
	}
	for _, c := range cases {
		got := groupMembers(c.appData)
		if len(got) != len(c.want) {
			t.Errorf("groupMembers(%q) = %v, want %v", c.appData, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("groupMembers(%q)[%d] = %q, want %q", c.appData, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseDialstringTargets(t *testing.T) {
	cases := []struct {
		appData string
		want    []string
	}{
		{"ds= IAX2/220,20,HhTtrQ(NO_ANSWER)M(auto-blkvm) ", []string{"IAX2/220"}},
		{"ds=PJSIP/201&PJSIP/202&IAX2/220,20,HhTtr", []string{"PJSIP/201", "PJSIP/202", "IAX2/220"}},
		{"ds= ", nil},
		{"0?Set(ds=PJSIP/201,20)", nil}, // ExecIf 实参不是 ds 帧
	}
	for _, c := range cases {
		got := parseDialstringTargets(c.appData)
		if len(got) != len(c.want) {
			t.Errorf("parseDialstringTargets(%q) = %v, want %v", c.appData, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseDialstringTargets(%q)[%d] = %q, want %q", c.appData, i, got[i], c.want[i])
			}
		}
	}
}

func TestLegacyDestinationField(t *testing.T) {
	q := &fakeQuerier{
		statuses: map[string]*ami.PresenceStatus{"PJSIP/210": {Found: true, Online: false}},
		queried:  make(chan queryCall, 4),
	}
	p := &fakePusher{got: make(chan pushMsg, 4)}
	m := newTestMonitor(q, p)

	// 旧版本 Asterisk 只有 Destination 字段，也应正常处理
	m.OnEvent("DialBegin", dialBeginLegacy("PJSIP/210", "138", "L9"))
	if c := waitQuery(t, q.queried); c.tech != "PJSIP" || c.ext != "210" {
		t.Fatalf("旧字段查询参数异常: %+v", c)
	}
}

func TestTarget(t *testing.T) {
	techs := []string{"PJSIP", "IAX2"}
	cases := []struct {
		dest     string
		wantTech string
		wantExt  string
	}{
		{"PJSIP/210", "PJSIP", "210"},
		{"pjsip/2001", "PJSIP", "2001"},
		{"PJSIP/1001/sip:1001@1.2.3.4", "PJSIP", "1001"},
		{"PJSIP/1001;@host", "PJSIP", "1001"},
		{"PJSIP/210-00000001", "PJSIP", "210"}, // 已建链 PJSIP 通道会话标记
		{"IAX2/3001", "IAX2", "3001"},
		{"IAX2/3001-42", "IAX2", "3001"}, // call 序号
		{"iax2/3001-1/1.2.3.4", "IAX2", "3001"},
		{"IAX2/3001-", "IAX2", "3001-"},         // 横线后为空，不是 call 序号
		{"Local/210@from-internal-1;1", "", ""}, // Local 通道不在白名单
		{"SIP/trunk/13800001111", "", ""},       // 未启用的技术
		{"PJSIP/", "PJSIP", ""},                 // 技术识别成功但无分机号（由 OnEvent 拦截）
		{"210", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		gotTech, gotExt := Target(c.dest, techs)
		if gotTech != c.wantTech || gotExt != c.wantExt {
			t.Errorf("Target(%q) = (%q, %q), want (%q, %q)",
				c.dest, gotTech, gotExt, c.wantTech, c.wantExt)
		}
	}
}
