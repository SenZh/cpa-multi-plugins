// schedule.go — 调度时区 + 签到限流重试（2026-10-20 实测修复）
//
// 问题一：时区漂移。checkinHours=[10,15,21] 原本按宿主本地时区解释，而
// saltlake VPS 的 TZ=Etc/UTC，于是自动签到的实际时刻是北京时间
// 18:00 / 23:00 / 次日 05:00——用户上午看面板永远"未签到"，看着像没执行
// （2026-10-20 现场：campaign_rounds.json 的自动 tick 时间戳正好
// 2026-10-20T10:00:00.806Z = 北京 18:00）。Qoder 的每日轮次按北京 10:00
// 刷新（campaign.go dailyWindowJustOpened 已经是 UTC+8 口径），调度必须锚同
// 一个时区，否则窗口滞后判定也一起错位。
//
// 修法：调度时区与宿主时区解耦，默认 Asia/Shanghai（tzdata 缺失时退化为
// 固定 +08:00），可用 plugins.configs.qoder.schedule_timezone 覆盖。
//
// 问题二：9074 限流。上游 claim 会返回 "当前参与用户太多，请稍后再试"
// （biz_code 9074，2026-10-20 日志里反复出现）。trae 插件遇到同样情况会
// 退避重试 10 次，qoder 之前一次失败就放弃当天，于是"签到了但没领到"。
// 这里给调度路径补一条后台退避重试：不阻塞 tick，复用 per-account 互斥锁
// 避免与面板手动签到打架。
package main

import (
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// defaultScheduleTimezone 是调度默认时区。CN 号池的全部时间语义（每日轮次
// 10:00 刷新、签到窗口）都是北京时间。
const defaultScheduleTimezone = "Asia/Shanghai"

var (
	scheduleLocMu sync.RWMutex
	scheduleLoc   = loadScheduleLocation(defaultScheduleTimezone)
)

// loadScheduleLocation 解析时区名；失败时退化为固定 +08:00（容器里没装
// tzdata 也能跑，绝不因为时区表缺失退回到 UTC）。
func loadScheduleLocation(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name != "" {
		if l, err := time.LoadLocation(name); err == nil && l != nil {
			return l
		}
		log.Printf("qoder: schedule_timezone %q unreadable, falling back to UTC+08:00", name)
	}
	return time.FixedZone("UTC+8", 8*3600)
}

func scheduleLocation() *time.Location {
	scheduleLocMu.RLock()
	defer scheduleLocMu.RUnlock()
	return scheduleLoc
}

// scheduleNow 是调度语义下的"现在"。
func scheduleNow() time.Time { return time.Now().In(scheduleLocation()) }

// setScheduleTimezone 由 configure() 调用；只有显式配置才移动指针。
func setScheduleTimezone(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	next := loadScheduleLocation(name)
	scheduleLocMu.Lock()
	changed := scheduleLoc.String() != next.String()
	scheduleLoc = next
	scheduleLocMu.Unlock()
	if changed {
		log.Printf("qoder: schedule_timezone=%s applied (auto check-in/keepalive clock)", next)
	}
}

// claimRateLimitedCodes 是上游"稍后再试"类限流码。9074 是 CN 领取接口的
// 名额/风控限流，10605 是排队中（claim 提交成功但未即时落账）。
var claimRateLimitedCodes = map[string]bool{"9074": true, "10605": true}

// checkinRateLimitedRes 判断一次 claim 结果是否属于可重试的限流。
func checkinRateLimitedRes(res map[string]any) bool {
	if res == nil {
		return false
	}
	if fc, ok := res["failure_code"].(string); ok && claimRateLimitedCodes[strings.TrimSpace(fc)] {
		return true
	}
	for _, k := range []string{"message", "msg"} {
		s, _ := res[k].(string)
		if s == "" {
			continue
		}
		low := strings.ToLower(s)
		if strings.Contains(s, "参与用户太多") || strings.Contains(s, "稍后再试") ||
			strings.Contains(low, "rate limit") || strings.Contains(low, "too many") {
			return true
		}
	}
	return false
}

// rateLimitBackoff 是后台重试的退避序列（累计约 67 分钟，5 次）。上游名额是
// 先到先得，白天重试命中率明显高于一次定论。
var rateLimitBackoff = []time.Duration{
	2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute,
}

// retryRateLimitedCheckinAsync 在后台按退避重试一次被限流的签到。任何一次
// 拿到非限流结论即停止（成功、已签、无权益都算结论）。
func retryRateLimitedCheckinAsync(authIndex, authID, who string, sa *storedAuth) {
	if sa == nil {
		return
	}
	go func() {
		for i, d := range rateLimitBackoff {
			time.Sleep(d)
			// 与面板手动签到共用同一把 per-account 锁，避免两次 claim 打架。
			mu := checkinLockFor(authIndex)
			mu.Lock()
			res, err := performCheckinCallFn(sa)
			mu.Unlock()
			switch {
			case err != nil:
				log.Printf("qoder checkin: %s rate-limited retry %d/%d transport error: %v", who, i+1, len(rateLimitBackoff), err)
			case checkinRateLimitedRes(res):
				log.Printf("qoder checkin: %s still rate-limited (retry %d/%d): %s", who, i+1, len(rateLimitBackoff), summarizeCheckinClaimRes(res))
			default:
				log.Printf("qoder checkin: %s rate-limit cleared on retry %d: %s", who, i+1, summarizeCheckinClaimRes(res))
				rememberCheckinMoment(authID)
				return
			}
		}
		log.Printf("qoder checkin: %s gave up after %d rate-limited retries", who, len(rateLimitBackoff))
	}()
}

// ---- 签到抖动 ----
//
// 三个账号同一秒打同一个 claim 接口，上游看到的是"同设备多号齐发"，命中
// 9074（当前参与用户太多）的概率明显更高。workbuddy 早就在 tick 上加了
// 逐账号 jitter，qoder 之前没有。这里给每个账号在窗口内取一个随机偏移，
// 错开几分钟再签。
const defaultCheckinJitterMinutes = 15

var (
	checkinJitterMu      sync.RWMutex
	checkinJitterMinutes = defaultCheckinJitterMinutes
)

// checkinJitterWindow 返回抖动窗口；<=0 表示不抖动。
func checkinJitterWindow() time.Duration {
	checkinJitterMu.RLock()
	defer checkinJitterMu.RUnlock()
	if checkinJitterMinutes <= 0 {
		return 0
	}
	return time.Duration(checkinJitterMinutes) * time.Minute
}

func setCheckinJitterMinutes(m int) {
	if m < 0 {
		m = 0
	}
	checkinJitterMu.Lock()
	changed := checkinJitterMinutes != m
	checkinJitterMinutes = m
	checkinJitterMu.Unlock()
	if changed {
		log.Printf("qoder: checkin_jitter_minutes=%d applied (per-account spread)", m)
	}
}

// jitterFor 在窗口内取一个随机偏移（含 0，不含窗口上界）。
func jitterFor(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(window)))
}
