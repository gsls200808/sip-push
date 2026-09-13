package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsAndValidate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := []byte("ami:\n  addr: \"1.2.3.4:5038\"\n  username: u\n  secret: s\nbark:\n  device_key: k\n")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Call.Technologies) != 1 || c.Call.Technologies[0] != "PJSIP" ||
		c.Call.ExtPattern != `^[0-9*#]{2,8}$` {
		t.Fatalf("默认值异常: %+v", c.Call)
	}
	if c.Bark.BaseURL != "https://api.day.app" {
		t.Fatalf("Bark 默认地址异常: %s", c.Bark.BaseURL)
	}
	if c.AMI.PingInterval.Std() != 30*time.Second || c.Call.DedupWindow.Std() != 5*time.Minute {
		t.Fatalf("时长默认值异常: ping=%v dedup=%v", c.AMI.PingInterval, c.Call.DedupWindow)
	}
}

func TestLoadRejectsMissingFields(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	// 缺 bark.device_key
	if err := os.WriteFile(p, []byte("ami:\n  addr: \"1.2.3.4:5038\"\n  username: u\n  secret: s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("缺少 device_key 应报错")
	}

	// 非法正则
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("ami:\n  addr: x\n  username: u\n  secret: s\ncall:\n  ext_pattern: \"[\"\nbark:\n  device_key: k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("非法正则应报错")
	}

	// 非法时长
	badDur := filepath.Join(dir, "d.yaml")
	if err := os.WriteFile(badDur, []byte("ami:\n  addr: x\n  username: u\n  secret: s\n  ping_interval: \"abc\"\nbark:\n  device_key: k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(badDur); err == nil {
		t.Fatal("非法时长应报错")
	}

	// 不支持的通道技术
	badTech := filepath.Join(dir, "tech.yaml")
	if err := os.WriteFile(badTech, []byte("ami:\n  addr: x\n  username: u\n  secret: s\ncall:\n  technologies: [\"DAHDI\"]\nbark:\n  device_key: k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(badTech); err == nil {
		t.Fatal("不支持的技术应报错")
	}
}

func TestLoadYakphoneChannel(t *testing.T) {
	dir := t.TempDir()

	// 仅配置 yakphone（bark 留空禁用）
	p := filepath.Join(dir, "yak.yaml")
	content := "ami:\n  addr: \"1.2.3.4:5038\"\n  username: u\n  secret: s\n" +
		"yakphone:\n  token: tok\n  domain: pbx.example.com\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("仅 yakphone 应可加载: %v", err)
	}
	if c.Yakphone.BaseURL != "https://push.yakteam.com" || c.Yakphone.PushTimeout.Std() != 8*time.Second {
		t.Fatalf("yakphone 默认值异常: %+v", c.Yakphone)
	}

	// 配了 token 但缺 domain 应报错
	bad := filepath.Join(dir, "nodomain.yaml")
	if err := os.WriteFile(bad, []byte("ami:\n  addr: x\n  username: u\n  secret: s\nyakphone:\n  token: tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("yakphone.token 缺 domain 应报错")
	}

	// bark 与 yakphone 同时配置
	both := filepath.Join(dir, "both.yaml")
	if err := os.WriteFile(both, []byte("ami:\n  addr: x\n  username: u\n  secret: s\nbark:\n  device_key: k\nyakphone:\n  token: tok\n  domain: d\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(both); err != nil {
		t.Fatalf("双渠道应可加载: %v", err)
	}
}

func TestLoadTechnologies(t *testing.T) {
	dir := t.TempDir()
	base := "ami:\n  addr: \"1.2.3.4:5038\"\n  username: u\n  secret: s\nbark:\n  device_key: k\n"

	// 显式配置多技术（含大小写/尾部斜杠容错）
	p := filepath.Join(dir, "multi.yaml")
	if err := os.WriteFile(p, []byte(base+"call:\n  technologies: [\"pjsip\", \"iax2/\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Call.Technologies) != 2 ||
		c.Call.Technologies[0] != "PJSIP" || c.Call.Technologies[1] != "IAX2" {
		t.Fatalf("technologies 归一化异常: %v", c.Call.Technologies)
	}

	// 旧配置 dest_prefix 兜底
	legacy := filepath.Join(dir, "legacy.yaml")
	if err := os.WriteFile(legacy, []byte(base+"call:\n  dest_prefix: \"IAX2/\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(legacy)
	if err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	if len(c2.Call.Technologies) != 1 || c2.Call.Technologies[0] != "IAX2" {
		t.Fatalf("旧 dest_prefix 兜底异常: %v", c2.Call.Technologies)
	}
}
