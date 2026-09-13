# sip-push

基于 RFC 8599 思路的未注册分机来电推送服务：主叫呼叫 Asterisk 上的分机时，如果被叫分机当前**未注册/离线**（话机根本不会振铃），立即向被叫的 iPhone 通过 [Bark](https://apps.apple.com/app/bark-customed-pushNotifications/id1403753865) 推送一条"未接来电"提醒。

程序作为旁路服务运行，通过 AMI 只读地观察呼叫事件，**不改动任何呼叫路由**。

## 工作原理

```
主叫 200 ──拨 210──> Asterisk/FreePBX ──AMI 事件──> sip-push ──离线?──> Bark 推送到被叫手机
```

### 1. 呼叫信号（三类，互为补充）

| 信号 | 触发条件 | 适用场景 |
|---|---|---|
| `DialBegin` 事件 | 被叫通道 leg 真正建立 | 标准 Asterisk 拨号计划（仅覆盖真正拨出的通道） |
| `VarSet` 信号链 | 同一 LinkedID 内先出现 `VarSet(THISDIAL=<技术>/<分机>)`，随后出现 `VarSet(DIALSTATUS=CHANUNAVAIL)` | FreePBX 直呼分机（`macro-dial-one` 路径） |
| `VarSet` 响铃组信号链 | `Newexten` 的 `AppData: dial,...,<全量成员列表>` 记录候选 → `ds=` Noop 帧对照过滤后拨号串 → `Context: macro-dial` 的 `DIALSTATUS` VarSet / Hangup 兜底 | FreePBX 响铃组/寻线组（`macro-dial` 路径） |

FreePBX 兼容是本项目的一个关键点，两条 FreePBX 路径行为完全不同：

- **直呼分机**（`macro-dial-one`）：先查 contact，离线 PJSIP 分机直接置 `DIALSTATUS=CHANUNAVAIL` 而不执行 Dial；离线 IAX2 分机即使执行了 `Dial(IAX2/xx)`，对端 leg 也建立不起来。两种情况都不会产生 `DialBegin`，因此需要 VarSet 信号兜底。
- **响铃组/寻线组**（`macro-dial`）：由 `dialparties.agi` 构建拨号串，**离线 PJSIP 成员被静默剔除**（不出现在 `ds` 中、不 Dial、无任何逐成员事件）；离线 IAX2 成员虽在 `ds` 中但通道建不起来，整次 Dial 以 `CHANUNAVAIL` 结束。`macro-dial` 从不设置 `THISDIAL`，需要专用信号链：从 `Macro(dial)` 的 AppData 拿全量成员，与过滤后拨号串（`ds=` Noop 帧）对照，不在拨号串中的成员立即推送；仍在拨号串中但未建起 `DialBegin` leg 的成员（如离线 IAX2）在 Dial 结束时补推。候选成员只有分机号没有技术信息，推送前按配置的技术逐一判活。

同一通呼叫多次置 `CHANUNAVAIL`、多次评估由去重窗口保证只推一次；呼叫状态在 `Hangup` 时清理。

### 2. 在线判定

按通道技术路由到不同的 AMI 查询（字段/动作名均按实测 Asterisk 16.30 实现）：

| 技术 | AMI 动作 | 在线判据 |
|---|---|---|
| PJSIP | `PJSIPShowAors`（列表后按 `ObjectName` 过滤） | `TotalContacts > 0`（18+），或 `Contacts` 非空（16.x） |
| IAX2 | `IAXpeerlist`（列表后按 peer 名过滤） | `Status` 为 `OK`/`LAGGED`，或 `UNMONITORED` 且有真实 IP |

IAX2 判定语义说明：

- `OK` / `LAGGED`：开启了 `qualify` 且探测正常（LAGGED 只是延迟高，仍可通话）
- `UNMONITORED`：未开 qualify——静态 peer 恒有配置地址；动态 peer 注册后才有地址，注册过期后地址被清空。因此**有真实 IP 即视为在线**
- `UNREACHABLE` / `UNKNOWN` / 无真实 IP（`(null)`、`(Unspecified)` 等）：离线

### 3. 去重与推送

- 去重键：`LinkedID + 技术/分机`，同一通呼叫在 `dedup_window`（默认 5 分钟）内只推一次
- 多渠道扇出：Bark 与 yakphone 可同时启用，同一来电按各渠道的**分机绑定**决定是否推送；单个渠道失败不影响其他渠道
- 分机绑定：每个渠道可用 `extensions` 限定只推送哪些分机；未绑定的分机在日志中记录`渠道[xxx]未绑定分机 xxx，跳过`
- Bark 网络错误/5xx 自动重试一次，4xx 等确定性错误不重试；yakphone 同策略

### 4. 推送渠道

| 渠道 | 形式 | 配置启用条件 |
|---|---|---|
| Bark | 通知条（标题 + 正文） | `bark.device_key` 非空 |
| yakphone | VoIP 来电唤醒（唤醒 App 弹出来电界面） | `yakphone.token` 非空 |

#### 分机绑定（extensions）

两个渠道各自独立配置，支持三种形态：

| 需求 | 写法 |
|---|---|
| 全部（默认） | 留空 / 删除该行，或 `["*"]`，或 `["all"]` |
| 一个 | `["210"]` |
| 多个 | `["210", "220"]` |

判定是**精确匹配**（`210` 不会命中 `2100`），与分机号所属技术无关（`PJSIP/210` 与 `IAX2/210` 都会命中绑定 `210` 的渠道）。因此可以让 Bark 推全部、yakphone 只推 210/220，实现"值班手机只接特定分机的 VoIP 唤醒"。

Bark 推送内容：标题 `分机 210 有未接来电`，正文 `200 呼叫分机 210，但该分机当前未注册`。

yakphone 推送载荷（`POST {base_url}/v1/notify`）：

```json
{
  "token": "<yakphone.token>",
  "caller_uri": "sip:<主叫号码>@<yakphone.domain>",
  "caller_name": "<主叫显示名>",
  "type": "voip"
}
```

- `caller_uri`：主叫号码取 AMI 帧的 `CallerIDNum`，缺失时用 `anonymous`（与 SIP 匿名呼叫惯例一致）；`domain` 来自 `yakphone.domain` 配置（PBX 的 SIP 域名或 IP）
- `caller_name`：优先 AMI 帧的 `CallerIDName`，回退主叫号码，最终兜底"未知号码"

## 目录结构

```
cmd/sippush/        程序入口与装配
internal/ami/       AMI 客户端：帧解析、断线重连、动作收发、PJSIP/IAX2 判活
internal/monitor/   核心编排：DialBegin/VarSet 信号 → 判活 → 多渠道扇出（含去重）
internal/notify/    推送渠道公共接口、消息结构与分机绑定过滤器
internal/bark/      Bark 推送渠道（POST {base_url}/push，JSON 载荷）
internal/yakphone/  yakphone VoIP 来电唤醒渠道（POST {base_url}/v1/notify）
internal/config/    YAML 配置加载、默认值与校验
configs/            config.example.yaml 配置示例
```

## 构建

依赖 Go 1.21+，无 CGO 依赖，可任意平台交叉编译：

```bash
# 本机构建（Windows）
go build -o bin/sippush.exe ./cmd/sippush

# 交叉编译 Linux 服务器版
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o bin/sippush-linux-amd64 ./cmd/sippush

# 测试 + 静态检查
go vet ./... && go test ./...
```

## 配置

复制 `configs/config.example.yaml` 为 `config.yaml`，主要字段：

### ami —— AMI 连接

| 字段 | 默认 | 说明 |
|---|---|---|
| `addr` | 必填 | Asterisk AMI 地址，FreePBX 默认端口 5038 |
| `username` / `secret` | 必填 | AMI 账号（见下文 Asterisk 侧准备） |
| `reconnect_interval` | 5s | 断线重连间隔 |
| `ping_interval` | 30s | AMI 心跳（Ping 动作）间隔 |
| `dial_timeout` / `action_timeout` | 5s | TCP 连接 / 单个动作响应超时 |

### call —— 呼叫判定

| 字段 | 默认 | 说明 |
|---|---|---|
| `technologies` | `["PJSIP"]` | 要监控的通道技术，可选 `PJSIP`、`IAX2` |
| `ext_pattern` | `^[0-9*#]{2,8}$` | 目标号码匹配该正则才视为分机（过滤外线/中继号） |
| `dedup_window` | 5m | 同一通呼叫（LinkedID + 分机）的推送去重窗口 |
| `dest_prefix` | — | 旧版单技术字段，仅在未配置 `technologies` 时兜底，可删除 |

### 推送渠道（bark / yakphone 至少配置一个，可同时启用）

#### bark —— 通知条推送（可选）

| 字段 | 默认 | 说明 |
|---|---|---|
| `base_url` | `https://api.day.app` | 官方实例；自建 Bark 改成你的地址 |
| `device_key` | 空（禁用该渠道） | Bark App 首页复制的 URL 末尾那串 key |
| `group` | `sip-push` | App 内通知分组名 |
| `extensions` | 全部 | 本渠道绑定的分机号，见「分机绑定」 |
| `push_timeout` | 8s | 单次推送超时 |

#### yakphone —— VoIP 来电唤醒推送（可选）

| 字段 | 默认 | 说明 |
|---|---|---|
| `base_url` | `https://push.yakteam.com` | yakphone 推送服务地址 |
| `token` | 空（禁用该渠道） | yakphone 分配的设备令牌 |
| `domain` | 必填（启用时） | 构造 `caller_uri` 的 SIP 域（PBX 的 SIP 域名或 IP） |
| `extensions` | 全部 | 本渠道绑定的分机号，见「分机绑定」 |
| `push_timeout` | 8s | 单次推送超时 |

## Asterisk / FreePBX 侧准备

在 `/etc/asterisk/manager_custom.conf`（FreePBX）或 `manager.conf` 增加专用账号：

```ini
[sippush]
secret=CHANGE_ME
deny=0.0.0.0/0.0.0.0
permit=127.0.0.1/255.255.255.255   ; 程序与 Asterisk 同机时只放行本机
read=call,dialplan
write=system,reporting
writetimeout=1000
```

然后 `asterisk -rx "manager reload"`。权限用途：

- `read=call` —— 接收 `DialBegin`、`Hangup` 事件
- `read=dialplan` —— 接收 `VarSet` 事件（FreePBX 离线分机信号链，必需）
- `write=system,reporting` —— 执行 `PJSIPShowAors` 与 `IAXpeerlist`

IAX2 支持还要求 Asterisk 已加载 `chan_iax2`（`asterisk -rx "module show like iax2"` 确认），且分机号需与 `iax2 show peers` 中的 peer Name 一致。

## 部署

程序为单文件静态二进制，推荐 systemd 管理（`/etc/systemd/system/sippush.service`）：

```ini
[Unit]
Description=SIP Push - missed-call push for offline PJSIP/IAX2 extensions
After=network.target asterisk.service

[Service]
ExecStart=/opt/sippush/sippush -c /opt/sippush/config.yaml
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now sippush
systemctl is-active sippush
journalctl -u sippush -n 20
```

启动成功标志：日志出现 `AMI 已连接并登录成功: 127.0.0.1:5038（sippush）`。

### 更新流程

重新交叉编译后上传替换二进制，再 `systemctl restart sippush` 即可。注意去重窗口：同一通呼叫 5 分钟内（可配）只推一次。

## 真实拨打验证

1. 挑一个当前未注册的分机（`asterisk -rx "pjsip show contacts"` / `iax2 show peers` 里无地址/无 Avail 的）
2. 用一部在线话机拨打它，听到无法接通/忙音后挂断
3. 观察日志，正常应出现完整闭环：

```
捕获离线呼叫信号(CHANUNAVAIL): PJSIP/210 caller=200 linked=1789270656.101
同一通呼叫已推送过，忽略: PJSIP/210 linked=1789270656.101   ← FreePBX 二次置值，去重正常
已推送离线来电提醒: PJSIP/210 caller=200
```

4. iPhone 应收到 Bark 通知

响铃组验证：向响铃组号码发起呼叫（真实外线呼入或 `asterisk -rx "channel originate Local/<组号>@from-internal/n application Wait 30"`），组内每个离线分机应各收到一条推送；组内不存在的分机号（两种技术都查不到）会记日志"不存在，跳过推送"。

已验证环境：FreePBX（Sangoma Linux 7）/ Asterisk 16.30 / chan_iax2，直呼 PJSIP/IAX2 分机与响铃组（ringall）链路均实测通过。

## 已知限制与设计取舍

- **判活是"注册状态"而非"设备可达"**：推送发生在呼叫瞬间，若分机在呼叫后、推送前几秒内恰好注册，会被判在线而不推（概率极低，可接受）
- **IAX2 未开 qualify 的 peer** 按"有注册地址即在线"处理，这对手机软门类动态 peer 是准确的；如需更精确可在 iax.conf 开 `qualify`
- **通道技术白名单**过滤了 `Local/`、`SIP/` 等非监控通道，不会误推外线/中继呼叫
- **响铃组成员没有技术信息**，推送前会对每个配置的技术各查一次（如 PJSIP+IAX2 就是每成员两次查询）；成员在两种技术下都不存在时跳过推送
- **响铃组含 Follow-Me（FMFM）成员**时存在极小误推可能：成员本体制已离线但 FMFM 转接手机接听了，该成员仍会收到"未接来电"推送（判活只看本机注册状态）
- 扩展新的通道技术（如 chan_sip 的 `SIPpeers`）只需：`internal/ami` 加一个查询实现 + `presence.go` 加一个 case + `config.go` 白名单放行
