package main

import (
	"testing"
	"time"
)

// 2026-10-20: 调度时钟必须锚北京时间，与宿主机时区无关。
func TestNextCheckinTimeUsesScheduleTimezone(t *testing.T) {
	setScheduleTimezone("Asia/Shanghai")
	defer setScheduleTimezone("Asia/Shanghai")

	cases := []struct {
		name    string
		nowUTC  string
		wantBJT string // 期望的下一次 tick，北京时间
	}{
		// 北京 08:00（UTC 00:00）→ 当天北京 10:00
		{"morning", "2026-10-20T00:00:00Z", "2026-10-20T10:00:00+08:00"},
		// 北京 20:00（UTC 12:00）→ 当天北京 21:00
		{"evening", "2026-10-20T12:00:00Z", "2026-10-20T21:00:00+08:00"},
		// 北京 22:30（UTC 14:30）→ 次日北京 10:00
		{"night", "2026-10-20T14:30:00Z", "2026-10-21T10:00:00+08:00"},
		// 北京 09:59（UTC 01:59）→ 当天北京 10:00
		{"just-before-window", "2026-10-20T01:59:00Z", "2026-10-20T10:00:00+08:00"},
	}
	bj := loadScheduleLocation("Asia/Shanghai")
	for _, tc := range cases {
		now, err := time.Parse(time.RFC3339, tc.nowUTC)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		got := nextCheckinTime(now).In(bj)
		want, _ := time.Parse(time.RFC3339, tc.wantBJT)
		if !got.Equal(want) {
			t.Fatalf("%s: nextCheckinTime(%s) = %s, want %s", tc.name, tc.nowUTC, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
}

// keepalive 与签到共用同一个调度时钟。
func TestShouldRunKeepaliveNowUsesScheduleTimezone(t *testing.T) {
	setScheduleTimezone("Asia/Shanghai")
	defer setScheduleTimezone("Asia/Shanghai")
	bj := loadScheduleLocation("Asia/Shanghai")
	// 北京 22:10 = UTC 14:10 → keepalive 窗口命中
	hit, _ := time.Parse(time.RFC3339, "2026-10-20T14:10:00Z")
	if !shouldRunKeepaliveNow(hit) {
		t.Fatalf("22:10 Beijing must be inside the keepalive window (schedule loc %s)", bj)
	}
	// 北京时间 10:10（UTC 02:10）→ 不在 keepalive 窗口
	miss, _ := time.Parse(time.RFC3339, "2026-10-20T02:10:00Z")
	if shouldRunKeepaliveNow(miss) {
		t.Fatal("10:10 Beijing must NOT be inside the keepalive window")
	}
}

func TestCheckinRateLimitedRes(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
		want bool
	}{
		{"9074 code", map[string]any{"result": "NOT_ELIGIBLE", "failure_code": "9074"}, true},
		{"10605 queued", map[string]any{"result": "NOT_ELIGIBLE", "failure_code": "10605"}, true},
		{"chinese message", map[string]any{"result": "NOT_ELIGIBLE", "message": "当前参与用户太多，请稍后再试"}, true},
		{"english message", map[string]any{"result": "NOT_ELIGIBLE", "message": "rate limit exceeded"}, true},
		{"out of stock is NOT rate limit", map[string]any{"result": "NOT_ELIGIBLE", "failure_code": "REDEMPTION_CODE_OUT_OF_STOCK"}, false},
		{"claimed", map[string]any{"success": true, "result": "CLAIMED"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := checkinRateLimitedRes(tc.res); got != tc.want {
			t.Fatalf("%s: checkinRateLimitedRes = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// schedule_timezone 支持配置覆盖，非法值退化为 UTC+8 而不是 UTC。
func TestSetScheduleTimezoneFallback(t *testing.T) {
	setScheduleTimezone("Not/AZone")
	loc := scheduleLocation()
	if _, off := time.Now().In(loc).Zone(); off != 8*3600 {
		t.Fatalf("invalid tz must fall back to UTC+8, got %s (offset %d)", loc, off)
	}
	setScheduleTimezone("Asia/Shanghai")
}

func TestJitterWindowAndRange(t *testing.T) {
	setCheckinJitterMinutes(15)
	defer setCheckinJitterMinutes(defaultCheckinJitterMinutes)
	w := checkinJitterWindow()
	if w != 15*time.Minute {
		t.Fatalf("window = %s, want 15m", w)
	}
	for i := 0; i < 200; i++ {
		d := jitterFor(w)
		if d < 0 || d >= w {
			t.Fatalf("jitter %s out of [0,%s)", d, w)
		}
	}
	// 0 = 关闭
	setCheckinJitterMinutes(0)
	if got := checkinJitterWindow(); got != 0 {
		t.Fatalf("window must be 0 when disabled, got %s", got)
	}
	if got := jitterFor(0); got != 0 {
		t.Fatalf("disabled window must yield zero jitter, got %s", got)
	}
	// 负数钳到 0
	setCheckinJitterMinutes(-5)
	if got := checkinJitterWindow(); got != 0 {
		t.Fatalf("negative minutes must clamp to 0, got %s", got)
	}
}

func TestJitterSpreadsAccounts(t *testing.T) {
	// 三个账号同一 tick：偏移不应全部相同（错开几分钟是这次改动的目的）。
	w := 15 * time.Minute
	seen := map[time.Duration]bool{}
	for i := 0; i < 3; i++ {
		seen[jitterFor(w).Truncate(time.Second)] = true
	}
	if len(seen) == 1 {
		t.Skip("random collision on a tiny sample; not a correctness failure")
	}
	if len(seen) < 2 {
		t.Fatalf("jitter failed to spread accounts: %v", seen)
	}
}
