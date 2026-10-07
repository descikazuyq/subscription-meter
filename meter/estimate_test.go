package meter

import (
	"errors"
	"testing"
	"time"
)

func mustEvent(t *testing.T, s *Service, accountID, eventID string, at time.Time, qty int64) {
	t.Helper()
	if _, err := s.RecordEvent(Event{
		AccountID: accountID, EventID: eventID, At: at, Quantity: qty,
	}); err != nil {
		t.Fatalf("record event %q: %v", eventID, err)
	}
}

func mustPlanUpdate(t *testing.T, s *Service, p Plan) {
	t.Helper()
	if err := s.UpdatePlan(p); err != nil {
		t.Fatalf("update plan %q: %v", p.ID, err)
	}
}

// 月中查询当前账期预估：完整月费、完整额度、超额按快照单价、税额对
// 月费与超额费用之和四舍五入；账期结束后正式出账金额与预估一致。
func TestEstimateCurrentBillBasicAndMatchesFinalBill(t *testing.T) {
	s, c := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 3, IncludedUnits: 2,
		OveragePrice: 3, TaxRateBasisPoints: 1000,
	})
	mustAccount(t, s, "acme")
	mustSubscribe(t, s, "acme", "plan-a", utc(2026, 1, 1, 0, 0))
	mustEvent(t, s, "acme", "e1", utc(2026, 1, 5, 0, 0), 3)

	est, err := s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !est.Estimated {
		t.Fatalf("Estimated = false, want true")
	}
	if est.Period != jan(2026) {
		t.Fatalf("period = %s, want 2026-01", est.Period)
	}
	if est.Terms.PlanID != "plan-a" || est.Terms.MonthlyFee != 3 ||
		est.Terms.IncludedUnits != 2 || est.Terms.OveragePrice != 3 ||
		est.Terms.TaxRateBasisPoints != 1000 {
		t.Fatalf("terms = %+v, want plan-a snapshot", est.Terms)
	}
	// 用量 3，超额 1 单位，超额费 3 分；税前 3+3=6；税 6×10%=0.6→1；应付 7。
	if est.TotalUsage != 3 || est.IncludedUnits != 2 || est.OverageUnits != 1 {
		t.Fatalf("usage=%d included=%d overage=%d, want 3/2/1",
			est.TotalUsage, est.IncludedUnits, est.OverageUnits)
	}
	if est.MonthlyFee != 3 || est.OverageFee != 3 || est.Tax != 1 || est.TotalDue != 7 {
		t.Fatalf("fee=%d overageFee=%d tax=%d total=%d, want 3/3/1/7",
			est.MonthlyFee, est.OverageFee, est.Tax, est.TotalDue)
	}

	// 账期结束后正式出账：金额与月中预估一致。
	c.t = utc(2026, 2, 1, 0, 0)
	bill, err := s.CreateBill("acme", jan(2026))
	if err != nil {
		t.Fatalf("create bill: %v", err)
	}
	if bill.TotalUsage != est.TotalUsage || bill.OverageUnits != est.OverageUnits ||
		bill.MonthlyFee != est.MonthlyFee || bill.OverageFee != est.OverageFee ||
		bill.Tax != est.Tax || bill.TotalDue != est.TotalDue {
		t.Fatalf("bill %+v differs from estimate %+v", bill, est)
	}
}

// 没有任何用量时也能查询：累计与超额为零，月费与税额照常计算；
// 月中开通同样收完整月费、给完整额度，不按天折算。
func TestEstimateCurrentBillZeroUsageMidMonthActivation(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 5, IncludedUnits: 100,
		OveragePrice: 1, TaxRateBasisPoints: 1000,
	})
	mustAccount(t, s, "acme")
	// 1 月 10 日月中开通。
	mustSubscribe(t, s, "acme", "plan-a", utc(2026, 1, 10, 0, 0))

	est, err := s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.TotalUsage != 0 || est.OverageUnits != 0 || est.OverageFee != 0 {
		t.Fatalf("usage=%d overage=%d overageFee=%d, want all zero",
			est.TotalUsage, est.OverageUnits, est.OverageFee)
	}
	// 完整月费 5 分、完整额度 100；税 5×10%=0.5→1；应付 6。
	if est.MonthlyFee != 5 || est.IncludedUnits != 100 ||
		est.Tax != 1 || est.TotalDue != 6 {
		t.Fatalf("fee=%d included=%d tax=%d total=%d, want 5/100/1/6",
			est.MonthlyFee, est.IncludedUnits, est.Tax, est.TotalDue)
	}
}

// 尚未生效的换套餐安排不混入本月预估；到达生效月月初后，直接查询即使用
// 安排接受时锁定的条件，无需先上报用量；期间修改套餐定义不影响锁定快照。
func TestEstimateCurrentBillPlanChangeBoundary(t *testing.T) {
	s, c := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 100, IncludedUnits: 10,
		OveragePrice: 1, TaxRateBasisPoints: 0,
	})
	mustPlan(t, s, Plan{
		ID: "plan-b", MonthlyFee: 200, IncludedUnits: 20,
		OveragePrice: 2, TaxRateBasisPoints: 500,
	})
	mustAccount(t, s, "acme")
	mustSubscribe(t, s, "acme", "plan-a", utc(2026, 1, 1, 0, 0))
	mustEvent(t, s, "acme", "e1", utc(2026, 1, 5, 0, 0), 12)

	if _, err := s.SchedulePlanChange("acme", "plan-b"); err != nil {
		t.Fatalf("schedule plan change: %v", err)
	}
	// 安排后修改 plan-b 定义：锁定的安排快照不应被改写。
	mustPlanUpdate(t, s, Plan{
		ID: "plan-b", MonthlyFee: 999, IncludedUnits: 1,
		OveragePrice: 9, TaxRateBasisPoints: 9999,
	})

	// 安排尚未生效：本月预估仍按 plan-a 条件（用量 12，超额 2，超额费 2，
	// 税前 102，税率 0，应付 102）。
	est, err := s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.Terms.PlanID != "plan-a" || est.TotalDue != 102 {
		t.Fatalf("terms=%s total=%d, want plan-a/102", est.Terms.PlanID, est.TotalDue)
	}

	// 到达生效月月初：直接查询即使用安排锁定的 plan-b 条件
	// （月费 200、额度 20、单价 2、税率 500），不依赖先上报用量，
	// 也不受安排后套餐定义修改影响。本月尚无用量：税 200×5%=10，应付 210。
	c.t = utc(2026, 2, 1, 0, 0)
	est, err = s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("estimate after effective: %v", err)
	}
	if est.Period != feb(2026) {
		t.Fatalf("period = %s, want 2026-02", est.Period)
	}
	if est.Terms.PlanID != "plan-b" || est.Terms.MonthlyFee != 200 ||
		est.Terms.IncludedUnits != 20 || est.Terms.OveragePrice != 2 ||
		est.Terms.TaxRateBasisPoints != 500 {
		t.Fatalf("terms = %+v, want locked plan-b snapshot", est.Terms)
	}
	if est.TotalUsage != 0 || est.MonthlyFee != 200 || est.Tax != 10 || est.TotalDue != 210 {
		t.Fatalf("usage=%d fee=%d tax=%d total=%d, want 0/200/10/210",
			est.TotalUsage, est.MonthlyFee, est.Tax, est.TotalDue)
	}
}

// 开通后修改套餐定义不改写订阅保存的快照，预估仍按开通时条件。
func TestEstimateCurrentBillPlanUpdateDoesNotRewriteSnapshot(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 100, IncludedUnits: 10,
		OveragePrice: 1, TaxRateBasisPoints: 0,
	})
	mustAccount(t, s, "acme")
	mustSubscribe(t, s, "acme", "plan-a", utc(2026, 1, 1, 0, 0))
	mustPlanUpdate(t, s, Plan{
		ID: "plan-a", MonthlyFee: 900, IncludedUnits: 0,
		OveragePrice: 9, TaxRateBasisPoints: 10000,
	})

	est, err := s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.Terms.MonthlyFee != 100 || est.TotalDue != 100 {
		t.Fatalf("terms fee=%d total=%d, want snapshot 100/100",
			est.Terms.MonthlyFee, est.TotalDue)
	}
}

// 已登记按月取消但尚未到终止时刻：仍可查询，当月完整计费；
// 到达终止时刻后订阅终止，查询返回无订阅错误。
func TestEstimateCurrentBillWhileCancelPending(t *testing.T) {
	s, c := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 100, IncludedUnits: 10,
		OveragePrice: 1, TaxRateBasisPoints: 0,
	})
	mustAccount(t, s, "acme")
	mustSubscribe(t, s, "acme", "plan-a", utc(2026, 1, 1, 0, 0))

	if _, err := s.CancelSubscription("acme"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	est, err := s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("estimate while cancel pending: %v", err)
	}
	if est.MonthlyFee != 100 || est.TotalDue != 100 {
		t.Fatalf("fee=%d total=%d, want full month 100/100", est.MonthlyFee, est.TotalDue)
	}

	// 到达终止时刻（2 月 1 日 00:00 UTC）后订阅已终止。
	c.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.EstimateCurrentBill("acme"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("estimate after termination: %v, want ErrSubscriptionNotFound", err)
	}
}

// 因欠费暂停接收新用量但订阅仍生效的账户也可查询；查询本身不解除停用。
func TestEstimateCurrentBillWhileSuspended(t *testing.T) {
	s, c := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 100, IncludedUnits: 10,
		OveragePrice: 1, TaxRateBasisPoints: 0,
	})
	mustAccount(t, s, "acme")
	mustSubscribe(t, s, "acme", "plan-a", utc(2026, 1, 1, 0, 0))

	// 一月出账后不付款，超过付款截止（账期结束后七天）即停用。
	c.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("acme", jan(2026)); err != nil {
		t.Fatalf("create bill: %v", err)
	}
	c.t = utc(2026, 2, 9, 0, 0)
	if _, err := s.RecordEvent(Event{
		AccountID: "acme", EventID: "e1", At: utc(2026, 2, 9, 0, 0), Quantity: 1,
	}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("record while suspended: %v, want ErrSuspended", err)
	}

	// 停用期间仍可查询当前账期预估（二月无用量，完整月费）。
	est, err := s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("estimate while suspended: %v", err)
	}
	if est.Period != feb(2026) || est.TotalUsage != 0 || est.TotalDue != 100 {
		t.Fatalf("period=%s usage=%d total=%d, want 2026-02/0/100",
			est.Period, est.TotalUsage, est.TotalDue)
	}

	// 查询不解除停用。
	st, err := s.Status("acme")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Suspended {
		t.Fatalf("suspended = false after estimate, want true")
	}
}

// 查询不产生副作用：不保存正式账单、不关闭当月、不改变已有账单；
// 查询之后同月新用量仍按原规则接收，再次查询反映新增累计量。
func TestEstimateCurrentBillNoSideEffects(t *testing.T) {
	s, c := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 100, IncludedUnits: 10,
		OveragePrice: 2, TaxRateBasisPoints: 0,
	})
	mustAccount(t, s, "acme")
	// 上一段订阅已终止并留有历史账单，验证预估不改变它。
	mustSubscribe(t, s, "acme", "plan-a", utc(2025, 12, 1, 0, 0))
	mustEvent(t, s, "acme", "old", utc(2025, 12, 10, 0, 0), 1)
	c.t = utc(2026, 1, 1, 0, 0)
	if _, err := s.CancelSubscription("acme"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c.t = utc(2026, 2, 1, 0, 0)
	oldBill, err := s.CreateBill("acme", Month{Year: 2025, Month: time.December})
	if err != nil {
		t.Fatalf("create old bill: %v", err)
	}
	// 结清旧账单，避免到期欠费停用干扰后续用量接收。
	if _, err := s.RecordPayment("acme", "p1",
		Month{Year: 2025, Month: time.December}, oldBill.TotalDue); err != nil {
		t.Fatalf("pay old bill: %v", err)
	}
	if oldBill, err = s.GetBill("acme", Month{Year: 2025, Month: time.December}); err != nil {
		t.Fatalf("reload old bill: %v", err)
	}
	// 重新开通，二月生效中。
	mustSubscribe(t, s, "acme", "plan-a", utc(2026, 2, 1, 0, 0))
	c.t = utc(2026, 2, 10, 12, 0)
	mustEvent(t, s, "acme", "e1", utc(2026, 2, 2, 0, 0), 5)

	est, err := s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.TotalUsage != 5 || est.TotalDue != 100 {
		t.Fatalf("usage=%d total=%d, want 5/100", est.TotalUsage, est.TotalDue)
	}

	// 不保存正式账单、不关闭当月。
	if _, err := s.GetBill("acme", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("get bill after estimate: %v, want ErrBillNotFound", err)
	}
	if _, err := s.CreateBill("acme", feb(2026)); !errors.Is(err, ErrBillMonthNotEnded) {
		t.Fatalf("create bill current month: %v, want ErrBillMonthNotEnded", err)
	}
	// 不改变已有账单的金额与付款状态。
	got, err := s.GetBill("acme", Month{Year: 2025, Month: time.December})
	if err != nil {
		t.Fatalf("get old bill: %v", err)
	}
	if got != oldBill {
		t.Fatalf("old bill changed: %+v -> %+v", oldBill, got)
	}

	// 查询之后同月新用量仍按原规则接收，再次查询反映新增累计量。
	c.t = utc(2026, 2, 11, 12, 0)
	mustEvent(t, s, "acme", "e2", utc(2026, 2, 11, 0, 0), 8)
	est, err = s.EstimateCurrentBill("acme")
	if err != nil {
		t.Fatalf("re-estimate: %v", err)
	}
	// 累计 13，超额 3，超额费 6，应付 106。
	if est.TotalUsage != 13 || est.OverageUnits != 3 ||
		est.OverageFee != 6 || est.TotalDue != 106 {
		t.Fatalf("usage=%d overage=%d overageFee=%d total=%d, want 13/3/6/106",
			est.TotalUsage, est.OverageUnits, est.OverageFee, est.TotalDue)
	}
}

// 账户不存在、从未开通、订阅已终止、提前登记但开通时刻尚未到达：
// 按各自原因返回已有错误，不输出预估。
func TestEstimateCurrentBillErrors(t *testing.T) {
	s, c := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 100, IncludedUnits: 10,
		OveragePrice: 1, TaxRateBasisPoints: 0,
	})

	if _, err := s.EstimateCurrentBill(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty id: %v, want ErrInvalidArgument", err)
	}
	if _, err := s.EstimateCurrentBill("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("unknown account: %v, want ErrAccountNotFound", err)
	}

	// 从未开通。
	mustAccount(t, s, "never")
	if _, err := s.EstimateCurrentBill("never"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("never subscribed: %v, want ErrSubscriptionNotFound", err)
	}

	// 提前登记、开通时刻尚未到达。
	mustAccount(t, s, "future")
	mustSubscribe(t, s, "future", "plan-a", utc(2026, 2, 1, 0, 0))
	if _, err := s.EstimateCurrentBill("future"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("not activated: %v, want ErrSubscriptionNotActivated", err)
	}
	// 到达开通时刻后即可查询。
	c.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.EstimateCurrentBill("future"); err != nil {
		t.Fatalf("estimate after activation: %v", err)
	}

	// 订阅已终止。
	mustAccount(t, s, "gone")
	mustSubscribe(t, s, "gone", "plan-a", utc(2026, 1, 1, 0, 0))
	c.t = utc(2026, 1, 20, 0, 0)
	if _, err := s.CancelSubscription("gone"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.EstimateCurrentBill("gone"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("terminated: %v, want ErrSubscriptionNotFound", err)
	}
}

// 超额费用、税前合计或预计应付金额超出非负 int64 范围时返回
// ErrOverflow，不返回部分金额。
func TestEstimateCurrentBillOverflow(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))

	// 超额费用溢出：超额单价与超额用量都接近 maxInt64。
	mustPlan(t, s, Plan{
		ID: "plan-ovf-fee", MonthlyFee: 0, IncludedUnits: 0,
		OveragePrice: maxInt64, TaxRateBasisPoints: 0,
	})
	mustAccount(t, s, "ovf-fee")
	mustSubscribe(t, s, "ovf-fee", "plan-ovf-fee", utc(2026, 1, 1, 0, 0))
	mustEvent(t, s, "ovf-fee", "e1", utc(2026, 1, 2, 0, 0), maxInt64)
	if _, err := s.EstimateCurrentBill("ovf-fee"); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overage fee overflow: %v, want ErrOverflow", err)
	}

	// 税前合计溢出：月费 maxInt64，超额费 1。
	mustPlan(t, s, Plan{
		ID: "plan-ovf-sub", MonthlyFee: maxInt64, IncludedUnits: 0,
		OveragePrice: 1, TaxRateBasisPoints: 0,
	})
	mustAccount(t, s, "ovf-sub")
	mustSubscribe(t, s, "ovf-sub", "plan-ovf-sub", utc(2026, 1, 1, 0, 0))
	mustEvent(t, s, "ovf-sub", "e1", utc(2026, 1, 2, 0, 0), 1)
	if _, err := s.EstimateCurrentBill("ovf-sub"); !errors.Is(err, ErrOverflow) {
		t.Fatalf("subtotal overflow: %v, want ErrOverflow", err)
	}

	// 预计应付金额溢出：税前 maxInt64，税率 100% 使税额为正。
	mustPlan(t, s, Plan{
		ID: "plan-ovf-total", MonthlyFee: maxInt64, IncludedUnits: 0,
		OveragePrice: 0, TaxRateBasisPoints: 10000,
	})
	mustAccount(t, s, "ovf-total")
	mustSubscribe(t, s, "ovf-total", "plan-ovf-total", utc(2026, 1, 1, 0, 0))
	if _, err := s.EstimateCurrentBill("ovf-total"); !errors.Is(err, ErrOverflow) {
		t.Fatalf("total overflow: %v, want ErrOverflow", err)
	}
}
