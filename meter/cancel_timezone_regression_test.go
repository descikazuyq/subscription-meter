package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“按月取消的终止时刻由取消请求时刻的 UTC 自然月决定”提供时区维度的
// 回归保障：用户看到的本地日期可能已经跨月甚至跨年，但终止时刻、状态展示与
// 用量归属只认 UTC 实际时刻，不能按本地显示月份让订阅多用或少用一个月。
// 场景均为已经生效、没有欠费、没有待换套餐安排的订阅，沿用 CancelSubscription、
// Status 与现有用量查询入口，不改变取消功能及公开返回结果。

// TestCancelEndDeterminedByUTCMonthAcrossTimezones 锁定终止时刻的取月规则：
// 本地钟面已跨入 2027 年 1 月（UTC+8）而 UTC 仍在 2026 年 12 月时，取消从
// 2027-01-01 00:00 UTC 终止；本地钟面仍在 2026 年 12 月（UTC-5）而 UTC 已入
// 2027 年 1 月时，取消从 2027-02-01 00:00 UTC 终止。同一当前时刻换用 UTC
// 表示，结果必须完全一致。
func TestCancelEndDeterminedByUTCMonthAcrossTimezones(t *testing.T) {
	// 当前时刻（UTC+8 本地 2027-01-01 07:30）实际仍是 UTC 2026-12-31 23:30。
	localPlus8 := time.Date(2027, time.January, 1, 7, 30, 0, 0, zonePlus8)
	s, clk := newTestService(localPlus8)
	mustPlan(t, s, planDef("a", 1000, 10, 5, 0))
	mustAccount(t, s, "u")
	mustAccount(t, s, "u-utc")
	mustSubscribe(t, s, "u", "a", utc(2026, 12, 1, 0, 0))
	mustSubscribe(t, s, "u-utc", "a", utc(2026, 12, 1, 0, 0))

	// 锁定测试前提：本地钟面已是 2027 年 1 月 1 日，UTC 仍是 2026 年 12 月 31 日，
	// 任何按本地显示月份取“下一月”的实现都会把终止时刻错推到 2027-02-01。
	if localPlus8.Month() != time.January || localPlus8.Year() != 2027 {
		t.Fatalf("local wall = %v, want January 2027", localPlus8)
	}
	if nowUTC := localPlus8.UTC(); nowUTC.Month() != time.December || nowUTC.Year() != 2026 {
		t.Fatalf("utc instant = %v, want December 2026", nowUTC)
	}

	// 首次登记取消：本次新建安排，终止时刻为 UTC 下一自然月月初 2027-01-01 00:00。
	wantEnd := utc(2027, 1, 1, 0, 0)
	r := mustCancel(t, s, "u")
	if !r.Cancelled || !r.Cancellation.EndAt.Equal(wantEnd) {
		t.Fatalf("cancel result = %+v, want created with end %v", r, wantEnd)
	}

	// 查询仍显示订阅有效，展示相同终止时刻与原套餐条件，无待换套餐安排。
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("status while cancelling: %+v", st)
	}
	if st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("scheduled end = %v, want %v", st.ScheduledEnd, wantEnd)
	}
	wantTerms := PlanTerms{PlanID: "a", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 0}
	if st.CurrentTerms != wantTerms || st.PendingChange != nil {
		t.Fatalf("terms/pending while cancelling: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 同一当前时刻改用 UTC 表示（2026-12-31 23:30 UTC）：另一份有效订阅的
	// 取消结果必须完全相同，不能因时钟的时区表达不同而多给或少给一个月。
	clk.t = localPlus8.UTC()
	rUTC := mustCancel(t, s, "u-utc")
	if !rUTC.Cancelled || !rUTC.Cancellation.EndAt.Equal(wantEnd) {
		t.Fatalf("utc-clock cancel = %+v, want created with end %v", rUTC, wantEnd)
	}

	// 另一组：本地钟面 2026-12-31 19:30（UTC-5）实际已是 UTC 2027-01-01 00:30，
	// 取消应从再下一个 UTC 自然月 2027-02-01 00:00 终止——不能按本地显示的
	// 12 月把终止时刻错定在 2027-01-01。
	localMinus5 := time.Date(2026, time.December, 31, 19, 30, 0, 0, zoneMinus5)
	if localMinus5.Month() != time.December {
		t.Fatalf("local wall = %v, want December 2026", localMinus5)
	}
	if nowUTC := localMinus5.UTC(); nowUTC.Month() != time.January || nowUTC.Year() != 2027 {
		t.Fatalf("utc instant = %v, want January 2027", nowUTC)
	}
	clk.t = localMinus5
	mustAccount(t, s, "v")
	mustAccount(t, s, "v-utc")
	mustSubscribe(t, s, "v", "a", utc(2026, 12, 1, 0, 0))
	mustSubscribe(t, s, "v-utc", "a", utc(2026, 12, 1, 0, 0))

	wantEndFeb := utc(2027, 2, 1, 0, 0)
	rv := mustCancel(t, s, "v")
	if !rv.Cancelled || !rv.Cancellation.EndAt.Equal(wantEndFeb) {
		t.Fatalf("minus5 cancel = %+v, want created with end %v", rv, wantEndFeb)
	}
	// 同一时刻换 UTC 表示，结果一致。
	clk.t = localMinus5.UTC()
	rvUTC := mustCancel(t, s, "v-utc")
	if !rvUTC.Cancelled || !rvUTC.Cancellation.EndAt.Equal(wantEndFeb) {
		t.Fatalf("utc-clock cancel = %+v, want created with end %v", rvUTC, wantEndFeb)
	}
	st, err = s.Status("v")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(wantEndFeb) {
		t.Fatalf("v status while cancelling: %+v", st)
	}
}

// TestCancelBoundaryStatusAndUsageAtNanosecond 锁定 2027-01-01 00:00 UTC 终止
// 边界的纳秒精度：终止前最后一纳秒状态仍显示有效订阅、原套餐条件与终止安排，
// 事件仍属十二月且可接收；恰好到达终止瞬间，Status 直接显示无有效订阅、
// 当前套餐为零值、不再展示终止安排（不依赖先上报用量或生成账单），恰在终止
// 瞬间的新事件返回 ErrEventBeforeSubscription。终止瞬间换时区表达结果不变。
func TestCancelBoundaryStatusAndUsageAtNanosecond(t *testing.T) {
	s, clk := newTestService(utc(2026, 12, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 0))
	mustAccount(t, s, "u")
	// quiet 账户从登记取消到跨过终止瞬间不做任何操作，用于锁定状态变化由
	// Status 查询直接体现，不依赖先上报用量或生成账单。
	mustAccount(t, s, "quiet")
	mustSubscribe(t, s, "u", "a", utc(2026, 12, 1, 0, 0))
	mustSubscribe(t, s, "quiet", "a", utc(2026, 12, 1, 0, 0))

	end := utc(2027, 1, 1, 0, 0)
	r := mustCancel(t, s, "u")
	if !r.Cancelled || !r.Cancellation.EndAt.Equal(end) {
		t.Fatalf("cancel = %+v, want end %v", r, end)
	}
	rq := mustCancel(t, s, "quiet")
	if !rq.Cancelled || !rq.Cancellation.EndAt.Equal(end) {
		t.Fatalf("quiet cancel = %+v, want end %v", rq, end)
	}

	// 十二月中旬先接收一笔用量，作为终止后仍可查询的历史。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mid", At: utc(2026, 12, 15, 0, 0), Quantity: 4}); err != nil {
		t.Fatalf("mid-december event: %v", err)
	}

	// 终止前最后一纳秒：2026-12-31 23:59:59.999999999 UTC。
	lastNS := end.Add(-time.Nanosecond)
	clk.t = lastNS
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	wantTerms := PlanTerms{PlanID: "a", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 0}
	if !st.Subscribed || st.CurrentTerms != wantTerms {
		t.Fatalf("status one ns before end: %+v", st)
	}
	if st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(end) || st.PendingChange != nil {
		t.Fatalf("cancellation not visible one ns before end: %+v", st)
	}
	// 该瞬间发生的事件仍属于十二月；十二月尚未出账，可以接收。
	rLast, err := s.RecordEvent(Event{AccountID: "u", EventID: "last-ns", At: lastNS, Quantity: 3})
	if err != nil || !rLast.Accepted || rLast.Period != dec(2026) {
		t.Fatalf("event one ns before end: %+v %v", rLast, err)
	}
	if u, _ := s.MonthlyUsage("u", dec(2026)); u.Total != 7 {
		t.Fatalf("december usage = %d, want 7", u.Total)
	}

	// 恰好到达终止瞬间：第一次调用就是 Status，不经过上报用量或生成账单，
	// quiet 账户必须直接显示无有效订阅、当前套餐为零值、不再展示终止安排。
	clk.t = end
	stQuiet, err := s.Status("quiet")
	if err != nil {
		t.Fatal(err)
	}
	if stQuiet.Subscribed || stQuiet.CurrentTerms != (PlanTerms{}) ||
		stQuiet.ScheduledEnd != nil || stQuiet.PendingChange != nil {
		t.Fatalf("quiet status at end: %+v", stQuiet)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.Subscribed || st.CurrentTerms != (PlanTerms{}) ||
		st.ScheduledEnd != nil || st.PendingChange != nil {
		t.Fatalf("status at end: %+v", st)
	}

	// 恰在终止瞬间发生的新事件（不晚于提交时刻）：返回 ErrEventBeforeSubscription，
	// 既不增加十二月累计，也不产生一月用量记录。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "at-end", At: end, Quantity: 5}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event at end instant = %v, want ErrEventBeforeSubscription", err)
	}
	if u, _ := s.MonthlyUsage("u", dec(2026)); u.Total != 7 {
		t.Fatalf("december usage after rejected event = %d, want 7", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", jan(2027)); u.Total != 0 {
		t.Fatalf("january usage appeared = %d, want 0", u.Total)
	}
	st, _ = s.Status("u")
	for _, mu := range st.MonthlyUsage {
		if mu.Period == jan(2027) {
			t.Fatalf("january 2027 usage record exists: %+v", st.MonthlyUsage)
		}
	}

	// 把终止瞬间换成其他时区表达（本地 2027-01-01 08:00 +08、
	// 本地 2026-12-31 19:00 -05，均为同一实际时刻），状态与事件判定不变。
	clk.t = time.Date(2027, time.January, 1, 8, 0, 0, 0, zonePlus8)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.Subscribed || st.CurrentTerms != (PlanTerms{}) || st.ScheduledEnd != nil {
		t.Fatalf("status at end expressed in +08: %+v", st)
	}
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "at-end-plus8",
		At:       time.Date(2027, time.January, 1, 8, 0, 0, 0, zonePlus8),
		Quantity: 5,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event at end instant in +08 = %v, want ErrEventBeforeSubscription", err)
	}
	clk.t = time.Date(2026, time.December, 31, 19, 0, 0, 0, zoneMinus5)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.Subscribed || st.CurrentTerms != (PlanTerms{}) || st.ScheduledEnd != nil {
		t.Fatalf("status at end expressed in -05: %+v", st)
	}
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "at-end-minus5",
		At:       time.Date(2026, time.December, 31, 19, 0, 0, 0, zoneMinus5),
		Quantity: 5,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event at end instant in -05 = %v, want ErrEventBeforeSubscription", err)
	}

	// 终止后：此前接收的十二月用量仍可查询，累计不变，且没有一月记录。
	if u, _ := s.MonthlyUsage("u", dec(2026)); u.Total != 7 {
		t.Fatalf("december usage after end = %d, want 7", u.Total)
	}
	st, _ = s.Status("u")
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: dec(2026), Total: 7}) {
		t.Fatalf("monthly usage after end = %+v, want [2026-12 7]", st.MonthlyUsage)
	}
}
