package meter

import (
	"errors"
	"testing"
	"time"
)

// assertNotSubscribed 断言状态显示没有已生效订阅：Subscribed 为 false，
// 当前套餐条件为零值，也没有待换套餐安排与待取消终止时刻。
func assertNotSubscribed(t *testing.T, st AccountStatus) {
	t.Helper()
	if st.Subscribed {
		t.Fatalf("Subscribed = true before activation instant, status = %+v", st)
	}
	if st.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("CurrentTerms = %+v before activation, want zero value", st.CurrentTerms)
	}
	if st.PendingChange != nil {
		t.Fatalf("PendingChange = %+v before activation, want nil", st.PendingChange)
	}
	if st.ScheduledEnd != nil {
		t.Fatalf("ScheduledEnd = %v before activation, want nil", *st.ScheduledEnd)
	}
}

// TestStatusFutureSubscriptionWaitsForActivationInstant 验证提前登记的订阅
// 在实际开通时刻到达前查询显示未生效，到达开通时刻这一瞬间才显示有效订阅与
// 登记时保存的完整套餐条件；等待期间的查询不删除也不提前激活登记。
func TestStatusFutureSubscriptionWaitsForActivationInstant(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 500))
	mustAccount(t, s, "u")
	wantTerms := termsOf(planDef("a", 1000, 10, 100, 500))
	activateAt := utc(2026, 1, 20, 12, 0)
	mustSubscribe(t, s, "u", "a", activateAt)

	// 一月十日登记、一月二十日中午才开通：登记当天仍未生效。
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertNotSubscribed(t, st)

	// 开通月份已经开始但当日凌晨仍未到开通时刻：不按开通月份提前生效。
	clk.t = utc(2026, 1, 20, 0, 0)
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)

	// 等待期间修改套餐定义，随后生效时仍应使用登记时保存的快照。
	if err := s.UpdatePlan(planDef("a", 9999, 99, 9, 1000)); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)

	// 等待期间再次开通按已有订阅拒绝；安排换套餐、登记取消返回尚未生效。
	if err := s.Subscribe("u", "a", utc(2026, 1, 20, 12, 0)); !errors.Is(err, ErrSubscriptionExists) {
		t.Fatalf("duplicate subscribe while waiting: %v", err)
	}
	if _, err := s.SchedulePlanChange("u", "a"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("schedule change while waiting: %v", err)
	}
	if _, err := s.CancelSubscription("u"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("cancel while waiting: %v", err)
	}

	// 上述失败与查询都不能删除或提前激活登记。
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)

	// 开通时刻前一秒：仍未生效。
	clk.t = activateAt.Add(-time.Second)
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)

	// 到达开通时刻这一瞬间：直接查询即显示有效订阅与登记时的完整条件，
	// 无需先上报用量、安排换套餐或生成账单。
	clk.t = activateAt
	st, _ = s.Status("u")
	if !st.Subscribed {
		t.Fatalf("Subscribed = false at activation instant, status = %+v", st)
	}
	if st.CurrentTerms != wantTerms {
		t.Fatalf("CurrentTerms = %+v, want snapshot %+v", st.CurrentTerms, wantTerms)
	}
	if st.PendingChange != nil || st.ScheduledEnd != nil {
		t.Fatalf("unexpected pending/end at activation: %+v %v", st.PendingChange, st.ScheduledEnd)
	}
}

// TestStatusFutureActivationTimezoneInstant 验证开通时刻按同一瞬间比较：
// 用带时区的时钟表示同一时刻，在开通瞬间前后得到与 UTC 时钟一致的结果。
func TestStatusFutureActivationTimezoneInstant(t *testing.T) {
	plusOne := time.FixedZone("UTC+1", 60*60)
	activateUTC := utc(2026, 1, 20, 12, 0)

	s, clk := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	// 用带时区的开通时刻登记，与 UTC 的 12:00 是同一瞬间。
	mustSubscribe(t, s, "u", "a", time.Date(2026, 1, 20, 13, 0, 0, 0, plusOne))

	// 时钟也使用带时区的时刻：UTC 11:59（当地 12:59）尚未生效。
	clk.t = time.Date(2026, 1, 20, 12, 59, 0, 0, plusOne)
	st, _ := s.Status("u")
	assertNotSubscribed(t, st)

	// 当地 13:00 == UTC 12:00：同一瞬间即生效。
	clk.t = time.Date(2026, 1, 20, 13, 0, 0, 0, plusOne)
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms.PlanID != "a" {
		t.Fatalf("status at timezone instant = %+v, want subscribed with plan a", st)
	}

	// 再用 UTC 时钟观察同一瞬间，结果应一致。
	s2, clk2 := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s2, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s2, "u2")
	mustSubscribe(t, s2, "u2", "a", activateUTC)
	clk2.t = activateUTC.Add(-time.Second)
	st2, _ := s2.Status("u2")
	assertNotSubscribed(t, st2)
	clk2.t = activateUTC
	st2, _ = s2.Status("u2")
	if !st2.Subscribed || st2.CurrentTerms.PlanID != "a" {
		t.Fatalf("utc clock status at instant = %+v", st2)
	}
}

// TestStatusFutureResubscribeGapAndDebt 验证旧订阅终止后登记未来重新开通：
// 空档期查询不显示旧套餐或新套餐；历史用量与账单继续按账期次序展示；
// Subscribed 与欠费停用相互独立——等待开通不清旧欠费，结清旧欠费也不提前
// 激活新订阅。
func TestStatusFutureResubscribeGapAndDebt(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("c", 3000, 30, 300, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "jan", At: utc(2026, 1, 10, 0, 0), Quantity: 7}); err != nil {
		t.Fatal(err)
	}
	mustCancel(t, s, "u") // 旧订阅 2/1 终止
	clk.t = utc(2026, 2, 1, 0, 0)
	janBill := mustBill(t, s, "u", jan(2026)) // 月费 1000，留作欠费

	// 登记 2 月 10 日中午重新开通，此刻尚在空档中。
	mustSubscribe(t, s, "u", "c", utc(2026, 2, 10, 12, 0))

	// 旧订阅已结束、新订阅尚未开始：不显示任何生效套餐或终止安排，
	// 但历史用量与账单继续展示。
	clk.t = utc(2026, 2, 5, 0, 0)
	st, _ := s.Status("u")
	assertNotSubscribed(t, st)
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) || st.MonthlyUsage[0].Total != 7 {
		t.Fatalf("gap usage = %+v, want january history", st.MonthlyUsage)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) || st.Bills[0].Balance != janBill.TotalDue {
		t.Fatalf("gap bills = %+v, want january bill with original balance", st.Bills)
	}
	if st.Suspended {
		t.Fatal("should not be suspended before bill due date")
	}

	// 付款截止（账期结束后七天）过后：未生效与停用并存。
	clk.t = utc(2026, 2, 9, 0, 0)
	st, _ = s.Status("u")
	if st.Subscribed {
		t.Fatalf("future resubscription activated early: %+v", st)
	}
	if !st.Suspended {
		t.Fatal("old overdue debt should suspend the account during the gap")
	}

	// 结清旧欠费只解除停用，不能让未来订阅提前生效。
	if _, err := s.RecordPayment("u", "p1", jan(2026), janBill.TotalDue); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("u")
	if st.Suspended {
		t.Fatalf("still suspended after payment: %+v", st)
	}
	assertNotSubscribed(t, st)
	if len(st.Bills) != 1 || !st.Bills[0].Settled || st.Bills[0].Balance != 0 {
		t.Fatalf("bill state after payment = %+v", st.Bills)
	}

	// 开通瞬间前仍未生效。
	clk.t = utc(2026, 2, 10, 11, 59)
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)

	// 到达开通时刻：新订阅生效，历史用量与账单次序保持不变。
	clk.t = utc(2026, 2, 10, 12, 0)
	st, _ = s.Status("u")
	if !st.Subscribed {
		t.Fatalf("not subscribed at resubscribe instant: %+v", st)
	}
	if st.Suspended {
		t.Fatalf("suspended after old debt settled: %+v", st)
	}
	if st.CurrentTerms.PlanID != "c" || st.CurrentTerms.MonthlyFee != 3000 {
		t.Fatalf("current terms = %+v, want plan c snapshot", st.CurrentTerms)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) {
		t.Fatalf("history usage changed = %+v", st.MonthlyUsage)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) || !st.Bills[0].Settled {
		t.Fatalf("history bills changed = %+v", st.Bills)
	}
}
