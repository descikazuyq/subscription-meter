package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“按月取消的终止时刻由取消请求时刻的 UTC 自然月决定”提供时区维度的
// 回归保障：用户看到的本地日期可能已经跨月甚至跨年，但终止时刻只认实际时刻的
// UTC 月份，不能让订阅多用或少用一个月。围绕已生效、无欠费、无待换套餐安排的
// 订阅，沿用 CancelSubscription、Status 与现有用量查询入口。

// TestCancelEndUsesUTCMonthNotLocalWallClock 锁定：取消请求时刻的本地钟面已经
// 跨月（甚至跨年）时，终止时刻仍按 UTC 自然月取下一月月初；同一当前时刻换用
// 其他时区或纯 UTC 表示，结果完全一致。
func TestCancelEndUsesUTCMonthNotLocalWallClock(t *testing.T) {
	s, clk := newTestService(utc(2026, 12, 15, 12, 0))
	mustPlan(t, s, planDef("p", 1000, 10, 5, 0))
	wantTerms := PlanTerms{PlanID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 5}
	for _, id := range []string{"plus8", "plus8-utc", "minus5", "minus5-utc"} {
		mustAccount(t, s, id)
		mustSubscribe(t, s, id, "p", utc(2026, 12, 1, 0, 0))
	}

	// 情形一：当前时刻本地钟面 2027-01-01 07:30（UTC+8），本地已跨入 2027 年
	// 1 月，实际仍是 UTC 2026-12-31 23:30。终止时刻应取 UTC 12 月的下一月
	// 月初：2027-01-01 00:00 UTC，而不是按本地 1 月再往后推一个月。
	plus8Now := time.Date(2027, time.January, 1, 7, 30, 0, 0, zonePlus8)
	plus8NowUTC := utc(2026, 12, 31, 23, 30)
	if !plus8Now.UTC().Equal(plus8NowUTC) {
		t.Fatalf("test setup: %v is not %v in UTC", plus8Now, plus8NowUTC)
	}
	if plus8Now.Year() != 2027 || plus8Now.Month() != time.January {
		t.Fatalf("test setup: wall clock should already show January 2027, got %v", plus8Now)
	}
	wantEndJan := utc(2027, 1, 1, 0, 0)

	clk.t = plus8Now
	r := mustCancel(t, s, "plus8")
	if !r.Cancelled || !r.Cancellation.EndAt.Equal(wantEndJan) {
		t.Fatalf("plus8 cancel = %+v, want created with end %v", r, wantEndJan)
	}
	// 同一当前时刻用纯 UTC 表示，另一份订阅的取消结果必须相同。
	clk.t = plus8NowUTC
	r = mustCancel(t, s, "plus8-utc")
	if !r.Cancelled || !r.Cancellation.EndAt.Equal(wantEndJan) {
		t.Fatalf("utc-representation cancel = %+v, want created with end %v", r, wantEndJan)
	}

	// 首次登记后查询：仍显示订阅有效，展示相同终止时刻与原套餐条件。
	st, err := s.Status("plus8")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(wantEndJan) {
		t.Fatalf("status while cancelling = %+v, want subscribed with end %v", st, wantEndJan)
	}
	if st.CurrentTerms != wantTerms || st.PendingChange != nil {
		t.Fatalf("terms/pending while cancelling: cur=%+v pending=%+v, want %+v",
			st.CurrentTerms, st.PendingChange, wantTerms)
	}

	// 情形二：当前时刻本地钟面 2026-12-31 19:30（UTC-5），本地还停在 2026 年
	// 12 月，实际已是 UTC 2027-01-01 00:30。终止时刻应取 UTC 1 月的下一月
	// 月初：2027-02-01 00:00 UTC，不能按本地 12 月少算一个月。
	minus5Now := time.Date(2026, time.December, 31, 19, 30, 0, 0, zoneMinus5)
	minus5NowUTC := utc(2027, 1, 1, 0, 30)
	if !minus5Now.UTC().Equal(minus5NowUTC) {
		t.Fatalf("test setup: %v is not %v in UTC", minus5Now, minus5NowUTC)
	}
	if minus5Now.Year() != 2026 || minus5Now.Month() != time.December {
		t.Fatalf("test setup: wall clock should still show December 2026, got %v", minus5Now)
	}
	wantEndFeb := utc(2027, 2, 1, 0, 0)

	clk.t = minus5Now
	r = mustCancel(t, s, "minus5")
	if !r.Cancelled || !r.Cancellation.EndAt.Equal(wantEndFeb) {
		t.Fatalf("minus5 cancel = %+v, want created with end %v", r, wantEndFeb)
	}
	// 同一当前时刻用纯 UTC 表示，结果相同。
	clk.t = minus5NowUTC
	r = mustCancel(t, s, "minus5-utc")
	if !r.Cancelled || !r.Cancellation.EndAt.Equal(wantEndFeb) {
		t.Fatalf("utc-representation cancel = %+v, want created with end %v", r, wantEndFeb)
	}
	st, err = s.Status("minus5")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(wantEndFeb) ||
		st.CurrentTerms != wantTerms {
		t.Fatalf("minus5 status while cancelling = %+v, want subscribed with end %v and terms %+v",
			st, wantEndFeb, wantTerms)
	}
}

// TestCancelEndBoundaryNanosecondStatusAndUsage 锁定终止瞬间的纳秒级边界：
// 终止前最后一纳秒查询仍有效、事件仍归十二月且可接收；恰到达终止瞬间，状态
// 查询直接显示无有效订阅、当前套餐为零值、不再展示终止安排（不依赖先上报
// 用量或生成账单），恰在该瞬间的新事件返回 ErrEventBeforeSubscription。
// 终止瞬间换任何时区表达，上述结果不变。
func TestCancelEndBoundaryNanosecondStatusAndUsage(t *testing.T) {
	s, clk := newTestService(utc(2026, 12, 15, 12, 0))
	mustPlan(t, s, planDef("p", 1000, 10, 5, 0))
	wantTerms := PlanTerms{PlanID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 5}
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "p", utc(2026, 12, 1, 0, 0))
	// quiet 从不上报用量、从不出账：状态翻转必须由状态查询直接体现。
	mustAccount(t, s, "quiet")
	mustSubscribe(t, s, "quiet", "p", utc(2026, 12, 1, 0, 0))

	r := mustCancel(t, s, "u")
	end := r.Cancellation.EndAt // 2027-01-01 00:00:00 UTC
	if !end.Equal(utc(2027, 1, 1, 0, 0)) {
		t.Fatalf("test setup: end = %v, want 2027-01-01 00:00 UTC", end)
	}
	if r2 := mustCancel(t, s, "quiet"); !r2.Cancellation.EndAt.Equal(end) {
		t.Fatalf("test setup: quiet end = %v, want %v", r2.Cancellation.EndAt, end)
	}

	// 终止前的十二月用量：先在 12 月 10 日累计 5。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mid", At: utc(2026, 12, 10, 0, 0), Quantity: 5}); err != nil {
		t.Fatal(err)
	}

	// 终止前最后一纳秒：2026-12-31 23:59:59.999999999 UTC。
	lastNs := end.Add(-time.Nanosecond)
	clk.t = lastNs
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.CurrentTerms != wantTerms ||
		st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(end) {
		t.Fatalf("status one ns before end = %+v, want subscribed with terms %+v and end %v",
			st, wantTerms, end)
	}
	// 同一纳秒换 -05:00 表达（本地 2026-12-31 18:59:59.999999999），
	// 订阅仍显示有效并展示终止安排。
	clk.t = lastNs.In(zoneMinus5)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(end) {
		t.Fatalf("status one ns before end in -05:00 = %+v, want subscribed with end %v", st, end)
	}
	clk.t = lastNs
	// 该瞬间发生的事件仍属于十二月，十二月尚未出账，可以接收。
	ev, err := s.RecordEvent(Event{AccountID: "u", EventID: "last-ns", At: lastNs, Quantity: 3})
	if err != nil || !ev.Accepted || ev.Period != dec(2026) {
		t.Fatalf("event one ns before end: %+v %v, want accepted in 2026-12", ev, err)
	}
	// 同一纳秒换 +08:00 表达（本地钟面已是 2027-01-01 07:59:59.999999999），
	// 仍归十二月。
	ev, err = s.RecordEvent(Event{
		AccountID: "u", EventID: "last-ns-zoned",
		At: lastNs.In(zonePlus8), Quantity: 1,
	})
	if err != nil || !ev.Accepted || ev.Period != dec(2026) {
		t.Fatalf("zoned event one ns before end: %+v %v, want accepted in 2026-12", ev, err)
	}

	// 恰到达终止瞬间：状态查询直接翻转——无有效订阅、当前套餐为零值、
	// 不再展示终止安排；quiet 从未上报用量或生成账单，结果相同。
	clk.t = end
	for _, id := range []string{"u", "quiet"} {
		st, err = s.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Subscribed || st.CurrentTerms != (PlanTerms{}) ||
			st.PendingChange != nil || st.ScheduledEnd != nil {
			t.Fatalf("status of %s at end instant = %+v, want unsubscribed zero terms no schedule", id, st)
		}
	}

	// 恰在终止瞬间发生的新事件（不晚于提交时刻）：ErrEventBeforeSubscription。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "at-end", At: end, Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event at end instant = %v, want ErrEventBeforeSubscription", err)
	}
	// 终止瞬间换 +08:00 表达（本地 2027-01-01 08:00），结果相同。
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "at-end-zoned",
		At: end.In(zonePlus8), Quantity: 1,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("zoned event at end instant = %v, want ErrEventBeforeSubscription", err)
	}
	// 被拒绝的事件不增加十二月累计，也不产生一月用量记录。
	if u, _ := s.MonthlyUsage("u", dec(2026)); u.Total != 9 {
		t.Fatalf("dec usage after rejected events = %d, want 9", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", jan(2027)); u.Total != 0 {
		t.Fatalf("jan usage after rejected events = %d, want 0", u.Total)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: dec(2026), Total: 9}) {
		t.Fatalf("monthly usage after rejected events = %+v, want only [2026-12 9]", st.MonthlyUsage)
	}

	// 把终止瞬间换成其他时区表达，状态查询结果不变。
	clk.t = end.In(zonePlus8)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.Subscribed || st.CurrentTerms != (PlanTerms{}) || st.ScheduledEnd != nil {
		t.Fatalf("status at end instant in +08:00 = %+v, want unsubscribed", st)
	}
	// 终止之后：此前接收的十二月用量仍可查询。
	clk.t = utc(2027, 1, 10, 0, 0)
	if u, _ := s.MonthlyUsage("u", dec(2026)); u.Total != 9 {
		t.Fatalf("dec usage after end = %d, want 9", u.Total)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: dec(2026), Total: 9}) {
		t.Fatalf("history usage after end = %+v, want only [2026-12 9]", st.MonthlyUsage)
	}
}
