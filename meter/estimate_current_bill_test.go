package meter

import (
	"errors"
	"testing"
)

// 账期内预计费用查询（EstimateCurrentBill）的回归测试。
// 预估必须：只针对当前 UTC 自然月、只统计已接收用量、沿用正式出账计价、
// 明确标为预估、纯读无副作用；待生效换套餐不提前混入；等待取消与欠费
// 停用期间可查；不可预估的情况返回各自已有错误；溢出返回 ErrOverflow。

// wantEstimate 逐项核对预估结果。
func wantEstimate(t *testing.T, e EstimatedBill, want EstimatedBill) {
	t.Helper()
	if !e.Estimated {
		t.Fatalf("estimate must be marked Estimated=true: %+v", e)
	}
	if e.AccountID != want.AccountID || e.Period != want.Period || e.Terms != want.Terms ||
		e.TotalUsage != want.TotalUsage || e.IncludedUnits != want.IncludedUnits ||
		e.OverageUnits != want.OverageUnits || e.MonthlyFee != want.MonthlyFee ||
		e.OverageFee != want.OverageFee || e.Tax != want.Tax ||
		e.EstimatedTotalDue != want.EstimatedTotalDue {
		t.Fatalf("estimate:\n got %+v\nwant %+v", e, want)
	}
}

func planTerms(id string, fee, included, overage, rate int64) PlanTerms {
	return PlanTerms{PlanID: id, MonthlyFee: fee, IncludedUnits: included,
		OveragePrice: overage, TaxRateBasisPoints: rate}
}

func TestEstimateCurrentBillPricing(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	// 月中开通：预估仍收完整月费、给完整额度，不按天折算。
	if err := s.Subscribe("a", "p", utc(2026, 1, 10, 12, 0)); err != nil {
		t.Fatal(err)
	}
	jan26 := jan(2026)
	terms := planTerms("p", 1000, 10, 100, 1000)

	// 没有任何用量也能查询：累计、超额为零，月费与税额照常（税 100）。
	e, err := s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("estimate with no usage: %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "a", Estimated: true, Period: jan26, Terms: terms,
		TotalUsage: 0, IncludedUnits: 10, OverageUnits: 0,
		MonthlyFee: 1000, OverageFee: 0, Tax: 100, EstimatedTotalDue: 1100,
	})

	// 接收 15 单位：超额 5、超额费 500、税 150、预计应付 1650。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 12, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	e, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatal(err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "a", Estimated: true, Period: jan26, Terms: terms,
		TotalUsage: 15, IncludedUnits: 10, OverageUnits: 5,
		MonthlyFee: 1000, OverageFee: 500, Tax: 150, EstimatedTotalDue: 1650,
	})

	// 账期未结束时仍不能正式出账：预估没有把当月关闭。
	if _, err := s.CreateBill("a", jan26); !errors.Is(err, ErrBillMonthNotEnded) {
		t.Fatalf("create bill mid-month: %v", err)
	}

	// 账期结束后无新增用量时，正式出账与预估各项金额一致。
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill("a", jan26)
	if err != nil {
		t.Fatal(err)
	}
	if b.Terms != e.Terms || b.TotalUsage != e.TotalUsage || b.IncludedUnits != e.IncludedUnits ||
		b.OverageUnits != e.OverageUnits || b.MonthlyFee != e.MonthlyFee ||
		b.OverageFee != e.OverageFee || b.Tax != e.Tax || b.TotalDue != e.EstimatedTotalDue {
		t.Fatalf("formal bill diverges from estimate:\n bill %+v\n est  %+v", b, e)
	}
}

func TestEstimateCurrentBillTaxRounding(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	// 与正式出账相同的舍入：不足半分舍去、恰好半分向上。
	cases := []struct {
		name      string
		fee       int64
		wantTax   int64
		wantTotal int64
	}{
		{"down", 4, 0, 4},
		{"half", 5, 1, 6},
	}
	for _, c := range cases {
		mustPlan(t, s, Plan{ID: "p-" + c.name, MonthlyFee: c.fee, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 1000})
		mustAccount(t, s, c.name)
		if err := s.Subscribe(c.name, "p-"+c.name, utc(2026, 1, 10, 0, 0)); err != nil {
			t.Fatal(err)
		}
		e, err := s.EstimateCurrentBill(c.name)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if e.Tax != c.wantTax || e.EstimatedTotalDue != c.wantTotal {
			t.Fatalf("%s: tax=%d due=%d, want tax=%d due=%d", c.name, e.Tax, e.EstimatedTotalDue, c.wantTax, c.wantTotal)
		}
	}
}

func TestEstimateCurrentBillNoSideEffects(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EstimateCurrentBill("a"); err != nil {
		t.Fatal(err)
	}

	// 不保存正式账单。
	if _, err := s.GetBill("a", jan(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("estimate saved a bill: %v", err)
	}
	// 不改变用量。
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 15 {
		t.Fatalf("usage changed after estimate: %d", u.Total)
	}
	// 不关闭当月：预估之后同月新用量照常接收，再次预估反映新增累计。
	clk.t = utc(2026, 1, 16, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e2", At: utc(2026, 1, 16, 0, 0), Quantity: 3}); err != nil {
		t.Fatalf("new usage after estimate rejected: %v", err)
	}
	e, err := s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatal(err)
	}
	if e.TotalUsage != 18 || e.OverageUnits != 8 || e.OverageFee != 800 || e.EstimatedTotalDue != 1980 {
		t.Fatalf("estimate did not reflect new usage: %+v", e)
	}
	// 重复预估结果一致、不累积任何状态。
	e2, err := s.EstimateCurrentBill("a")
	if err != nil || e2 != e {
		t.Fatalf("repeated estimate differs: %+v %v", e2, err)
	}
	// 预估不影响状态中的账单列表。
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Bills) != 0 || st.Suspended {
		t.Fatalf("estimate changed status: %+v", st)
	}

	// 跨月后一月仍未出账、可正常出账；二月预估只看二月用量（0），
	// 不把一月用量带进来。
	clk.t = utc(2026, 2, 10, 0, 0)
	febEst, err := s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatal(err)
	}
	if febEst.Period != feb(2026) || febEst.TotalUsage != 0 || febEst.OverageUnits != 0 ||
		febEst.MonthlyFee != 1000 || febEst.Tax != 100 {
		t.Fatalf("february estimate: %+v", febEst)
	}
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.TotalUsage != 18 {
		t.Fatalf("january bill usage = %d, want 18", b.TotalUsage)
	}
}

func TestEstimateCurrentBillPendingPlanChangeNotMixed(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p-a", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustPlan(t, s, Plan{ID: "p-b", MonthlyFee: 2000, IncludedUnits: 20, OveragePrice: 200, TaxRateBasisPoints: 600})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p-a", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulePlanChange("a", "p-b"); err != nil {
		t.Fatal(err)
	}
	// 安排后再修改两个套餐的定义：订阅快照与安排快照都不应被改写。
	if err := s.UpdatePlan(Plan{ID: "p-a", MonthlyFee: 9, IncludedUnits: 0, OveragePrice: 9, TaxRateBasisPoints: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePlan(Plan{ID: "p-b", MonthlyFee: 5000, IncludedUnits: 5, OveragePrice: 900, TaxRateBasisPoints: 2500}); err != nil {
		t.Fatal(err)
	}

	// 一月预估仍是旧条件，待生效安排不提前混入。
	e, err := s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatal(err)
	}
	if e.Terms != planTerms("p-a", 1000, 10, 100, 1000) || e.EstimatedTotalDue != 1650 {
		t.Fatalf("january estimate mixed pending change: %+v", e)
	}

	// 到二月月初后直接预估即用安排接受时锁定的条件，不依赖先上报用量。
	clk.t = utc(2026, 2, 1, 0, 0)
	e, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatal(err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "a", Estimated: true, Period: feb(2026),
		Terms:         planTerms("p-b", 2000, 20, 200, 600),
		TotalUsage:    0,
		IncludedUnits: 20,
		OverageUnits:  0,
		MonthlyFee:    2000,
		OverageFee:    0,
		Tax:           120,
		// 2000 × 6% = 120。
		EstimatedTotalDue: 2120,
	})

	// 二月接收 25 单位：超额按锁定单价 200 计 5 个单位，税按 6%。
	clk.t = utc(2026, 2, 10, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e2", At: utc(2026, 2, 2, 0, 0), Quantity: 25}); err != nil {
		t.Fatal(err)
	}
	e, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatal(err)
	}
	if e.TotalUsage != 25 || e.OverageUnits != 5 || e.OverageFee != 1000 || e.Tax != 180 || e.EstimatedTotalDue != 3180 {
		t.Fatalf("february estimate with usage: %+v", e)
	}
}

func TestEstimateCurrentBillWhileCancelling(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelSubscription("a"); err != nil {
		t.Fatal(err)
	}

	// 已登记按月取消但尚未到终止时刻：仍可查询，当月完整计费。
	e, err := s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("estimate while cancelling failed: %v", err)
	}
	if !e.Estimated || e.MonthlyFee != 1000 || e.IncludedUnits != 10 ||
		e.OverageFee != 500 || e.EstimatedTotalDue != 1650 {
		t.Fatalf("estimate while cancelling: %+v", e)
	}

	// 终止时刻到达后订阅已终止：返回无订阅错误，不输出预估。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.EstimateCurrentBill("a"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("estimate after cancellation end: %v", err)
	}
	// 预估失败不留账单；一月正式出账仍按完整月计。
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.TotalDue != 1650 {
		t.Fatalf("january bill after end: %+v", b)
	}
}

func TestEstimateCurrentBillWhileSuspended(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	// 二月 5 日（一月账单截止 2 月 8 日之前）先接收 5 单位二月用量。
	clk.t = utc(2026, 2, 5, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e-feb", At: utc(2026, 2, 5, 0, 0), Quantity: 5}); err != nil {
		t.Fatal(err)
	}
	// 过了截止时刻为一月出账：一月账单未付，账户欠费停用；订阅仍生效。
	clk.t = utc(2026, 2, 10, 0, 0)
	janBill, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Status("a")
	if !st.Subscribed || !st.Suspended {
		t.Fatalf("want subscribed+suspended, got %+v", st)
	}

	// 停用期间仍可查询预估，包含停用前已接收的二月累计 5。
	e, err := s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("estimate while suspended failed: %v", err)
	}
	if e.Period != feb(2026) || e.TotalUsage != 5 || e.MonthlyFee != 1000 || e.EstimatedTotalDue != 1000 {
		t.Fatalf("estimate while suspended: %+v", e)
	}
	// 查询本身不能解除停用，也不改变新用量被拒绝的规则。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e-new", At: utc(2026, 2, 10, 0, 0), Quantity: 1}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("new usage after estimate: %v", err)
	}
	st, _ = s.Status("a")
	if !st.Suspended {
		t.Fatal("estimate cleared suspension")
	}
	// 预估没有改变欠费账单。
	cur, err := s.GetBill("a", jan(2026))
	if err != nil || cur.Balance != janBill.TotalDue || cur.Settled {
		t.Fatalf("overdue bill changed: %+v %v", cur, err)
	}

	// 结清后预估照常，且二月累计仍是 5（被拒收的事件未累计）。
	if _, err := s.RecordPayment("a", "pay-jan", jan(2026), janBill.TotalDue); err != nil {
		t.Fatal(err)
	}
	e, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatal(err)
	}
	if e.TotalUsage != 5 || e.EstimatedTotalDue != 1000 {
		t.Fatalf("estimate after payment: %+v", e)
	}
}

func TestEstimateCurrentBillErrors(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "never")
	mustAccount(t, s, "future")
	mustAccount(t, s, "ended")
	mustAccount(t, s, "gap")

	if err := s.Subscribe("future", "p", utc(2026, 1, 20, 12, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe("ended", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelSubscription("ended"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.EstimateCurrentBill(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty id: %v", err)
	}
	if _, err := s.EstimateCurrentBill("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if _, err := s.EstimateCurrentBill("never"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("never subscribed: %v", err)
	}
	// 提前登记、开通时刻尚未到达：订阅尚未生效，不提前预估。
	if _, err := s.EstimateCurrentBill("future"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("future activation: %v", err)
	}
	// 开通当天零点仍未到登记的具体时刻：同样尚未生效。
	clk.t = utc(2026, 1, 20, 0, 0)
	if _, err := s.EstimateCurrentBill("future"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("activation day midnight: %v", err)
	}
	// 恰好到达开通时刻即可预估。
	clk.t = utc(2026, 1, 20, 12, 0)
	if e, err := s.EstimateCurrentBill("future"); err != nil || !e.Estimated || e.MonthlyFee != 1000 {
		t.Fatalf("estimate at activation instant: %+v %v", e, err)
	}

	// 已登记取消、到达终止时刻后订阅终止。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.EstimateCurrentBill("ended"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("ended subscription: %v", err)
	}

	// 旧订阅已终止、新订阅提前登记尚未开通：空档中按尚未生效处理。
	if err := s.Subscribe("gap", "p", utc(2026, 2, 10, 0, 0)); err != nil {
		// gap 账户没有旧订阅；直接登记未来开通即可。
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 5, 0, 0)
	if _, err := s.EstimateCurrentBill("gap"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("gap before future resubscribe: %v", err)
	}
	clk.t = utc(2026, 2, 10, 0, 0)
	if e, err := s.EstimateCurrentBill("gap"); err != nil || !e.Estimated {
		t.Fatalf("estimate after future activation: %+v %v", e, err)
	}
}

func TestEstimateCurrentBillOverflow(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))

	// 月费 maxInt64、1 单位超额即让税前合计溢出。
	mustPlan(t, s, Plan{ID: "p-sum", MonthlyFee: maxInt64, IncludedUnits: 0, OveragePrice: 1, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p-sum", utc(2026, 1, 10, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 12, 0, 0), Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EstimateCurrentBill("a"); !errors.Is(err, ErrOverflow) {
		t.Fatalf("subtotal overflow: %v", err)
	}

	// 税前合法、含税总额溢出。
	mustPlan(t, s, Plan{ID: "p-tax", MonthlyFee: 9000000000000000000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "b")
	if err := s.Subscribe("b", "p-tax", utc(2026, 1, 10, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EstimateCurrentBill("b"); !errors.Is(err, ErrOverflow) {
		t.Fatalf("total overflow: %v", err)
	}

	// 溢出时不返回部分金额、不留账单、不改用量，可重试。
	if _, err := s.GetBill("a", jan(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("overflow left a bill: %v", err)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 1 {
		t.Fatalf("usage changed: %d", u.Total)
	}
	if _, err := s.EstimateCurrentBill("a"); !errors.Is(err, ErrOverflow) {
		t.Fatal("retry estimate lost overflow")
	}

	// 计税中间乘积可超 int64，但最终金额合法时预估必须精确完成。
	mustPlan(t, s, Plan{ID: "p-large", MonthlyFee: 8000000000000000005, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "c")
	if err := s.Subscribe("c", "p-large", utc(2026, 1, 10, 0, 0)); err != nil {
		t.Fatal(err)
	}
	e, err := s.EstimateCurrentBill("c")
	if err != nil {
		t.Fatalf("large but legal estimate: %v", err)
	}
	if e.MonthlyFee != 8000000000000000005 || e.Tax != 800000000000000001 || e.EstimatedTotalDue != 8800000000000000006 {
		t.Fatalf("large estimate amounts: %+v", e)
	}

	// 时钟推进不影响上述判断的独立性（避免 unused 警告，同时验证跨月后
	// 一月仍未因预估而关闭：二月账户 c 的预估是零用量整月费）。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.GetBill("c", jan(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("january bill should not exist after estimates")
	}
}
