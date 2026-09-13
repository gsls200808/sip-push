package notify

import "testing"

func TestExtFilterMatchAll(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{"留空表示全部", nil},
		{"空切片表示全部", []string{}},
		{"只有空白项表示全部", []string{"", "   "}},
		{"星号表示全部", []string{"*"}},
		{"all 表示全部", []string{"all"}},
		{"ALL 大小写不敏感", []string{"ALL"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := NewExtFilter(c.in)
			if !f.MatchAll() {
				t.Fatal("应判定为全部")
			}
			for _, ext := range []string{"210", "220", "9999"} {
				if !f.Match(ext) {
					t.Fatalf("%s 应命中", ext)
				}
			}
			if got := f.String(); got != "全部" {
				t.Fatalf("String() = %q", got)
			}
		})
	}
}

func TestExtFilterSingle(t *testing.T) {
	f := NewExtFilter([]string{"210"})
	if f.MatchAll() {
		t.Fatal("不应判定为全部")
	}
	if !f.Match("210") {
		t.Fatal("210 应命中")
	}
	if f.Match("220") || f.Match("21") {
		t.Fatal("未绑定的分机不应命中")
	}
	if got := f.String(); got != "210" {
		t.Fatalf("String() = %q", got)
	}
}

func TestExtFilterMultiple(t *testing.T) {
	f := NewExtFilter([]string{"220", "210"})
	if f.MatchAll() {
		t.Fatal("不应判定为全部")
	}
	for _, ext := range []string{"210", "220"} {
		if !f.Match(ext) {
			t.Fatalf("%s 应命中", ext)
		}
	}
	if f.Match("230") {
		t.Fatal("230 不应命中")
	}
	if got := f.String(); got != "210,220" {
		t.Fatalf("String() 应排序，got %q", got)
	}
}

func TestExtFilterTrimsAndExactMatch(t *testing.T) {
	f := NewExtFilter([]string{" 210 ", "220"})
	if !f.Match("210") {
		t.Fatal("应忽略配置项两侧空白")
	}
	// 前缀不应误命中
	if f.Match("2") || f.Match("2100") {
		t.Fatal("必须是精确匹配")
	}
}
