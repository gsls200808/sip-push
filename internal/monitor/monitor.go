// Package monitor 是核心业务编排：监听 AMI 事件 → 取目标分机 →
// 按通道技术（PJSIP: PJSIPShowAors / IAX2: IAXpeerlist）判断是否已注册 →
// 未注册时通过 Bark 推送未接来电提醒。
//
// 支持三类呼叫信号，互为补充：
//
//  1. DialBegin 事件（标准 Asterisk 行为）：被叫通道 leg 真正建立时产生，
//     DestChannel 即目标分机。仅覆盖真正拨出的通道，离线分机没有此事件。
//
//  2. VarSet 直呼信号（FreePBX macro-dial-one 路径）：FreePBX 直呼分机时先
//     通过 PJSIP_DIAL_CONTACTS 检查 contact，离线 PJSIP 分机会直接置
//     DIALSTATUS=CHANUNAVAIL 而不执行 Dial；离线 IAX2 分机即使执行了
//     Dial(IAX2/xx)，对端 leg 也建不起来。此时依据同一 Linkedid 内先出现
//     VarSet(THISDIAL=技术/分机)、随后出现 VarSet(DIALSTATUS=CHANUNAVAIL)
//     来识别“呼叫了离线分机”。
//
//  3. VarSet 响铃组信号（FreePBX macro-dial 路径）：响铃组/寻线组经
//     dialparties.agi 构建拨号串，离线 PJSIP 成员被**静默剔除**（ds 中不
//     出现、不 Dial、无任何逐成员事件）；离线 IAX2 成员虽在 ds 中但通道
//     建不起来，整次 Dial 以 CHANUNAVAIL 结束。macro-dial 从不设置
//     THISDIAL，需专用信号链：
//
//     Newexten(AppData: dial,<超时>,<选项>,<全量成员列表>)  → 记录候选成员
//     Newexten(AppData: ds=<过滤后拨号串>)                  → 不在 ds中的成员
//     即被剔除的离线成员，立即推送
//     VarSet(DIALSTATUS, Context: macro-dial) / Hangup      → 在 ds 中但未建起
//     DialBegin leg 的成员
//     （如离线 IAX2），补推
//
//     候选成员只有分机号没有通道技术，推送前按配置的技术逐一判活。
//     需要 AMI 账号具备 read=call,dialplan 权限。
//
// 设计参考 RFC 8599（Push Notification with the Session Initiation Protocol）
// 的目标：主叫呼叫时，若被叫 UA 未注册（无法振铃），及时向被叫推送通知。
// 本服务是 Asterisk 之外的旁路实现，不改动呼叫路由。
package monitor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"sip-push/internal/ami"
)

// PresenceQuerier 按通道技术查询分机在线状态（*ami.Client 实现）
type PresenceQuerier interface {
	CheckOnline(ctx context.Context, tech, ext string) (*ami.PresenceStatus, error)
}

// Pusher 推送能力（*bark.Client 实现）
type Pusher interface {
	Push(ctx context.Context, title, body string) error
}

// Config 监控配置
type Config struct {
	// Technologies 要监控的通道技术，如 ["PJSIP", "IAX2"]；为空时默认 ["PJSIP"]
	Technologies []string
	ExtPattern   string
	DedupWindow  time.Duration
}

// Logger 最小日志接口（*log.Logger 天然满足）
type Logger interface {
	Printf(format string, v ...interface{})
}

// dialCandidate 是一通呼叫中出现过的待拨目标。
// tech 为空表示来自响铃组成员列表（macro-dial），推送前需按技术逐一判活；
// tech 非空表示来自 THISDIAL（直呼路径），技术已明确。
type dialCandidate struct {
	tech   string
	ext    string
	caller string
	at     time.Time
}

// Monitor 来电未注册推送监控器
type Monitor struct {
	ami    PresenceQuerier
	push   Pusher
	cfg    Config
	techs  []string // 归一化后的技术名（大写）
	extRe  *regexp.Regexp
	logger Logger
	now    func() time.Time

	mu      sync.Mutex
	recent  map[string]time.Time       // 去重键 -> 首次命中时间
	pending map[string][]dialCandidate // Linkedid -> 该通呼叫出现过的待拨目标
	legs    map[string]map[string]bool // Linkedid -> 已建起 DialBegin leg 的分机号集合
}

// New 创建监控器。AMI 查询器可稍后通过 BindAMI 注入（方便 main 装配）。
func New(q PresenceQuerier, p Pusher, cfg Config, logger Logger) (*Monitor, error) {
	re, err := regexp.Compile(cfg.ExtPattern)
	if err != nil {
		return nil, fmt.Errorf("分机号正则非法: %w", err)
	}
	techs := make([]string, 0, len(cfg.Technologies))
	for _, t := range cfg.Technologies {
		t = strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(t, "/")))
		if t != "" {
			techs = append(techs, t)
		}
	}
	if len(techs) == 0 {
		techs = []string{"PJSIP"}
	}
	return &Monitor{
		ami:     q,
		push:    p,
		cfg:     cfg,
		techs:   techs,
		extRe:   re,
		logger:  logger,
		now:     time.Now,
		recent:  make(map[string]time.Time),
		pending: make(map[string][]dialCandidate),
		legs:    make(map[string]map[string]bool),
	}, nil
}

// BindAMI 注入 AMI 查询器
func (m *Monitor) BindAMI(q PresenceQuerier) {
	m.ami = q
}

// OnEvent 实现 ami.EventHandler。
// 处理 DialBegin（标准信号）、VarSet（直呼/响铃组信号链）、Newexten（响铃组
// 成员与拨号串）、Hangup（兜底评估与状态清理）。
// 注意：该方法在 AMI 读取协程上被调用，所有阻塞工作都转交独立协程。
func (m *Monitor) OnEvent(event string, f ami.Frame) {
	switch event {
	case "DialBegin":
		m.onDialBegin(f)
	case "VarSet":
		m.onVarSet(f)
	case "Newexten":
		m.onNewExten(f)
	case "Hangup":
		// 兜底评估：macro-dial 在“无可拨成员”路径（dialparties 全量剔除后
		// MacroExit）不产生 DIALSTATUS VarSet，只剩 Hangup 可触发。
		// 兜底与常规触发共用去重窗口，重复推送不会发生。
		if linked := linkedID(f); linked != "" {
			m.evaluateGroup(linked, "Hangup")
			m.removeCallState(linked)
		}
	}
}

// onDialBegin 处理标准 DialBegin 信号（被叫通道 leg 已建立）
func (m *Monitor) onDialBegin(f ami.Frame) {
	// Asterisk 12+ 字段为 DestChannel（如 PJSIP/210、IAX2/220-3）；
	// 极旧版本为 Destination，做兼容。
	dest := f.Get("DestChannel")
	if dest == "" {
		dest = f.Get("Destination")
	}
	tech, ext := Target(dest, m.techs)
	if ext == "" || !m.extRe.MatchString(ext) {
		return
	}
	linked := linkedID(f)
	m.rememberLeg(linked, ext) // 响铃组路径：该成员的通道已建起，不是离线成员
	m.fire(linked, tech, ext, normalizeCaller(f.Get("CallerIDNum")), "DialBegin")
}

// onVarSet 处理 FreePBX 离线信号链：
//
//	VarSet(THISDIAL=PJSIP/210)  → 记录待拨目标
//	VarSet(DIALSTATUS=CHANUNAVAIL) → 目标不可达，触发判活与推送
//
// VarSet 数量很多，只关心这两个变量，开销可忽略。
func (m *Monitor) onVarSet(f ami.Frame) {
	linked := linkedID(f)
	if linked == "" {
		return
	}
	switch f.Get("Variable") {
	case "THISDIAL":
		tech, ext := Target(f.Get("Value"), m.techs)
		if ext == "" || !m.extRe.MatchString(ext) {
			return
		}
		caller := normalizeCaller(f.Get("CallerIDNum"))
		m.rememberPending(linked, dialCandidate{tech: tech, ext: ext, caller: caller, at: m.now()})
	case "DIALSTATUS":
		// FreePBX 对同一通呼叫会多次置该值，由 fire 的去重窗口保证只推一次。
		if f.Get("Context") == "macro-dial" {
			// 响铃组路径：macro-dial 内 Dial 应用结束时（无论最终
			// CHANUNAVAIL/NOANSWER/ANSWER），评估在 ds 中但未建起 leg 的成员。
			m.evaluateGroup(linked, "macro-dial DIALSTATUS")
			return
		}
		// 直呼路径：CHANUNAVAIL 表示目标设备不可用（未注册/无 contact）。
		if f.Get("Value") != "CHANUNAVAIL" {
			return
		}
		// 不清空 pending：轮询/多设备场景下各设备可能依次上报 CHANUNAVAIL，
		// 重复推送由 fire 的去重窗口兜底；状态在 Hangup 时统一清理。
		for _, c := range m.snapshotPending(linked) {
			if c.tech == "" { // 响铃组候选由 evaluateGroup 处理
				continue
			}
			m.fire(linked, c.tech, c.ext, c.caller, "CHANUNAVAIL")
		}
	}
}

// onNewExten 处理响铃组信号链（macro-dial）：
//
//  1. AppData 为 "dial,..." / "dial-confirm,..."（Macro 调用实参）时，
//     最后一个实参（dial-confirm 为倒数第二个）是 '-' 分隔的全量成员列表。
//  2. AppData 以 "ds=" 开头（Noop(ds= ${ds}) 调试帧）时，首个逗号字段是
//     dialparties.agi 过滤后的拨号目标——不在其中的成员已被静默剔除，
//     即离线成员，立即评估推送。
func (m *Monitor) onNewExten(f ami.Frame) {
	linked := linkedID(f)
	if linked == "" {
		return
	}
	appData := f.Get("AppData")
	if members := groupMembers(appData); members != nil {
		for _, ext := range members {
			if !m.extRe.MatchString(ext) {
				continue
			}
			m.rememberPending(linked, dialCandidate{ext: ext, caller: normalizeCaller(f.Get("CallerIDNum")), at: m.now()})
		}
		return
	}
	if f.Get("Context") == "macro-dial" {
		m.evaluateDialstring(linked, parseDialstringTargets(appData))
	}
}

// groupMembers 从 Macro(dial) / Macro(dial-confirm) 的 AppData 实参中解析
// 全量成员列表。实参格式：
//
//	dial,<超时>,<选项>,<成员>           -> 最后一个逗号字段
//	dial-confirm,<超时>,<选项>,<成员>,<组号> -> 倒数第二个逗号字段
//
// AppData 不是上述两种宏调用（如 dial-one、Dial 应用实参）时返回 nil。
func groupMembers(appData string) []string {
	isDial := strings.HasPrefix(appData, "dial,")
	isDialConfirm := strings.HasPrefix(appData, "dial-confirm,")
	if !isDial && !isDialConfirm {
		return nil
	}
	fields := strings.Split(appData, ",")
	idx := len(fields) - 1
	if isDialConfirm {
		idx = len(fields) - 2
	}
	if idx <= 0 || fields[idx] == "" {
		return nil
	}
	members := strings.Split(fields[idx], "-")
	out := make([]string, 0, len(members))
	for _, x := range members {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// parseDialstringTargets 从 Noop(ds= ...) 的 AppData 中解析过滤后的拨号目标。
// 格式："ds= IAX2/220,20,..." —— 'ds=' 之后、首个逗号之前是 '&'-分隔的目标通道。
// AppData 不以 'ds=' 开头时返回 nil。
func parseDialstringTargets(appData string) []string {
	if !strings.HasPrefix(appData, "ds=") {
		return nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(appData, "ds="))
	if rest == "" {
		return nil
	}
	if i := strings.IndexByte(rest, ','); i >= 0 {
		rest = rest[:i]
	}
	return strings.Split(rest, "&")
}

// evaluateDialstring 立即评估被 dialparties 剔除的成员：
// 候选成员不在过滤后拨号串中 = 不会被 Dial，直接触发判活推送并移出候选。
func (m *Monitor) evaluateDialstring(linked string, targets []string) {
	if len(targets) == 0 {
		return
	}
	inDS := make(map[string]bool, len(targets))
	for _, t := range targets {
		if _, ext := Target(t, m.techs); ext != "" {
			inDS[ext] = true
		}
	}
	m.mu.Lock()
	cs := m.pending[linked]
	kept := cs[:0]
	var reqs []dialCandidate
	for _, c := range cs {
		if inDS[c.ext] {
			kept = append(kept, c) // 仍在拨号串中，留待 evaluateGroup 按 leg 评估
			continue
		}
		reqs = append(reqs, c)
	}
	if len(kept) == 0 {
		delete(m.pending, linked)
	} else {
		m.pending[linked] = kept
	}
	m.mu.Unlock()

	for _, c := range reqs { // 锁外触发，fire 需再次加锁
		m.fireCandidates(linked, c, "dialparties 剔除")
	}
}

// evaluateGroup 评估响铃组中“在拨号串里但未建起通道 leg”的成员并移出候选。
// 触发点：macro-dial 内 DIALSTATUS VarSet 与 Hangup 兜底。
func (m *Monitor) evaluateGroup(linked, signal string) {
	m.mu.Lock()
	cs := m.pending[linked]
	legs := m.legs[linked]
	var reqs []dialCandidate
	for _, c := range cs {
		if legs[c.ext] {
			continue // 该成员通道已建起（在线振铃中），不算离线
		}
		reqs = append(reqs, c)
	}
	delete(m.pending, linked)
	m.mu.Unlock()

	for _, c := range reqs { // 锁外触发，fire 需再次加锁
		m.fireCandidates(linked, c, signal)
	}
}

// fireCandidates 触发单个候选的判活推送：技术未知时按配置逐一判活
func (m *Monitor) fireCandidates(linked string, c dialCandidate, signal string) {
	if c.tech != "" {
		m.fire(linked, c.tech, c.ext, c.caller, signal)
		return
	}
	for _, t := range m.techs {
		m.fire(linked, t, c.ext, c.caller, signal)
	}
}

// fire 去重命中后异步判活并推送；signal 仅用于日志区分来源
func (m *Monitor) fire(linked, tech, ext, caller, signal string) {
	if linked == "" {
		return
	}
	dedupKey := linked + "|" + tech + "/" + ext
	if !m.markRecent(dedupKey) {
		m.logger.Printf("同一通呼叫已推送过，忽略: %s/%s linked=%s", tech, ext, linked)
		return
	}
	m.logger.Printf("捕获离线呼叫信号(%s): %s/%s caller=%s linked=%s", signal, tech, ext, caller, linked)
	go m.checkAndPush(tech, ext, caller)
}

// linkedID 取整通呼叫标识；个别版本没有 LinkedID 时回退 UniqueID
func linkedID(f ami.Frame) string {
	if v := f.Get("LinkedID"); v != "" {
		return v
	}
	return f.Get("UniqueID")
}

// normalizeCaller 规整主叫号码显示
func normalizeCaller(caller string) string {
	caller = strings.TrimSpace(caller)
	if caller == "" || caller == "<unknown>" {
		return "未知号码"
	}
	return caller
}

// rememberPending 记录一通呼叫出现过的待拨目标（同一目标去重）
func (m *Monitor) rememberPending(linked string, c dialCandidate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.expireLocked(now)
	for _, x := range m.pending[linked] {
		if x.tech == c.tech && x.ext == c.ext {
			return
		}
	}
	m.pending[linked] = append(m.pending[linked], c)
}

// rememberLeg 记录一通呼叫中已建起 DialBegin leg 的分机号
func (m *Monitor) rememberLeg(linked, ext string) {
	if linked == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked(m.now())
	if m.legs[linked] == nil {
		m.legs[linked] = make(map[string]bool)
	}
	m.legs[linked][ext] = true
}

// expireLocked 清理超过去重窗口的过期呼叫状态（须持有 m.mu）
func (m *Monitor) expireLocked(now time.Time) {
	for k, cs := range m.pending {
		if len(cs) > 0 && now.Sub(cs[0].at) > m.cfg.DedupWindow {
			delete(m.pending, k)
		}
	}
	// legs 仅在 pending 存在时才会被 evaluateGroup 查询，
	// 无 pending 的残留 legs 直接清理，防泄漏
	for k := range m.legs {
		if _, ok := m.pending[k]; !ok {
			delete(m.legs, k)
		}
	}
}

// removeCallState 挂断时清理该通呼叫的全部中间状态
func (m *Monitor) removeCallState(linked string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, linked)
	delete(m.legs, linked)
}

// snapshotPending 返回一通呼叫全部待拨目标的副本（不在锁内触发推送）
func (m *Monitor) snapshotPending(linked string) []dialCandidate {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.pending[linked]
	out := make([]dialCandidate, len(cs))
	copy(out, cs)
	return out
}

// removePending 挂断时清理
func (m *Monitor) removePending(linked string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, linked)
}

// checkAndPush 查询在线状态并在离线时推送
func (m *Monitor) checkAndPush(tech, ext, caller string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := m.ami.CheckOnline(ctx, tech, ext)
	if err != nil {
		m.logger.Printf("查询 %s 分机 %s 在线状态失败: %v", tech, ext, err)
		return
	}
	if !st.Found {
		m.logger.Printf("%s 分机 %s 不存在，跳过推送", tech, ext)
		return
	}
	if st.Online {
		m.logger.Printf("分机 %s/%s 在线，无需推送", tech, ext)
		return
	}

	title := fmt.Sprintf("分机 %s 有未接来电", ext)
	body := fmt.Sprintf("%s 呼叫分机 %s，但该分机当前未注册", caller, ext)
	if err := m.push.Push(ctx, title, body); err != nil {
		m.logger.Printf("推送失败 %s/%s caller=%s: %v", tech, ext, caller, err)
		return
	}
	m.logger.Printf("已推送离线来电提醒: %s/%s caller=%s", tech, ext, caller)
}

// markRecent 去重：窗口内已存在的键返回 false；顺带清理过期条目。
func (m *Monitor) markRecent(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, t := range m.recent {
		if now.Sub(t) > m.cfg.DedupWindow {
			delete(m.recent, k)
		}
	}
	if _, ok := m.recent[key]; ok {
		return false
	}
	m.recent[key] = now
	return true
}

// Target 从 DialBegin 的 DestChannel（旧版本为 Destination）字段提取通道技术
// 与分机号。例如：
//
//	"PJSIP/210"               -> ("PJSIP", "210")
//	"pjsip/2001"              -> ("PJSIP", "2001")
//	"PJSIP/1001/sip:1001@x"   -> ("PJSIP", "1001")
//	"PJSIP/1001;@host"        -> ("PJSIP", "1001")
//	"PJSIP/210-00000001"      -> ("PJSIP", "210")  // 已建链通道名带会话标记
//	"IAX2/3001"               -> ("IAX2", "3001")
//	"IAX2/3001-42"            -> ("IAX2", "3001")  // 已建链通道名带 call 序号
//
// 技术不在 techs 白名单内（如 SIP/ 外线中继、Local/ 本地通道）或无法解析时返回空串。
func Target(destination string, techs []string) (tech, ext string) {
	slash := strings.IndexByte(destination, '/')
	if slash <= 0 {
		return "", ""
	}
	t := strings.ToUpper(destination[:slash])
	matched := false
	for _, x := range techs {
		if strings.EqualFold(x, t) {
			matched = true
			t = x // 用白名单里的规范写法
			break
		}
	}
	if !matched {
		return "", ""
	}
	rest := destination[slash+1:]
	if i := strings.IndexAny(rest, "/;"); i >= 0 {
		rest = rest[:i]
	}
	// 已建立的通道名带会话标记：PJSIP 形如 "210-00000001"（8 位十六进制），
	// IAX2 形如 "3001-42"（十进制 callno）。尾部 -<纯数字/十六进制> 一律剥掉。
	if i := strings.LastIndexByte(rest, '-'); i >= 0 && isChannelToken(rest[i+1:]) {
		rest = rest[:i]
	}
	return t, strings.TrimSpace(rest)
}

// isChannelToken 判断非空字符串是否为通道会话标记（纯数字或小写十六进制）
func isChannelToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
