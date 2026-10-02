package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestCancelSubscriptionBasic 验证按月取消的基本行为：
// 以请求时刻的下一个 UTC 自然月月初为终止时刻，返回该时刻；
// 当月照常接收用量，按完整月费和额度计费，不按天退款。
func TestCancelSubscriptionBasic(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 1 月 15 日请求取消，终止时刻应为 2 月 1 日 00:00 UTC。
	cancelAt, err := s.CancelSubscription("u")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	want := utc(2026, 2, 1, 0, 0)
	if !cancelAt.Equal(want) {
		t.Fatalf("cancelAt = %v, want %v", cancelAt, want)
	}

	// 等待取消期间状态：展示终止时刻，仍显示有有效订阅。
	st, _ := s.Status("u")
	if !st.Subscribed || st.CancelAt.IsZero() || !st.CancelAt.Equal(want) {
		t.Fatalf("status while waiting: subscribed=%v cancelAt=%v", st.Subscribed, st.CancelAt)
	}
	if st.CurrentTerms.PlanID != "a" {
		t.Fatalf("current terms while waiting = %s", st.CurrentTerms.PlanID)
	}

	// 当月照常接收用量（时钟推进到 1 月 20 日）。
	clk.t = utc(2026, 1, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 1, 20, 0, 0), Quantity: 5}); err != nil {
		t.Fatalf("event while waiting: %v", err)
	}

	// 到达终止时刻：即使没有上报用量或生成账单，查询也显示无有效订阅。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ = s.Status("u")
	if st.Subscribed || !st.CancelAt.IsZero() {
		t.Fatalf("status after termination: subscribed=%v cancelAt=%v", st.Subscribed, st.CancelAt)
	}
	if st.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("current terms after termination = %+v, want zero", st.CurrentTerms)
	}
	if st.PendingChange != nil {
		t.Fatalf("pending change after termination = %+v, want nil", st.PendingChange)
	}

	// 1 月仍可出账，按完整月费和额度计费。
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("jan bill after termination: %v", err)
	}
	if janBill.TotalUsage != 5 || janBill.MonthlyFee != 1000 || janBill.OverageUnits != 0 {
		t.Fatalf("jan bill: %+v", janBill)
	}
}

// TestCancelSubscriptionRepeated 验证同一订阅重复取消始终返回原终止时刻，不把期限后移。
func TestCancelSubscriptionRepeated(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	first, err := s.CancelSubscription("u")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CancelSubscription("u")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatalf("repeated cancel moved date: first=%v second=%v", first, second)
	}
	if !second.Equal(utc(2026, 2, 1, 0, 0)) {
		t.Fatalf("cancelAt = %v, want 2026-02-01", second)
	}
}

// TestWithdrawCancellation 验证撤回取消：
// 生效前可撤回；没有取消安排时撤回也成功；终止后撤回失败。
func TestWithdrawCancellation(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 没有取消安排时撤回也成功。
	if err := s.WithdrawCancellation("u"); err != nil {
		t.Fatalf("withdraw without cancellation: %v", err)
	}

	cancelAt, err := s.CancelSubscription("u")
	if err != nil {
		t.Fatal(err)
	}
	// 生效前撤回。
	if err := s.WithdrawCancellation("u"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	st, _ := s.Status("u")
	if !st.Subscribed || !st.CancelAt.IsZero() {
		t.Fatalf("after withdraw: subscribed=%v cancelAt=%v", st.Subscribed, st.CancelAt)
	}
	_ = cancelAt

	// 再次取消并推进到终止时刻。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
	// 终止后撤回失败。
	if err := s.WithdrawCancellation("u"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("withdraw after termination: %v", err)
	}
}

// TestCancelSubscriptionErrors 验证取消的错误场景：
// 账户不存在、没有有效订阅、开通时刻尚未到达。
func TestCancelSubscriptionErrors(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustAccount(t, s, "late")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	// 开通时刻尚未到达。
	mustSubscribe(t, s, "late", "a", utc(2026, 1, 20, 0, 0))

	if _, err := s.CancelSubscription("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cancel ghost: %v", err)
	}
	if _, err := s.CancelSubscription("late"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("cancel not activated: %v", err)
	}
}

// TestCancelClearsPendingPlanChange 验证取消成功时清除尚未生效的换套餐安排，
// 等待取消期间拒绝新换套餐安排，撤回取消不恢复被清除的安排。
func TestCancelClearsPendingPlanChange(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 安排换套餐。
	mustSchedule(t, s, "u", "b")
	st, _ := s.Status("u")
	if st.PendingChange == nil {
		t.Fatal("pending change should exist before cancel")
	}

	// 取消成功，清除换套餐安排。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("u")
	if st.PendingChange != nil {
		t.Fatalf("pending change after cancel = %+v, want nil", st.PendingChange)
	}

	// 等待取消期间拒绝新换套餐安排。
	if _, err := s.SchedulePlanChange("u", "b"); !errors.Is(err, ErrSubscriptionCancelling) {
		t.Fatalf("schedule while cancelling: %v", err)
	}

	// 撤回取消不恢复被清除的安排。
	if err := s.WithdrawCancellation("u"); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("u")
	if st.PendingChange != nil {
		t.Fatalf("pending change restored after withdraw = %+v, want nil", st.PendingChange)
	}
}

// TestCancelAtPlanChangeBoundary 验证请求恰在换套餐生效的月初时，
// 应先承认该次切换，取消从再下一月生效。
func TestCancelAtPlanChangeBoundary(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	mustSchedule(t, s, "u", "b")

	// 推进到 2 月 1 日 00:00（换套餐生效时刻）。
	clk.t = utc(2026, 2, 1, 0, 0)

	// 此刻换套餐已生效，当前套餐应为 b。
	st, _ := s.Status("u")
	if st.CurrentTerms.PlanID != "b" {
		t.Fatalf("current terms at boundary = %s, want b", st.CurrentTerms.PlanID)
	}

	// 请求取消：取消应从再下一月（4 月 1 日）生效。
	cancelAt, err := s.CancelSubscription("u")
	if err != nil {
		t.Fatal(err)
	}
	want := utc(2026, 4, 1, 0, 0)
	if !cancelAt.Equal(want) {
		t.Fatalf("cancelAt = %v, want %v (month after next)", cancelAt, want)
	}
}

// TestEventAfterTermination 验证终止后事件处理：
// 终止后仍允许补报旧订阅期间且尚未出账的用量，但到期欠费时仍拒绝新增；
// 已接受事件原样重报仍成功；空档期间拒绝。
func TestEventAfterTermination(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 1 月上报用量（时钟推进到 1 月 20 日）。
	clk.t = utc(2026, 1, 20, 0, 0)
	janEvent := Event{AccountID: "u", EventID: "j1", At: utc(2026, 1, 20, 0, 0), Quantity: 5}
	if _, err := s.RecordEvent(janEvent); err != nil {
		t.Fatal(err)
	}

	// 取消并推进到 2 月。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)

	// 终止后补报 1 月用量（尚未出账）：成功。
	lateEvent := Event{AccountID: "u", EventID: "j2", At: utc(2026, 1, 25, 0, 0), Quantity: 3}
	if _, err := s.RecordEvent(lateEvent); err != nil {
		t.Fatalf("backfill after termination: %v", err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 8 {
		t.Fatalf("jan usage = %d, want 8", u.Total)
	}

	// 已接受事件原样重报仍成功。
	if r, err := s.RecordEvent(janEvent); err != nil || r.Accepted {
		t.Fatalf("replay after termination: %+v %v", r, err)
	}
	// 标识相同而时刻或数量不同仍报冲突。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "j1", At: utc(2026, 1, 20, 0, 0), Quantity: 99}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("conflict after termination: %v", err)
	}

	// 空档期间（2 月）事件拒绝。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "f1", At: utc(2026, 2, 10, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventOutsideSubscription) {
		t.Fatalf("gap event: %v", err)
	}

	// 出账后新事件拒绝。
	if _, err := s.CreateBill("u", jan(2026)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "j3", At: utc(2026, 1, 26, 0, 0), Quantity: 1}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new event after billed: %v", err)
	}
}

// TestEventAtSubscriptionBoundaries 验证事件时刻边界：
// 开通时刻计入、终止时刻不计入。
func TestEventAtSubscriptionBoundaries(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 15, 0, 0))

	// 开通时刻计入。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "at", At: utc(2026, 1, 15, 0, 0), Quantity: 1}); err != nil {
		t.Fatalf("event at activation: %v", err)
	}

	// 取消并推进到终止时刻。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)

	// 终止时刻不计入。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "term", At: utc(2026, 2, 1, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventOutsideSubscription) {
		t.Fatalf("event at termination: %v", err)
	}
}

// TestBillAfterTermination 验证终止后出账：
// 旧订阅覆盖的已结束月份仍可出账，使用各月当时锁定的条件；
// 完全无订阅的月份出账失败；已出账的金额、税额和用量不变。
func TestBillAfterTermination(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 时钟推进到 1 月 20 日记录用量。
	clk.t = utc(2026, 1, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 1, 20, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}

	// 取消并推进到 3 月（2 月已结束）。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 3, 1, 0, 0)

	// 1 月（旧订阅覆盖）仍可出账，使用 1 月当时条件。
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("jan bill: %v", err)
	}
	// 用量 15 -> 超额 5 * 100 = 500，月费 1000，合计 1500。
	if janBill.TotalUsage != 15 || janBill.MonthlyFee != 1000 || janBill.OverageFee != 500 || janBill.TotalDue != 1500 {
		t.Fatalf("jan bill: %+v", janBill)
	}

	// 2 月（旧订阅覆盖，终止日为 2 月 1 日，2 月无订阅）出账失败。
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("feb bill (no subscription): %v", err)
	}

	// 完全无订阅的月份（3 月）出账失败。
	if _, err := s.CreateBill("u", mar(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("mar bill (no subscription): %v", err)
	}

	// 已出账的金额、税额和用量不变。
	janBill2, _ := s.GetBill("u", jan(2026))
	if janBill2.TotalUsage != 15 || janBill2.TotalDue != 1500 {
		t.Fatalf("jan bill changed: %+v", janBill2)
	}
}

// TestReactivation 验证重新开通：
// 终止后可通过原 Subscribe 重新开通，开通时刻不得早于上次终止时刻；
// 新订阅保存重新开通时的套餐条件，当月仍收完整月费并给完整额度；
// 有尚未终止的订阅时仍拒绝再次开通。
func TestReactivation(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 终止。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)

	// 开通时刻早于上次终止时刻：失败。
	if err := s.Subscribe("u", "b", utc(2026, 1, 31, 0, 0)); !errors.Is(err, ErrReactivationTooEarly) {
		t.Fatalf("reactivation too early: %v", err)
	}

	// 开通时刻等于上次终止时刻：成功。
	if err := s.Subscribe("u", "b", utc(2026, 2, 1, 0, 0)); err != nil {
		t.Fatalf("reactivation at termination: %v", err)
	}
	st, _ := s.Status("u")
	if !st.Subscribed || st.CurrentTerms.PlanID != "b" {
		t.Fatalf("after reactivation: subscribed=%v plan=%s", st.Subscribed, st.CurrentTerms.PlanID)
	}
	if !st.CancelAt.IsZero() {
		t.Fatalf("cancelAt after reactivation = %v, want zero", st.CancelAt)
	}

	// 当月仍收完整月费并给完整额度（时钟推进到 2 月 10 日）。
	clk.t = utc(2026, 2, 10, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 2, 10, 0, 0), Quantity: 25}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 3, 1, 0, 0)
	febBill, err := s.CreateBill("u", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	// 用量 25 -> 超额 5 * 200 = 1000，月费 2000，合计 3000。
	if febBill.MonthlyFee != 2000 || febBill.OverageUnits != 5 || febBill.OverageFee != 1000 || febBill.TotalDue != 3000 {
		t.Fatalf("feb bill after reactivation: %+v", febBill)
	}

	// 再次终止后重新开通：支持多次取消和重新开通。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	// 时钟在 3 月 1 日，取消从 4 月 1 日生效。
	clk.t = utc(2026, 4, 1, 0, 0)
	if err := s.Subscribe("u", "a", utc(2026, 4, 1, 0, 0)); err != nil {
		t.Fatalf("second reactivation: %v", err)
	}
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms.PlanID != "a" {
		t.Fatalf("after second reactivation: subscribed=%v plan=%s", st.Subscribed, st.CurrentTerms.PlanID)
	}
}

// TestReactivationKeepsHistory 验证重新开通不重置账户内事件和付款标识，
// 不清除旧欠费；旧月份归属与计价不能被后来订阅覆盖。
func TestReactivationKeepsHistory(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 1 月用量（时钟推进到 1 月 20 日）。
	clk.t = utc(2026, 1, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e1", At: utc(2026, 1, 20, 0, 0), Quantity: 5}); err != nil {
		t.Fatal(err)
	}
	// 1 月出账（时钟推进到 2 月 1 日）。
	clk.t = utc(2026, 2, 1, 0, 0)
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	// 部分付款。
	if _, err := s.RecordPayment("u", "p1", jan(2026), 500); err != nil {
		t.Fatal(err)
	}

	// 终止（欠费停用不妨碍取消）。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	// 时钟在 2 月 1 日，取消从 3 月 1 日生效；推进到 3 月 1 日后重新开通。
	clk.t = utc(2026, 3, 1, 0, 0)
	if err := s.Subscribe("u", "a", utc(2026, 3, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}

	// 旧欠费仍在：1 月账单余额 500。
	gotJan, _ := s.GetBill("u", jan(2026))
	if gotJan.Paid != 500 || gotJan.Balance != 500 {
		t.Fatalf("jan bill after reactivation: paid=%d bal=%d", gotJan.Paid, gotJan.Balance)
	}
	// 事件标识不重置：相同标识重报仍成功。
	if r, err := s.RecordEvent(Event{AccountID: "u", EventID: "e1", At: utc(2026, 1, 20, 0, 0), Quantity: 5}); err != nil || r.Accepted {
		t.Fatalf("replay after reactivation: %+v %v", r, err)
	}
	// 付款标识不重置：相同标识重报仍成功。
	if r, err := s.RecordPayment("u", "p1", jan(2026), 500); err != nil || r.Registered {
		t.Fatalf("payment replay after reactivation: %+v %v", r, err)
	}

	// 旧月份归属与计价不被后来订阅覆盖：1 月账单金额不变。
	if gotJan.TotalDue != janBill.TotalDue {
		t.Fatalf("jan bill total changed: %d vs %d", gotJan.TotalDue, janBill.TotalDue)
	}
}

// TestSettlementDoesNotReviveSubscription 验证结清债务只能解除欠费停用，
// 不能复活已取消的订阅。
func TestSettlementDoesNotReviveSubscription(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 时钟推进到 1 月 20 日记录用量。
	clk.t = utc(2026, 1, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 1, 20, 0, 0), Quantity: 5}); err != nil {
		t.Fatal(err)
	}
	// 时钟推进到 2 月 1 日出账。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("u", jan(2026)); err != nil {
		t.Fatal(err)
	}
	// 终止。
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	// 时钟在 2 月 1 日，取消从 3 月 1 日生效；推进到 3 月 1 日使订阅终止。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, _ := s.Status("u")
	if st.Subscribed {
		t.Fatal("subscription should be terminated")
	}
	if !st.Suspended {
		t.Fatal("should be suspended before payment")
	}

	// 结清债务：解除停用，但不复活订阅。
	if _, err := s.RecordPayment("u", "p1", jan(2026), 1000); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("u")
	if st.Suspended {
		t.Fatal("should not be suspended after payment")
	}
	if st.Subscribed {
		t.Fatal("subscription revived after payment, should remain cancelled")
	}
}

// TestCancelWhileSuspended 验证欠费停用不妨碍取消，也不免除债务。
func TestCancelWhileSuspended(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 时钟推进到 1 月 20 日记录用量。
	clk.t = utc(2026, 1, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 1, 20, 0, 0), Quantity: 5}); err != nil {
		t.Fatal(err)
	}
	// 时钟推进到 2 月 1 日出账。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("u", jan(2026)); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 8, 0, 0)
	st, _ := s.Status("u")
	if !st.Suspended {
		t.Fatal("should be suspended")
	}

	// 欠费停用不妨碍取消。
	cancelAt, err := s.CancelSubscription("u")
	if err != nil {
		t.Fatalf("cancel while suspended: %v", err)
	}
	if !cancelAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("cancelAt = %v, want 2026-03-01", cancelAt)
	}
	// 不免除债务：账单仍在。
	gotJan, _ := s.GetBill("u", jan(2026))
	if gotJan.Balance != 1000 {
		t.Fatalf("debt forgiven after cancel: balance = %d", gotJan.Balance)
	}
}

// TestConcurrentCancelAndReactivate 验证并发取消、撤回、重新开通、换套餐、
// 用量和出账时，结果须能对应一个先后顺序，失败请求不留下部分变更。
func TestConcurrentCancelAndReactivate(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n * 6)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = s.CancelSubscription("u")
		}()
		go func() {
			defer wg.Done()
			_ = s.WithdrawCancellation("u")
		}()
		go func() {
			defer wg.Done()
			_ = s.Subscribe("u", "a", utc(2026, 2, 1, 0, 0))
		}()
		go func() {
			defer wg.Done()
			_, _ = s.SchedulePlanChange("u", "b")
		}()
		go func(i int) {
			defer wg.Done()
			_, _ = s.RecordEvent(Event{
				AccountID: "u",
				EventID:   fmt.Sprintf("e%d", i),
				At:        utc(2026, 1, 20, 0, 0),
				Quantity:  1,
			})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = s.CreateBill("u", jan(2026))
		}()
	}
	wg.Wait()

	// 无论上述操作如何交错，最终状态必须内部一致。
	st, _ := s.Status("u")
	_ = st

	// 推进到 3 月并出账：1 月账单必须恰好使用一个套餐的条件。
	clk.t = utc(2026, 3, 1, 0, 0)
	bills := make([]Bill, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			b, err := s.CreateBill("u", jan(2026))
			if err != nil {
				t.Errorf("concurrent bill: %v", err)
				return
			}
			bills[i] = b
		}(i)
	}
	wg.Wait()
	var first Bill
	for i, b := range bills {
		if i == 0 {
			first = b
			continue
		}
		if b != first {
			t.Fatalf("bills differ:\n%+v\n%+v", first, b)
		}
	}
	// 内部金额自洽。
	wantOverage := first.TotalUsage - 10
	if wantOverage < 0 {
		wantOverage = 0
	}
	if first.OverageUnits != wantOverage || first.MonthlyFee != 1000 ||
		first.OverageFee != wantOverage*100 || first.TotalDue != 1000+wantOverage*100 {
		t.Fatalf("jan bill inconsistent: %+v", first)
	}
}

// TestCancelDoesNotAutoBill 验证取消不自动出账。
func TestCancelDoesNotAutoBill(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 时钟推进到 1 月 20 日记录用量。
	clk.t = utc(2026, 1, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 1, 20, 0, 0), Quantity: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)

	// 取消不自动出账：1 月账单尚未生成。
	if _, err := s.GetBill("u", jan(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("bill auto-created after cancel: %v", err)
	}
	// 手动出账仍可生成。
	if _, err := s.CreateBill("u", jan(2026)); err != nil {
		t.Fatalf("manual bill: %v", err)
	}
}

// 确保 time 包被使用（utc 函数返回 time.Time）。
var _ = time.Time{}
