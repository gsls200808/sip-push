package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 支持在 yaml 里写 "5s" / "30s" / "5m" 这样的字符串
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	td, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("无效的时长 %q: %w", s, err)
	}
	*d = Duration(td)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config 全部配置
type Config struct {
	AMI      AMIConfig      `yaml:"ami"`
	Call     CallConfig     `yaml:"call"`
	Bark     BarkConfig     `yaml:"bark"`
	Yakphone YakphoneConfig `yaml:"yakphone"`
}

// AMIConfig Asterisk Manager Interface 连接配置
type AMIConfig struct {
	Addr              string   `yaml:"addr"`
	Username          string   `yaml:"username"`
	Secret            string   `yaml:"secret"`
	ReconnectInterval Duration `yaml:"reconnect_interval"`
	PingInterval      Duration `yaml:"ping_interval"`
	DialTimeout       Duration `yaml:"dial_timeout"`
	ActionTimeout     Duration `yaml:"action_timeout"`
}

// CallConfig 来电判定与去重配置
type CallConfig struct {
	// Technologies 要监控的通道技术，可选 "PJSIP"、"IAX2"，默认 ["PJSIP"]。
	// 对应 DialBegin 的 Destination 前缀（PJSIP/xxx、IAX2/xxx）。
	Technologies []string `yaml:"technologies"`
	// DestPrefix 旧版单技术配置（如 "PJSIP/"），仅在未配置 technologies 时兜底，
	// 保留用于兼容旧 config.yaml。
	DestPrefix string `yaml:"dest_prefix"`
	// ExtPattern 目标分机号正则，匹配才查询/推送；默认纯数字/*# 2~8 位
	ExtPattern string `yaml:"ext_pattern"`
	// DedupWindow 同一通呼叫（LinkedID+分机）在此时间内只推一次
	DedupWindow Duration `yaml:"dedup_window"`
}

// BarkConfig Bark 推送配置（可选渠道：device_key 为空时禁用）
type BarkConfig struct {
	BaseURL   string `yaml:"base_url"`
	DeviceKey string `yaml:"device_key"`
	// Group Bark 通知分组名，便于在 App 里归类
	Group string `yaml:"group"`
	// PushTimeout 单次推送超时
	PushTimeout Duration `yaml:"push_timeout"`
}

// YakphoneConfig yakphone 软电话 VoIP 来电推送配置
// （可选渠道：token 为空时禁用；与 bark 至少启用一个）。
// 文档：POST {base_url}/v1/notify
// {"token":"...","caller_uri":"sip:1000@pbx.example.com","caller_name":"Alice","type":"voip"}
type YakphoneConfig struct {
	BaseURL string `yaml:"base_url"`
	Token   string `yaml:"token"`
	// Domain 构造 caller_uri 的 SIP 域（PBX 的 SIP 域名或 IP），
	// yakphone App 以此匹配来电归属
	Domain string `yaml:"domain"`
	// PushTimeout 单次推送超时
	PushTimeout Duration `yaml:"push_timeout"`
}

// Load 读取并校验配置
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.AMI.ReconnectInterval == 0 {
		c.AMI.ReconnectInterval = Duration(5 * time.Second)
	}
	if c.AMI.PingInterval == 0 {
		c.AMI.PingInterval = Duration(30 * time.Second)
	}
	if c.AMI.DialTimeout == 0 {
		c.AMI.DialTimeout = Duration(5 * time.Second)
	}
	if c.AMI.ActionTimeout == 0 {
		c.AMI.ActionTimeout = Duration(5 * time.Second)
	}
	if len(c.Call.Technologies) == 0 {
		// 兼容旧配置：从 dest_prefix（如 "PJSIP/"）推导技术名
		if p := strings.TrimSpace(c.Call.DestPrefix); p != "" {
			c.Call.Technologies = []string{p}
		} else {
			c.Call.Technologies = []string{"PJSIP"}
		}
	}
	for i, t := range c.Call.Technologies {
		// 容错：去空白、去尾部 "/"、统一大写（"pjsip/" -> "PJSIP"）
		c.Call.Technologies[i] = strings.ToUpper(
			strings.TrimSuffix(strings.TrimSpace(t), "/"))
	}
	if c.Call.ExtPattern == "" {
		c.Call.ExtPattern = `^[0-9*#]{2,8}$`
	}
	if c.Call.DedupWindow == 0 {
		c.Call.DedupWindow = Duration(5 * time.Minute)
	}
	if c.Bark.BaseURL == "" {
		c.Bark.BaseURL = "https://api.day.app"
	}
	if c.Bark.Group == "" {
		c.Bark.Group = "sip-push"
	}
	if c.Bark.PushTimeout == 0 {
		c.Bark.PushTimeout = Duration(8 * time.Second)
	}
	if c.Yakphone.BaseURL == "" {
		c.Yakphone.BaseURL = "https://push.yakteam.com"
	}
	if c.Yakphone.PushTimeout == 0 {
		c.Yakphone.PushTimeout = Duration(8 * time.Second)
	}
}

// Validate 检查必填项与格式
func (c *Config) Validate() error {
	if c.AMI.Addr == "" {
		return fmt.Errorf("ami.addr 不能为空")
	}
	if c.AMI.Username == "" || c.AMI.Secret == "" {
		return fmt.Errorf("ami.username / ami.secret 不能为空")
	}
	if len(c.Call.Technologies) == 0 {
		return fmt.Errorf("call.technologies 至少配置一种通道技术")
	}
	for _, t := range c.Call.Technologies {
		switch t {
		case "PJSIP", "IAX2":
		default:
			return fmt.Errorf("call.technologies 包含不支持的技术 %q（仅支持 PJSIP/IAX2）", t)
		}
	}
	if _, err := regexp.Compile(c.Call.ExtPattern); err != nil {
		return fmt.Errorf("call.ext_pattern 不是合法正则: %w", err)
	}
	if c.Bark.DeviceKey == "" && c.Yakphone.Token == "" {
		return fmt.Errorf("至少配置一个推送渠道：bark.device_key 或 yakphone.token")
	}
	if c.Yakphone.Token != "" && c.Yakphone.Domain == "" {
		return fmt.Errorf("yakphone.domain 不能为空（用于构造 caller_uri 的 SIP 域）")
	}
	return nil
}
