package meter

import (
	"errors"
	"testing"
	"time"
)

// assertNotActivated 校验状态按“尚无实际生效订阅”展示：
// Subscribed 为 false，当前条件为零值，也没有待换套餐安排与待取消终止时刻。
func assertNotActivated(t *testing.T, st AccountStatus) {
	t.Helper()
	if st.Subscribed {
		t.Fatalf("Subscribed = true before activation time, status = %+v", st)
	}
	if st.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("CurrentTerms = %+v before activation time, want zero value", st.CurrentTerms)
	}
	if st.PendingChange != nil {
		t.Fatalf("PendingChange = %+v before activation time, want nil", st.PendingChange)
	}
	if st.ScheduledEnd != nil {
		t.Fatalf("ScheduledEnd = %v before activation time, want nil", *st.ScheduledEnd)
	}
}

// TestStatusFutureSubscriptionWaitsForActivationInstant 覆盖提前登记订阅后，
// Status 在开通时刻前按未生效展示；判断以实际开通时刻（UTC 瞬间）为界，
// 不按开通月份提前生效；查询与失败的写操作都不会删除或提前激活登记，
// 到达时刻后按登记时保存的套餐快照自动生效。
func TestStatusFutureSubscriptionWaitsForActivationInstant(t *testing.T) {
	planA := planDef("a", 1000, 10, 5, 1000)
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planA)
	mustPlan(t, s, planDef("b", 2000, 20, 8, 600))
	mustAccount(t, s, "u")

	// 一月十日登记一月二十日中午开通。
	activateAt := utc(2026, 1, 20, 12, 0)
	mustSubscribe(t, s, "u", "a", activateAt)
	wantTerms := termsOf(planA)

	// 登记后、开通前：未生效展示。
	assertNotActivated(t, mustStatus(t, s, "u"))

	// 等待期间换套餐、登记取消均按“尚未生效”拒绝。
	if _, err := s.SchedulePlanChange("u", "b"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("schedule before activation: %v", err)
	}
	if _, err := s.CancelSubscription("u"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("cancel before activation: %v", err)
	}
	// 再次开通仍按已有订阅的规则拒绝。
	if err := s.Subscribe("u", "b", activateAt); !errors.Is(err, ErrSubscriptionExists) {
		t.Fatalf("duplicate subscribe before activation: %v", err)
	}

	// 等待期间修改套餐定义，不能改变随后生效的订阅条件。
	if err := s.UpdatePlan(planDef("a", 9999, 99, 99, 5000)); err != nil {
		t.Fatal(err)
	}

	// 多次查询不应改变登记：仍未生效。
	assertNotActivated(t, mustStatus(t, s, "u"))

	// 一月二十日凌晨（同一自然月、同一天的早于时刻）：仍未生效。
	clk.t = utc(2026, 1, 20, 0, 0)
	assertNotActivated(t, mustStatus(t, s, "u"))

	// 开通时刻前一分钟：仍未生效。
	clk.t = utc(2026, 1, 20, 11, 59)
	assertNotActivated(t, mustStatus(t, s, "u"))

	// 到达开通时刻：直接查询即生效，返回登记时保存的完整套餐条件，
	// 无需先上报用量、安排换套餐或生成账单。
	clk.t = activateAt
	st := mustStatus(t, s, "u")
	if !st.Subscribed {
		t.Fatalf("Subscribed = false at activation instant, status = %+v", st)
	}
	if st.CurrentTerms != wantTerms {
		t.Fatalf("CurrentTerms = %+v, want registered snapshot %+v", st.CurrentTerms, wantTerms)
	}
	if st.PendingChange != nil || st.ScheduledEnd != nil {
		t.Fatalf("pending = %+v scheduledEnd = %v, want both nil", st.PendingChange, st.ScheduledEnd)
	}
}

// TestStatusFutureSubscriptionWithTimezone 验证使用带时区的开通时间时，
// 同一瞬间无论用何种偏移表示，Status 的生效判断都相同。
func TestStatusFutureSubscriptionWithTimezone(t *testing.T) {
	planA := planDef("a", 1000, 10, 5, 0)
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planA)
	mustAccount(t, s, "tz")

	// 本地钟面 2026-01-20 20:00（UTC+08:00）= 2026-01-20 12:00 UTC。
	activateWall := time.Date(2026, 1, 20, 20, 0, 0, 0, zonePlus8)
	mustSubscribe(t, s, "tz", "a", activateWall)

	// 开通前：UTC 11:59（用 UTC-05:00 表示为 06:59，同一瞬间）。
	clk.t = time.Date(2026, 1, 20, 6, 59, 0, 0, zoneMinus5)
	assertNotActivated(t, mustStatus(t, s, "tz"))
	clk.t = utc(2026, 1, 20, 11, 59)
	assertNotActivated(t, mustStatus(t, s, "tz"))

	// 开通瞬间：UTC 12:00，用 UTC-05:00 的 07:00 表示同一瞬间，结果一致。
	clk.t = time.Date(2026, 1, 20, 7, 0, 0, 0, zoneMinus5)
	st := mustStatus(t, s, "tz")
	if !st.Subscribed || st.CurrentTerms != termsOf(planA) {
		t.Fatalf("status at activation instant (UTC-05:00 representation) = %+v", st)
	}
	clk.t = time.Date(2026, 1, 20, 20, 0, 0, 0, zonePlus8)
	st = mustStatus(t, s, "tz")
	if !st.Subscribed || st.CurrentTerms != termsOf(planA) {
		t.Fatalf("status at activation instant (UTC+08:00 representation) = %+v", st)
	}
}

// TestStatusFutureResubscribeGap 覆盖旧订阅已终止、未来重新开通尚未开始的
// 间隔：查询不显示旧套餐也不显示新套餐；历史用量与账单继续展示；欠费停用与
// 订阅生效相互独立，结清旧欠费不会让未来订阅提前生效；到达开通时刻后按
// 重新开通登记时的快照生效。
func TestStatusFutureResubscribeGap(t *testing.T) {
	planA := planDef("a", 1000, 10, 5, 0)
	planC := planDef("c", 3000, 30, 9, 0)
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planA)
	mustPlan(t, s, planC)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 一月有 12 单位用量，登记按月取消，二月一日旧订阅终止。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "jan-use", At: utc(2026, 1, 10, 0, 0), Quantity: 12}); err != nil {
		t.Fatal(err)
	}
	mustCancel(t, s, "u")
	clk.t = utc(2026, 2, 1, 0, 0)

	// 一月账单：月费 1000 + 超额 2*5 = 1010，截止 2 月 8 日。
	janBill := mustBill(t, s, "u", jan(2026))
	if janBill.TotalDue != 1010 {
		t.Fatalf("jan bill total = %d, want 1010", janBill.TotalDue)
	}

	// 旧订阅已结束、新订阅尚未开始：登记二月二十日中午用套餐 c 重新开通。
	mustSubscribe(t, s, "u", "c", utc(2026, 2, 20, 12, 0))
	// 登记后修改套餐 c，随后生效的条件仍以登记时快照为准。
	if err := s.UpdatePlan(planDef("c", 7777, 77, 77, 777)); err != nil {
		t.Fatal(err)
	}

	// 二月十日（已过账单截止日）：旧欠费导致停用，但订阅未生效；
	// 状态既不显示旧套餐 a，也不显示新套餐 c。
	clk.t = utc(2026, 2, 10, 12, 0)
	st := mustStatus(t, s, "u")
	assertNotActivated(t, st)
	if !st.Suspended {
		t.Fatalf("Suspended = false with overdue prior bill in gap")
	}

	// 历史各月用量继续展示，账期次序保持原规则。
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) || st.MonthlyUsage[0].Total != 12 {
		t.Fatalf("monthly usage in gap = %+v", st.MonthlyUsage)
	}
	// 已生成账单继续展示，余额保持原查询规则。
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) ||
		st.Bills[0].TotalDue != 1010 || st.Bills[0].Balance != 1010 || st.Bills[0].Settled {
		t.Fatalf("bills in gap = %+v", st.Bills)
	}

	// 空档期间换套餐、取消仍按订阅尚未生效拒绝；再次开通也被拒绝。
	if _, err := s.SchedulePlanChange("u", "a"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("schedule in gap: %v", err)
	}
	if _, err := s.CancelSubscription("u"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("cancel in gap: %v", err)
	}
	if err := s.Subscribe("u", "a", utc(2026, 2, 20, 12, 0)); !errors.Is(err, ErrSubscriptionExists) {
		t.Fatalf("duplicate subscribe in gap: %v", err)
	}

	// 结清旧欠费：停用解除，但未来订阅不能提前生效。
	if _, err := s.RecordPayment("u", "pay-jan", jan(2026), 1010); err != nil {
		t.Fatalf("settle old bill: %v", err)
	}
	st = mustStatus(t, s, "u")
	if st.Suspended {
		t.Fatalf("Suspended = true after settling overdue bill")
	}
	if st.Subscribed || st.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("future subscription activated early by payment: %+v", st)
	}
	// 付款与账单状态反映结清结果，历史数据不变。
	if len(st.Bills) != 1 || st.Bills[0].Balance != 0 || !st.Bills[0].Settled {
		t.Fatalf("bills after payment = %+v", st.Bills)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Total != 12 {
		t.Fatalf("monthly usage after payment = %+v", st.MonthlyUsage)
	}

	// 二月二十日凌晨仍未生效；中午到达开通时刻，按重新开通登记时的套餐 c 生效。
	clk.t = utc(2026, 2, 20, 0, 0)
	assertNotActivated(t, mustStatus(t, s, "u"))
	clk.t = utc(2026, 2, 20, 12, 0)
	st = mustStatus(t, s, "u")
	if !st.Subscribed {
		t.Fatalf("Subscribed = false at resubscription instant, status = %+v", st)
	}
	if st.CurrentTerms != termsOf(planC) {
		t.Fatalf("CurrentTerms = %+v, want resubscription snapshot %+v", st.CurrentTerms, termsOf(planC))
	}
	if st.Suspended {
		t.Fatalf("Suspended = true after old debt settled")
	}
	// 历史用量与账单在新订阅生效后仍继续展示。
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) || st.MonthlyUsage[0].Total != 12 {
		t.Fatalf("monthly usage after activation = %+v", st.MonthlyUsage)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) || !st.Bills[0].Settled {
		t.Fatalf("bills after activation = %+v", st.Bills)
	}
}

func mustStatus(t *testing.T, s *Service, acct string) AccountStatus {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %s: %v", acct, err)
	}
	return st
}
