package meter

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func apr(y int) Month { return Month{Year: y, Month: time.April} }

func mustCancel(t *testing.T, s *Service, acct string) CancelSubscriptionResult {
	t.Helper()
	r, err := s.CancelSubscription(acct)
	if err != nil {
		t.Fatalf("cancel %s: %v", acct, err)
	}
	return r
}

// TestCancelSubscriptionScheduleEndAndStatus 覆盖终止时刻、重复取消不后移、
// 等待期与终止后的状态展示，以及当月完整计费、历史可查。
func TestCancelSubscriptionScheduleEndAndStatus(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 10, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 终止时刻为请求时刻的下一个 UTC 自然月月初。
	r := mustCancel(t, s, "u")
	wantEnd := utc(2026, 2, 1, 0, 0)
	if !r.Cancelled || !r.Cancellation.EndAt.Equal(wantEnd) {
		t.Fatalf("cancel result = %+v, want end %v", r, wantEnd)
	}
	// 重复取消始终返回原终止时刻，不后移。
	clk.t = utc(2026, 1, 20, 9, 0)
	r2 := mustCancel(t, s, "u")
	if r2.Cancelled || !r2.Cancellation.EndAt.Equal(wantEnd) {
		t.Fatalf("repeat cancel = %+v, want same end %v", r2, wantEnd)
	}

	// 等待取消期间仍显示有有效订阅，并展示终止时刻。
	st, _ := s.Status("u")
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("status while cancelling: %+v", st)
	}
	if st.CurrentTerms.PlanID != "a" || st.PendingChange != nil {
		t.Fatalf("terms/pending while cancelling: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 当月照常接收用量。
	clk.t = utc(2026, 1, 25, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "j", At: utc(2026, 1, 25, 0, 0), Quantity: 15}); err != nil {
		t.Fatalf("usage in cancellation month: %v", err)
	}

	// 到达终止时刻：即使没有再上报用量或生成账单，也立即无有效订阅、无当前套餐、
	// 无待切换、无终止安排。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ = s.Status("u")
	if st.Subscribed || st.CurrentTerms != (PlanTerms{}) || st.PendingChange != nil || st.ScheduledEnd != nil {
		t.Fatalf("status at end: %+v", st)
	}

	// 历史用量继续可查。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 15 {
		t.Fatalf("jan usage after end = %d", u.Total)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) {
		t.Fatalf("history usage lost: %+v", st.MonthlyUsage)
	}

	// 取消当月按完整月费与额度计费，不按天退款：
	// fee 1000 + 超额 (15-10)*10 = 1500。
	b := mustBill(t, s, "u", jan(2026))
	if b.MonthlyFee != 1000 || b.OverageUnits != 5 || b.OverageFee != 50 || b.TotalDue != 1050 {
		t.Fatalf("cancellation month bill not full terms: %+v", b)
	}
	// 历史账单在状态中继续可查。
	st, _ = s.Status("u")
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) {
		t.Fatalf("history bills lost: %+v", st.Bills)
	}
}

// TestUndoCancellation 覆盖撤回：生效前可撤回、无安排也成功、终止后失败、
// 撤回不恢复被清除的换套餐安排。
func TestUndoCancellation(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 8, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 有效订阅没有取消安排时撤回也成功。
	if err := s.UndoCancelSubscription("u"); err != nil {
		t.Fatalf("undo without arrangement: %v", err)
	}

	// 登记的换套餐安排会被取消清除；撤回取消不恢复它。
	mustSchedule(t, s, "u", "b")
	mustCancel(t, s, "u")
	st, _ := s.Status("u")
	if st.PendingChange != nil || st.ScheduledEnd == nil {
		t.Fatalf("pending not cleared by cancel: %+v", st)
	}
	if err := s.UndoCancelSubscription("u"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	st, _ = s.Status("u")
	if st.Subscribed != true || st.ScheduledEnd != nil || st.PendingChange != nil {
		t.Fatalf("status after undo: %+v", st)
	}
	// 撤回后订阅照常延续：2 月仍是原套餐且可接收用量。
	clk.t = utc(2026, 2, 10, 0, 0)
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms.PlanID != "a" {
		t.Fatalf("subscription ended despite undo: %+v", st)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "f", At: utc(2026, 2, 5, 0, 0), Quantity: 1}); err != nil {
		t.Fatalf("event after undo: %v", err)
	}

	// 终止后撤回失败。
	mustCancel(t, s, "u") // clk 2/10 -> end 3/1
	clk.t = utc(2026, 3, 1, 0, 0)
	if err := s.UndoCancelSubscription("u"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("undo after end = %v", err)
	}

	// 引用错误。
	if err := s.UndoCancelSubscription("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("undo ghost: %v", err)
	}
	mustAccount(t, s, "nosub")
	if err := s.UndoCancelSubscription("nosub"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("undo without subscription: %v", err)
	}
}

// TestCancelInteractsWithPlanChanges 覆盖清除待生效安排、等待期拒绝新安排，
// 以及恰在换套餐生效月初时先承认切换、取消从再下一月生效。
func TestCancelInteractsWithPlanChanges(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 8, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 登记取消清除尚未生效的安排，等待期间拒绝新安排。
	mustSchedule(t, s, "u", "b")
	r := mustCancel(t, s, "u")
	if !r.Cancellation.EndAt.Equal(utc(2026, 2, 1, 0, 0)) {
		t.Fatalf("end = %v", r.Cancellation.EndAt)
	}
	st, _ := s.Status("u")
	if st.PendingChange != nil {
		t.Fatalf("pending survived cancel: %+v", st.PendingChange)
	}
	if _, err := s.SchedulePlanChange("u", "b"); !errors.Is(err, ErrPlanChangeWhileCancelling) {
		t.Fatalf("schedule while cancelling: %v", err)
	}
	// 失败的安排不留痕迹。
	st, _ = s.Status("u")
	if st.PendingChange != nil || st.ScheduledEnd == nil {
		t.Fatalf("state after rejected schedule: %+v", st)
	}

	// 恰在换套餐生效的月初：先承认该次切换，取消从再下一月生效。
	mustAccount(t, s, "v")
	mustSubscribe(t, s, "v", "a", utc(2026, 1, 1, 0, 0))
	mustSchedule(t, s, "v", "b")
	clk.t = utc(2026, 2, 1, 0, 0)
	rv := mustCancel(t, s, "v")
	if !rv.Cancellation.EndAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("boundary cancel end = %v, want 2026-03-01", rv.Cancellation.EndAt)
	}
	st, _ = s.Status("v")
	if st.CurrentTerms.PlanID != "b" || st.PendingChange != nil {
		t.Fatalf("switch not acknowledged before cancel: %+v", st)
	}
	// 终止后该段以 b 套餐条件记入历史。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, _ = s.Status("v")
	if st.Subscribed {
		t.Fatalf("v still subscribed after end")
	}
	febBill := mustBill(t, s, "v", feb(2026))
	if febBill.Terms.PlanID != "b" || febBill.MonthlyFee != 2000 {
		t.Fatalf("feb bill after boundary cancel: %+v", febBill)
	}
}

// TestCancelSubscriptionErrors 覆盖不能取消的情形。
func TestCancelSubscriptionErrors(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 0))
	mustAccount(t, s, "u")
	mustAccount(t, s, "late")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	// 开通时刻尚未到达。
	mustSubscribe(t, s, "late", "a", utc(2026, 1, 20, 0, 0))
	mustAccount(t, s, "never")

	if _, err := s.CancelSubscription("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if _, err := s.CancelSubscription(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty account: %v", err)
	}
	if _, err := s.CancelSubscription("never"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("never subscribed: %v", err)
	}
	if _, err := s.CancelSubscription("late"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("not activated: %v", err)
	}

	// 终止后再取消失败。
	mustCancel(t, s, "u")
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CancelSubscription("u"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("cancel after end: %v", err)
	}
}

// TestCancelWhileSuspended 欠费停用不妨碍取消，也不免除债务。
func TestCancelWhileSuspended(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 0, 0, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 1, 10, 0, 0), Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("u", jan(2026)); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 8, 0, 0)
	st, _ := s.Status("u")
	if !st.Suspended {
		t.Fatal("should be suspended")
	}

	// 停用期间仍可取消。
	r := mustCancel(t, s, "u")
	if !r.Cancellation.EndAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("suspended cancel end = %v", r.Cancellation.EndAt)
	}
	st, _ = s.Status("u")
	if !st.Suspended || !st.Subscribed {
		t.Fatalf("cancel changed suspension/debt: %+v", st)
	}
	// 终止后欠费依旧存在。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, _ = s.Status("u")
	if st.Subscribed || !st.Suspended {
		t.Fatalf("debt cleared at end: %+v", st)
	}
	if len(st.Bills) != 1 || st.Bills[0].Balance != 1000 {
		t.Fatalf("bill missing after end: %+v", st.Bills)
	}
	// 结清只解除停用，不复活已取消的订阅。
	if _, err := s.RecordPayment("u", "p", jan(2026), 1000); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("u")
	if st.Suspended || st.Subscribed {
		t.Fatalf("payment revived subscription: %+v", st)
	}
}

// TestEventMembershipAcrossSegments 事件按发生时刻判断订阅期间归属：
// 开通时刻计入、终止时刻不计入、空档拒绝；跨取消与重新开通的重报规则。
func TestEventMembershipAcrossSegments(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 开通时刻计入。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "start", At: utc(2026, 1, 1, 0, 0), Quantity: 1}); err != nil {
		t.Fatalf("event at activation: %v", err)
	}

	mustCancel(t, s, "u") // end 2/1

	// 等待取消期间、终止前一刻仍接收。
	clk.t = utc(2026, 1, 31, 23, 59)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "last", At: utc(2026, 1, 31, 23, 59), Quantity: 1}); err != nil {
		t.Fatalf("event just before end: %v", err)
	}
	// 终止时刻不计入。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "at-end", At: utc(2026, 2, 1, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event at end: %v", err)
	}
	// 空档期间拒绝。
	clk.t = utc(2026, 2, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "gap", At: utc(2026, 2, 10, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event in gap: %v", err)
	}

	// 3/1 重新开通：2 月事件仍属空档，3/1 00:00（开通时刻）计入新段。
	mustSubscribe(t, s, "u", "a", utc(2026, 3, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "gap2", At: utc(2026, 2, 15, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("past gap after resubscribe: %v", err)
	}
	clk.t = utc(2026, 3, 2, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "restart", At: utc(2026, 3, 1, 0, 0), Quantity: 1}); err != nil {
		t.Fatalf("event at reactivation: %v", err)
	}

	// 已接受事件原样重报，在取消和重新开通后仍成功、不重复累计。
	old := Event{AccountID: "u", EventID: "start", At: utc(2026, 1, 1, 0, 0), Quantity: 1}
	if r, err := s.RecordEvent(old); err != nil || r.Accepted {
		t.Fatalf("identical replay across segments: %+v %v", r, err)
	}
	// 标识相同而时刻或数量不同仍报冲突。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "start", At: utc(2026, 3, 1, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("same id new time: %v", err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "start", At: utc(2026, 1, 1, 0, 0), Quantity: 2}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("same id new qty: %v", err)
	}
	// 空档事件被拒不消耗标识：同标识之后落在订阅期间仍可上报。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "gap", At: utc(2026, 3, 1, 12, 0), Quantity: 1}); err != nil {
		t.Fatalf("rejected gap id reused in segment: %v", err)
	}
}

// TestBackfillAfterEndAndSuspension 终止后补报旧期间未出账用量；
// 到期欠费时仍拒绝新增，但相同重报成功。
func TestBackfillAfterEndAndSuspension(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 0, 0, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	old := Event{AccountID: "u", EventID: "old", At: utc(2026, 1, 10, 0, 0), Quantity: 1}
	if _, err := s.RecordEvent(old); err != nil {
		t.Fatal(err)
	}
	mustCancel(t, s, "u") // end 2/1

	// 终止后补报旧订阅期间、尚未出账的用量。
	clk.t = utc(2026, 2, 10, 0, 0)
	if r, err := s.RecordEvent(Event{AccountID: "u", EventID: "late", At: utc(2026, 1, 20, 0, 0), Quantity: 2}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("backfill after end: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 3 {
		t.Fatalf("backfilled usage = %d, want 3", u.Total)
	}

	// 出账后补报被拒，但已接受事件重报成功。
	b := mustBill(t, s, "u", jan(2026))
	if b.TotalUsage != 3 {
		t.Fatalf("bill usage = %d, want 3", b.TotalUsage)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "x", At: utc(2026, 1, 21, 0, 0), Quantity: 1}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("backfill billed month: %v", err)
	}
	if r, err := s.RecordEvent(old); err != nil || r.Accepted {
		t.Fatalf("replay after billed+cancel: %+v %v", r, err)
	}

	// 到期欠费时即使补报旧期间也拒绝新增。
	mustAccount(t, s, "v")
	mustSubscribe(t, s, "v", "a", utc(2026, 1, 1, 0, 0))
	vOld := Event{AccountID: "v", EventID: "old", At: utc(2026, 1, 5, 0, 0), Quantity: 1}
	if _, err := s.RecordEvent(vOld); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
	mustBill(t, s, "v", jan(2026))
	mustCancel(t, s, "v") // end 3/1，2 月仍在订阅覆盖内但尚未出账
	clk.t = utc(2026, 2, 8, 0, 0)
	st, _ := s.Status("v")
	if !st.Suspended {
		t.Fatal("v should be suspended")
	}
	// 覆盖内、未出账的 2 月新事件：到期欠费仍拒绝。
	if _, err := s.RecordEvent(Event{AccountID: "v", EventID: "late", At: utc(2026, 2, 3, 0, 0), Quantity: 1}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("new event while suspended: %v", err)
	}
	if r, err := s.RecordEvent(vOld); err != nil || r.Accepted {
		t.Fatalf("replay while suspended: %+v %v", r, err)
	}
}

// TestBillingAcrossCancellationAndResubscribe 取消不自动出账、空档月份失败、
// 各月按当时锁定条件出账，支持多次取消与重新开通且旧月份不被覆盖。
func TestBillingAcrossCancellationAndResubscribe(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 月费不同便于识别条件：a=1000，c=3000，额度均为 0、单价 0、税 0。
	mustPlan(t, s, planDef("a", 1000, 0, 0, 0))
	mustPlan(t, s, planDef("c", 3000, 0, 0, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 段一：1 月；1/15 取消，2/1 终止。
	mustCancel(t, s, "u")
	clk.t = utc(2026, 2, 1, 0, 0)
	// 取消不自动出账。
	if _, err := s.GetBill("u", jan(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("auto-bill on cancel: %v", err)
	}
	// 旧订阅覆盖的已结束月份仍可出账，用当时锁定条件。
	janBill := mustBill(t, s, "u", jan(2026))
	if janBill.Terms.PlanID != "a" || janBill.MonthlyFee != 1000 {
		t.Fatalf("jan bill: %+v", janBill)
	}

	// 段二：3/1 重新开通（用新套餐），3/15 再取消，4/1 终止。
	mustSubscribe(t, s, "u", "c", utc(2026, 3, 1, 0, 0))
	clk.t = utc(2026, 3, 15, 0, 0)
	mustCancel(t, s, "u")
	clk.t = utc(2026, 4, 1, 0, 0)

	// 重开当月仍收完整月费；3 月账单按新段条件，1 月账单不被覆盖。
	marBill := mustBill(t, s, "u", mar(2026))
	if marBill.Terms.PlanID != "c" || marBill.MonthlyFee != 3000 {
		t.Fatalf("mar bill: %+v", marBill)
	}
	gotJan, _ := s.GetBill("u", jan(2026))
	if gotJan.MonthlyFee != 1000 || gotJan.Terms.PlanID != "a" {
		t.Fatalf("jan bill overwritten: %+v", gotJan)
	}
	// 段间空档月份（2 月，已结束）出账失败。
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("bill feb gap: %v", err)
	}
	// 终止后的 4 月等其结束后同样出账失败。
	clk.t = utc(2026, 5, 1, 0, 0)
	if _, err := s.CreateBill("u", apr(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("bill apr gap: %v", err)
	}

	// 后来修改套餐定义，已出账金额与税额不变。
	if err := s.UpdatePlan(planDef("a", 9999, 0, 0, 1000)); err != nil {
		t.Fatal(err)
	}
	gotJan, _ = s.GetBill("u", jan(2026))
	if gotJan.MonthlyFee != 1000 || gotJan.TotalDue != 1000 {
		t.Fatalf("jan bill changed after plan update: %+v", gotJan)
	}
}

// TestResubscribeRules 重新开通的时刻限制、快照、当月完整条款，
// 以及事件/付款标识不重置。
func TestResubscribeRules(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 0))
	mustPlan(t, s, planDef("c", 3000, 30, 9, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "keep", At: utc(2026, 1, 10, 0, 0), Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	mustCancel(t, s, "u") // end 2/1
	clk.t = utc(2026, 2, 1, 0, 0)

	// 开通时刻早于上次终止时刻：拒绝。
	if err := s.Subscribe("u", "c", utc(2026, 1, 31, 23, 59)); !errors.Is(err, ErrResubscribeBeforeEnd) {
		t.Fatalf("resubscribe before end: %v", err)
	}
	// 失败不留部分变更。
	st, _ := s.Status("u")
	if st.Subscribed {
		t.Fatalf("partial subscription after failed resubscribe")
	}
	// 恰为上次终止时刻：允许，保存重新开通时的套餐条件。
	mustSubscribe(t, s, "u", "c", utc(2026, 2, 1, 0, 0))
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms.PlanID != "c" || st.CurrentTerms.MonthlyFee != 3000 {
		t.Fatalf("status after resubscribe: %+v", st)
	}
	// 有尚未终止的订阅时拒绝再次开通。
	if err := s.Subscribe("u", "a", utc(2026, 2, 1, 0, 0)); !errors.Is(err, ErrSubscriptionExists) {
		t.Fatalf("dup resubscribe: %v", err)
	}

	// 事件标识不随重新开通重置：旧标识冲突规则依旧。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "keep", At: utc(2026, 2, 5, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("event id reset by resubscribe: %v", err)
	}

	// 付款标识不随重新开通重置。
	janBill := mustBill(t, s, "u", jan(2026))
	if _, err := s.RecordPayment("u", "pay1", jan(2026), janBill.TotalDue); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 3, 1, 0, 0)
	febBill := mustBill(t, s, "u", feb(2026))
	if febBill.MonthlyFee != 3000 {
		t.Fatalf("feb (resubscribe month) bill = %+v", febBill)
	}
	if _, err := s.RecordPayment("u", "pay1", feb(2026), 1); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("payment id reset by resubscribe: %v", err)
	}
}

// TestResubscribeWithOldDebt 重新开通不清旧欠费；欠费停用规则照旧。
func TestResubscribeWithOldDebt(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 0, 0, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	clk.t = utc(2026, 2, 1, 0, 0)
	mustBill(t, s, "u", jan(2026))
	clk.t = utc(2026, 2, 8, 0, 0)
	if st, _ := s.Status("u"); !st.Suspended {
		t.Fatal("should be suspended")
	}
	mustCancel(t, s, "u") // end 3/1
	clk.t = utc(2026, 3, 1, 0, 0)

	// 带着旧欠费重新开通：允许，但停用状态依旧，新用量仍被拒绝。
	mustSubscribe(t, s, "u", "a", utc(2026, 3, 1, 0, 0))
	st, _ := s.Status("u")
	if !st.Subscribed || !st.Suspended {
		t.Fatalf("resubscribe cleared debt: %+v", st)
	}
	clk.t = utc(2026, 3, 2, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "m", At: utc(2026, 3, 1, 12, 0), Quantity: 1}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("new usage with old debt: %v", err)
	}
	// 结清旧债只解除停用，订阅保持有效（不改变其生命周期）。
	if _, err := s.RecordPayment("u", "p", jan(2026), 1000); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("u")
	if st.Suspended || !st.Subscribed {
		t.Fatalf("status after settling old debt: %+v", st)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "m", At: utc(2026, 3, 1, 12, 0), Quantity: 1}); err != nil {
		t.Fatalf("usage after debt cleared: %v", err)
	}
}

// TestConcurrentCancellation 并发重复取消只有一次生效，终止时刻一致；
// 取消/撤回/换套餐/用量/出账混跑后状态自洽，账单金额内部一致。
func TestConcurrentCancellation(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 5, 10, 0))
	mustPlan(t, s, planDef("b", 2000, 5, 20, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	var created int
	ends := make([]time.Time, 0, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r, err := s.CancelSubscription("u")
			if err != nil {
				t.Errorf("concurrent cancel: %v", err)
				return
			}
			mu.Lock()
			if r.Cancelled {
				created++
			}
			ends = append(ends, r.Cancellation.EndAt)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("cancellations created = %d, want 1", created)
	}
	for _, e := range ends {
		if !e.Equal(utc(2026, 2, 1, 0, 0)) {
			t.Fatalf("end = %v, want 2026-02-01", e)
		}
	}

	// 撤回后再混跑取消/撤回/换套餐/撤回换套餐/用量，结果必须对应某个先后顺序。
	if err := s.UndoCancelSubscription("u"); err != nil {
		t.Fatal(err)
	}
	wg.Add(n * 4)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); _, _ = s.CancelSubscription("u") }()
		go func() { defer wg.Done(); _ = s.UndoCancelSubscription("u") }()
		go func() { defer wg.Done(); _, _ = s.SchedulePlanChange("u", "b") }()
		go func() { defer wg.Done(); _ = s.CancelPlanChange("u") }()
	}
	wg.Wait()
	st, _ := s.Status("u")
	if !st.Subscribed {
		t.Fatalf("not subscribed in January: %+v", st)
	}
	// 不变量：取消安排与换套餐安排不能同时存在。
	if st.ScheduledEnd != nil && st.PendingChange != nil {
		t.Fatalf("both cancellation and plan change pending: %+v", st)
	}

	// 1 月先补一笔用量，推进到 2 月并发出账：1 月账单永远适用 a，金额自洽。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "base", At: utc(2026, 1, 12, 0, 0), Quantity: 8}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
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
	if first.Terms.PlanID != "a" || first.MonthlyFee != 1000 || first.OverageUnits != 3 ||
		first.OverageFee != 30 || first.TotalDue != 1030 {
		t.Fatalf("jan bill inconsistent: %+v", first)
	}
}
