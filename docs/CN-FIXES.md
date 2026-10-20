# 本地改动说明（CN 生产实测，2026-10-20）

本仓库是 [mmqz/cpa-multi-plugins](https://github.com/mmqz/cpa-multi-plugins) 的镜像 + 我们自己的
qoder 插件修复。全部改动都只落在 `plugins/qoder/`，上游其它插件（trae / workbuddy / mimo / zcode）未改动。

改动来源：CN 生产实例（Debian 13, CLIProxyAPI v8.0.23, 宿主时区 UTC）上的真实问题，每条都带实弹验证记录。

## 1. 图片输入：从"一律拒绝"改成默认放行

**问题**：v0.8.66（commit 0d50f05）为了解决 issue #37 加了无条件拒绝——任何带 `image_url` 的请求直接报
`qoder 通道不支持图像输入…`。

**实测**：拿 CN 账号跑插件自带的 `go test -tags live`（`qoder_live_image_test.go`），
**CN 网关完全能看见图**：

| 方言 / 场景 | 结果 |
|---|---|
| A 插件现状透传（messages 里 `[{text},{image_url}]`，data URI 内联） | 200，读出 `ZX-4417` |
| C 删掉 chat_context | 200，"Red" |
| D 官方模板 `contents` 字段 | 200，"Red" |
| E 纯图片（无 text part、chat_context.text 为空） | 200，描述图片 |
| 1.6MB / 4.18MB base64 截图 | 200，准确引用屏幕上的错误码 |
| 历史图 + 后一轮纯文字 | 200，正常 |
| 一次两张图 | 200，`ZX-4417, PLUM 92` 全对 |
| 流式 `stream=true` | 200，正常 |
| `qwen3.7-plus` / `deepseek-v4-pro` / 公网域名入口 / Anthropic `/v1/messages` image block | 全 200 |

**改动**：`body.go` 的守卫改成 `image_forward` 开关（默认 true）；纯图片（prompt 为空但有图）不再报
`no user message in request`，`business.name` 给 `[image]` 兜底。逻辑在新文件 `image_input.go`。

**配置**：`plugins.configs.qoder.image_forward: true|false`（Intl 若发现图被静默忽略，置 false 回到旧报错）。

**非图片附件（PDF/HTML/docx）**：上游 `agent_chat_generation` 的 content 只认 `text` 与 `image_url`，
`file` / `input_file` 块会被**服务端静默丢掉**（HTTP 200，模型答"没看到文件"）。实测：PDF/HTML 作为
file part 全部失效；同一内容作为 text part 完整可用。插件侧是原样透传的（构建出的 body 里
`file_data`、`filename` 都在），所以这块改插件救不回来——**文件必须由客户端抽成文本再发**。

## 2. 调度时区：固定北京时间（Asia/Shanghai）

**问题**：`checkinHours = [10,15,21]` 原本按宿主本地时区解释，而 VPS 的 `TZ=Etc/UTC`，于是自动签到的
实际时刻是**北京时间 18:00 / 23:00 / 次日 05:00**。用户上午看面板永远是"未签到"，看着像没执行
（现场证据：`qoder_campaign_rounds.json` 的自动 tick 时间戳 `2026-10-20T10:00:00.806Z` = 北京 18:00）。

**改动**（新文件 `schedule.go`）：调度时钟与宿主时区解耦，默认 `Asia/Shanghai`（tzdata 缺失时退化为固定
`+08:00`，绝不退回 UTC）。`nextCheckinTime` 与 `shouldRunKeepaliveNow` 都改用该时钟。

**配置**：`plugins.configs.qoder.schedule_timezone`（默认 Asia/Shanghai）。

**验证**：重启后日志 `qoder: scheduler armed — next tick 2026-10-21 10:00 CST (tz=Asia/Shanghai, UTC now 2026-10-20 15:32)`。

## 3. 9074 限流：后台退避重试

**问题**：上游 claim 会返回 `当前参与用户太多，请稍后再试`（`biz_code 9074`，日志里反复出现）。
trae 插件遇到同样情况会退避重试 10 次，qoder 之前**一次失败就放弃当天**，于是"签到了但没领到"。

**改动**（`schedule.go` + `checkin.go`）：调度路径检测到 `9074` / `10605` / "参与用户太多" /
"rate limit" 后，起一个后台 goroutine 按 `2m → 5m → 10m → 20m → 30m` 退避重试，拿到任何非限流结论即停；
重试复用 per-account 互斥锁（`checkinLockFor`），不会和面板手动签到打架，也不阻塞调度 tick。

## 4. 逐账号签到抖动

**问题**：三个账号同一秒打同一个 claim 接口，在上游看就是"同设备多号齐发"，最容易命中 9074。

**改动**（`checkin.go`）：每个账号在窗口内取随机偏移再签，默认窗口 **15 分钟**
（`checkin_jitter_minutes`，`0` 关闭）。抖动发生在拿并发槽之前，等待中的账号不占并发名额。
日志形如 `qoder checkin: <auth> scheduled with jitter 7m12s (target 10:07:12)`。

## 5. 可观测性

qoder 插件原本签到全程**不打一行日志**，所以"到底跑没跑"只能靠猜。现在补了：

- `qoder: scheduler armed — next tick …`（进程启动时一次，含时区与下次 tick）
- `qoder checkin: scheduled tick start — N account(s), tz=…, now=…`
- 每个账号的 tick 结论（`already checked in today` / `rate-limited …` / `tick verdict: …`）
- 限流重试的每次尝试与最终放弃

## 构建与部署

```bash
cd plugins/qoder
CGO_ENABLED=1 go build -buildmode=c-shared -o qoder.so .   # Go 1.26.x + gcc
# 导出符号与上游 release 产物逐行一致（69 个），可直接替换
install -m 644 qoder.so /opt/cliproxyapi/plugins/qoder.so
systemctl restart cliproxyapi
```

回滚：`cp /opt/cliproxyapi/plugins/qoder.so.bak-<时间戳> /opt/cliproxyapi/plugins/qoder.so && systemctl restart cliproxyapi`

## 测试

```bash
go test -count=1 .                      # 全绿（含本次新增的 schedule_test.go / body_vision_test.go）
go test -tags live -run TestLiveImageDialects -v .   # 需要 QD_TOKEN / QD_UID / QD_REGION
```

新增/改写的测试：`schedule_test.go`（时区、keepalive 时钟、限流识别、抖动窗口）、
`body_vision_test.go`（默认放行 / 关掉才拒绝 / 历史图不追溯 / 裸 reconfigure 复位）、
`status_passthrough_test.go`（10:00 开窗时刻改按调度时区断言）。
