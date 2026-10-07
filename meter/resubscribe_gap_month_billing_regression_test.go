package meter

import (
	"errors"
	"testing"
)

// 本文件回归“取消后隔一个空档月、月中重新开通的首月出账”。
//
// 用量按实际订阅期间接收（事件发生时刻必须落在某段订阅的 [activatedAt, end) 内），
// 账单按 UTC 自然月计费（开通当月整月出账，不按剩余天数缩减）。重新开通的账户
// 既要按整月条件出三月账，又不能接收三月开通时刻之前的用量；两段订阅之间的
// 空档月（二月）永远不可出账，不因三月重新开通而补出月费。
//
// 时间线（全部 UTC）：
//
//	2026-01-05 开通旧套餐 plan-old：月费 1000、包含 50、超额单价 10、税率 1000 万分点。
//	2026-01-10 接收用量 5。
//	2026-01-20 登记按月取消，2026-02-01 00:00 实际终止（一月仍按完整月计费）。
//	2026-02-01 为一月出账：1000 + 税 100 = 1100，一次付清，此后无欠费停用。
//	2026-03-01 空档月检查：二月出账返回 ErrBillBeforeSubscription，查不到二月账单。
//	2026-03-15 12:00 重新开通新套餐 plan-new：月费 2000、包含 20、超额单价 50、
//	           税率 600 万分点，开通快照即三月账期的唯一计费依据。
//	           随后把 plan-new 当前定义改为 5000/5/300/1000，不影响已保存快照。
//	2026-03-15 12:00 恰在开通时刻上报用量 25：首次接收，归入三月。
//	           发生在 2026-03-14 的新事件属于空档，即使提交时已重新开通，
//	           也返回 ErrEventBeforeSubscription，不增加三月累计。
//	2026-04-01 三月结束后首次出账：总用量 25、超额 5、超额费 250、税 135、
//	           应付 2385；包含额度与月费取完整值。二月仍不可出账。
//	           三月账单查询与 Status 摘要一致，一月账单与付款结果保持原样。

// resubscribeGapFixture 搭好账户与两份套餐：plan-old 已开通并接收了一月用量 5，
// 时钟停在 2026-01-20 12:00（取消登记前）。
func resubscribeGapFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 10, 12, 0))
	// 旧套餐与新套餐的计费条件明显不同：月费、额度、单价、税率全部相异。
	mustPlan(t, s, planDef("plan-old", 1000, 50, 10, 1000))
	mustPlan(t, s, planDef("plan-new", 2000, 20, 50, 600))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "plan-old", utc(2026, 1, 5, 0, 0))
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "jan-use", At: utc(2026, 1, 10, 0, 0), Quantity: 5})

	clk.t = utc(2026, 1, 20, 12, 0)
	return s, clk
}

// TestResubscribeAfterGapMonthFirstBilling 端到端回归：月中重新开通后的首月
// 按重新开通时保存的完整条件整月出账，空档侧的事件与账单都被正确拒绝。
func TestResubscribeAfterGapMonthFirstBilling(t *testing.T) {
	s, clk := resubscribeGapFixture(t)

	oldTerms := PlanTerms{PlanID: "plan-old", MonthlyFee: 1000, IncludedUnits: 50, OveragePrice: 10, TaxRateBasisPoints: 1000}
	newTerms := PlanTerms{PlanID: "plan-new", MonthlyFee: 2000, IncludedUnits: 20, OveragePrice: 50, TaxRateBasisPoints: 600}

	// 一月中旬登记按月取消：终止时刻为二月月初零点。
	cancelRes := mustCancel(t, s, "u")
	if !cancelRes.Cancelled || !cancelRes.Cancellation.EndAt.Equal(utc(2026, 2, 1, 0, 0)) {
		t.Fatalf("cancel result = %+v, want end 2026-02-01", cancelRes)
	}

	// 二月月初旧订阅实际终止；为一月出账：月费 1000 + 税 100 = 1100，一次付清。
	clk.t = utc(2026, 2, 1, 0, 0)
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	if janBill.TotalUsage != 5 || janBill.MonthlyFee != 1000 || janBill.Tax != 100 ||
		janBill.TotalDue != 1100 || janBill.Terms != oldTerms {
		t.Fatalf("jan bill: %+v", janBill)
	}
	janPay, err := s.RecordPayment("u", "pay-jan", jan(2026), 1100)
	if err != nil || !janPay.Registered || janPay.BillBalance != 0 || !janPay.Settled {
		t.Fatalf("pay jan bill: %+v %v", janPay, err)
	}

	// 空档月（二月）结束后：整月无任何订阅覆盖，出账失败、查不到账单；
	// 账户无有效订阅，但旧账已结清，不停用。
	clk.t = utc(2026, 3, 1, 0, 0)
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("bill gap month feb: %v", err)
	}
	if _, err := s.GetBill("u", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("get feb bill in gap: %v", err)
	}
	gapStatus, err := s.Status("u")
	if err != nil {
		t.Fatalf("status in gap: %v", err)
	}
	if gapStatus.Subscribed || gapStatus.Suspended || gapStatus.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("gap status: %+v", gapStatus)
	}

	// 三月十五日十二点重新开通另一套餐：保存当时的完整条件快照。
	clk.t = utc(2026, 3, 15, 12, 0)
	mustSubscribe(t, s, "u", "plan-new", utc(2026, 3, 15, 12, 0))

	// 重新开通后修改套餐当前定义：已保存的开通快照不被改写。
	if err := s.UpdatePlan(planDef("plan-new", 5000, 5, 300, 1000)); err != nil {
		t.Fatalf("update plan-new: %v", err)
	}
	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status after resubscribe: %v", err)
	}
	if !st.Subscribed || st.CurrentTerms != newTerms {
		t.Fatalf("current terms after plan update = %+v, want resubscribe snapshot %+v", st.CurrentTerms, newTerms)
	}
	if st.Suspended {
		t.Fatalf("settled account must not be suspended: %+v", st.Bills)
	}

	// 恰在开通时刻发生的事件：首次接收并归入三月（开通时刻计入订阅期间）。
	marEvent := Event{AccountID: "u", EventID: "mar-use", At: utc(2026, 3, 15, 12, 0), Quantity: 25}
	r, err := s.RecordEvent(marEvent)
	if err != nil || !r.Accepted || r.Period != mar(2026) {
		t.Fatalf("event at activation instant: %+v %v", r, err)
	}

	// 发生在开通前、处于空档的新事件：即使提交时账户已重新开通，也按
	// 发生时刻判定，返回 ErrEventBeforeSubscription，不增加三月累计。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "gap-use", At: utc(2026, 3, 14, 10, 0), Quantity: 7}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("gap event after resubscribe: %v", err)
	}
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 25 {
		t.Fatalf("march usage after rejected gap event = %d, want 25", u.Total)
	}

	// 三月结束后首次出账：总用量 25、包含 20、超额 5、超额费 250；
	// 月费取完整 2000 不按剩余天数缩减；税 (2000+250)*6% = 135；应付 2385。
	// 条件是重新开通时保存的快照，不是旧套餐、也不是修改后的套餐定义。
	clk.t = utc(2026, 4, 1, 0, 0)
	marBill, err := s.CreateBill("u", mar(2026))
	if err != nil {
		t.Fatalf("create march bill: %v", err)
	}
	if marBill.TotalUsage != 25 || marBill.IncludedUnits != 20 || marBill.OverageUnits != 5 {
		t.Fatalf("march usage lines: %+v", marBill)
	}
	if marBill.MonthlyFee != 2000 || marBill.OverageFee != 250 || marBill.Tax != 135 || marBill.TotalDue != 2385 {
		t.Fatalf("march amounts: fee=%d over=%d tax=%d due=%d, want 2000/250/135/2385",
			marBill.MonthlyFee, marBill.OverageFee, marBill.Tax, marBill.TotalDue)
	}
	if marBill.Terms != newTerms {
		t.Fatalf("march bill terms = %+v, want resubscribe snapshot %+v", marBill.Terms, newTerms)
	}
	if marBill.Paid != 0 || marBill.Balance != 2385 || marBill.Settled {
		t.Fatalf("march bill initial payment state: %+v", marBill)
	}
	if !marBill.DueAt.Equal(utc(2026, 4, 8, 0, 0)) {
		t.Fatalf("march bill due at = %v, want 2026-04-08", marBill.DueAt)
	}

	// 相邻空档月的区别仍然保留：三月重新开通并出账后，二月依旧不可出账、
	// 查不到账单，不能补出二月月费。
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("bill gap month feb after march billed: %v", err)
	}
	if _, err := s.GetBill("u", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("get feb bill after march billed: %v", err)
	}

	// 账单查询与 Status 摘要一致地展示三月的应付与初始余额。
	got, err := s.GetBill("u", mar(2026))
	if err != nil || got != marBill {
		t.Fatalf("get march bill: %+v %v", got, err)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatalf("final status: %v", err)
	}
	if !st.Subscribed || st.CurrentTerms != newTerms {
		t.Fatalf("final current terms = %+v, want %+v", st.CurrentTerms, newTerms)
	}
	if len(st.Bills) != 2 {
		t.Fatalf("final status bills = %+v, want jan+mar", st.Bills)
	}
	if st.Bills[0].Period != jan(2026) || st.Bills[0].TotalDue != 1100 ||
		st.Bills[0].Paid != 1100 || st.Bills[0].Balance != 0 || !st.Bills[0].Settled {
		t.Fatalf("jan summary changed: %+v", st.Bills[0])
	}
	if st.Bills[1].Period != mar(2026) || st.Bills[1].TotalDue != 2385 ||
		st.Bills[1].Paid != 0 || st.Bills[1].Balance != 2385 || st.Bills[1].Settled {
		t.Fatalf("march summary = %+v, want due=2385 paid=0 balance=2385 settled=false", st.Bills[1])
	}

	// 旧账单的金额与付款结果保持原样：一月仍是旧套餐条件的 1100 且已结清，
	// 原样重报旧付款返回首次登记完成时的历史结果，不再次登记。
	janNow, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatalf("get jan bill: %v", err)
	}
	if janNow.TotalDue != 1100 || janNow.Paid != 1100 || janNow.Balance != 0 ||
		!janNow.Settled || janNow.Terms != oldTerms {
		t.Fatalf("jan bill changed after resubscribe: %+v", janNow)
	}
	replay, err := s.RecordPayment("u", "pay-jan", jan(2026), 1100)
	if err != nil || replay.Registered || replay.BillBalance != 0 || !replay.Settled {
		t.Fatalf("jan payment replay: %+v %v", replay, err)
	}
}
