package notify

import (
	"errors"
	"sort"
	"strings"
)

// ErrSkipped 表示该渠道未绑定此分机、主动跳过；它不是失败，
// monitor 据此区分"跳过"与"推送失败"两种日志。
var ErrSkipped = errors.New("渠道未绑定该分机，跳过推送")

// ExtFilter 分机号绑定过滤器。
// 支持三种配置形态：
//   - 全部：留空，或写 ["*"] / ["all"]
//   - 一个：["210"]
//   - 多个：["210", "220"]
type ExtFilter struct {
	matchAll bool
	set      map[string]struct{}
}

// NewExtFilter 由配置构造过滤器；空白项忽略，["*"]/["all"] 表示不限。
func NewExtFilter(extensions []string) ExtFilter {
	f := ExtFilter{set: make(map[string]struct{})}
	for _, raw := range extensions {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if e == "*" || strings.EqualFold(e, "all") {
			f.matchAll = true
			continue
		}
		f.set[e] = struct{}{}
	}
	// 未配置任何有效分机号 => 不限制（保持旧配置行为）
	if len(f.set) == 0 {
		f.matchAll = true
	}
	if f.matchAll {
		f.set = nil // 全部命中时无需保留明细
	}
	return f
}

// Match 判断分机号是否在绑定列表中
func (f ExtFilter) Match(ext string) bool {
	if f.matchAll {
		return true
	}
	_, ok := f.set[ext]
	return ok
}

// MatchAll 是否绑定全部
func (f ExtFilter) MatchAll() bool { return f.matchAll }

// String 便于日志展示：全部 / 210,220
func (f ExtFilter) String() string {
	if f.matchAll {
		return "全部"
	}
	list := make([]string, 0, len(f.set))
	for e := range f.set {
		list = append(list, e)
	}
	sort.Strings(list)
	return strings.Join(list, ",")
}
