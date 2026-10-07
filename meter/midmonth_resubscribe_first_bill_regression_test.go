package meter

import (
	"errors"
	"testing"
)

// 本文件回归“月中取消旧订阅、跨过一个整月空档后月中重新开通，首月按完整
// 月费与完整额度出账”这一条主线，同时锁定两类判定不能互相污染：
//
//   - 用量按事件实际发生时刻判断是否落在某段订阅期间（开通计入、终止不计入），
//     月中重新开通当月虽可整月出账，开通之前的事件仍属空档；
//   - 账单按 UTC 自然月判断该月是否被某段订阅覆盖（开通当月整月计入），
//     重新开通当月按新订阅在重新开通时保存的完整条件快照出整月账单，不按
//     剩余天数缩减，也不沿用旧订阅，更不被重新开通之后修改的套餐定义覆盖。
//
// 时间线（全部 UTC）：
//
//	2026-01-01 开通旧套餐 plan-old：月费 1000、包含 10、超额单价 100、税率 10%。
//	2026-01-10 一月用量 15；2026-01-15 登记按月取消，2026-02-01 00:00 终止。
//	2026-02-01 一月账期结束即出账：应付 1650，当日全额结清，三月接收用量时
//	           不存在欠费停用。
//	2026-02 整月空档；2026-03-01 仍为空档：二月出账仍报
//	           ErrBillBeforeSubscription，查询不到二月账单。
//	2026-03-15 12:00 重新开通另一套餐 plan-new，保存当时的完整条件：
//	           月费 2000、包含 20、超额单价 50、税率 600（万分比）。
//	           随后把 plan-new 的当前定义改为 5000/5/300/1000，新订阅不受影响。
//	           恰在开通瞬间上报数量 25 的事件：首次接收、归入三月；
//	           另一条发生在 3 月 14 日的事件即便在重新开通之后提交，仍返回
//	           ErrEventBeforeSubscription，不增加三月累计。
//	2026-04-01 三月结束后首次出账：总用量 25、超额 5、超额费 250、税 135、
//	           应付 2385，条件取重新开通时保存的 2000/20/50/600 完整快照；
//	           二月仍是 ErrBillBeforeSubscription/ErrBillNotFound，一月旧账单
//	           金额与付款结果保持原样。

// TestMidMonthResubscribeFirstMonthBilledWithFullSavedTerms 端到端回归上述主线。
func TestMidMonthResubscribeFirstMonthBilledWithFullSavedTerms(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))

	oldDef := planDef("plan-old", 1000, 10, 100, 1000)
	newDefAtResubscribe := planDef("plan-new", 2000, 20, 50, 600)
	newDefUpdated := planDef("plan-new", 5000, 5, 300, 1000)
	oldTerms := termsOf(oldDef)
	newSavedTerms := termsOf(newDefAtResubscribe)
	updatedTerms := termsOf(newDefUpdated)

	mustPlan(t, s, oldDef)
	mustPlan(t, s, newDefAtResubscribe)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "plan-old", utc(2026, 1, 1, 0, 0))

	// 一月用量 15：超出旧套餐包含的 10 单位，超额 5×100=500。
	clk.t = utc(2026, 1, 10, 0, 0)
	if r, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-jan", At: utc(2026, 1, 10, 0, 0), Quantity: 15,
	}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("january usage: %+v %v", r, err)
	}

	// 一月中旬登记按月取消：终止时刻 2026-02-01 00:00，一月仍按完整月计费。
	clk.t = utc(2026, 1, 15, 12, 0)
	cancelRes := mustCancel(t, s, "u")
	if !cancelRes.Cancelled || !cancelRes.Cancellation.EndAt.Equal(utc(2026, 2, 1, 0, 0)) {
		t.Fatalf("cancel result = %+v, want end 2026-02-01", cancelRes)
	}

	// 2026-02-01 终止时刻到达：无有效订阅；一月账期恰好结束，出账并结清，
	// 保证三月接收用量时没有欠费停用。
	clk.t = utc(2026, 2, 1, 0, 0)
	if st, _ := s.Status("u"); st.Subscribed || st.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("status at termination = %+v, want unsubscribed zero terms", st)
	}
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create january bill: %v", err)
	}
	// 旧账单金额与新套餐条件明显不同，三月账单若错用旧订阅会在这里对不上：
	// 1000 + 5×100 = 1500，税 150，应付 1650。
	if janBill.Terms != oldTerms || janBill.TotalUsage != 15 ||
		janBill.IncludedUnits != 10 || janBill.OverageUnits != 5 ||
		janBill.MonthlyFee != 1000 || janBill.OverageFee != 500 ||
		janBill.Tax != 150 || janBill.TotalDue != 1650 ||
		janBill.Paid != 0 || janBill.Balance != 1650 || janBill.Settled {
		t.Fatalf("january bill initial = %+v, want plan-old 15/5/500/150/1650 unpaid", janBill)
	}
	pay, err := s.RecordPayment("u", "pay-jan", jan(2026), 1650)
	if err != nil || !pay.Registered || pay.BillBalance != 0 || !pay.Settled {
		t.Fatalf("settle january bill: %+v %v", pay, err)
	}

	// 二月整月空档：二月发生的事件不属任何订阅期间，现有错误拒绝。
	clk.t = utc(2026, 2, 15, 12, 0)
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-feb", At: utc(2026, 2, 10, 0, 0), Quantity: 1,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("february gap event = %v, want ErrEventBeforeSubscription", err)
	}

	// 三月一日仍处两段订阅之间的空档：二月账期已结束，但不能因之后重新开通
	// 而补出二月月费，也查询不到二月账单。
	clk.t = utc(2026, 3, 1, 0, 0)
	if st, _ := s.Status("u"); st.Subscribed {
		t.Fatalf("status during march gap = %+v, want unsubscribed", st)
	}
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("bill february gap = %v, want ErrBillBeforeSubscription", err)
	}
	if _, err := s.GetBill("u", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("february bill queryable in gap = %v, want ErrBillNotFound", err)
	}

	// 2026-03-15 12:00 月中重新开通另一套餐：保存此刻 plan-new 的完整条件快照。
	reopenAt := utc(2026, 3, 15, 12, 0)
	clk.t = reopenAt
	mustSubscribe(t, s, "u", "plan-new", reopenAt)
	st, _ := s.Status("u")
	if !st.Subscribed || st.Suspended {
		t.Fatalf("status at reopen = %+v, want subscribed and not suspended", st)
	}
	if st.CurrentTerms != newSavedTerms {
		t.Fatalf("terms at reopen = %+v, want saved snapshot %+v", st.CurrentTerms, newSavedTerms)
	}

	// 重新开通之后修改该套餐的当前定义：只影响之后新保存的快照，不能改写
	// 本订阅已经保存的条件。
	if err := s.UpdatePlan(newDefUpdated); err != nil {
		t.Fatalf("update plan-new: %v", err)
	}
	st, _ = s.Status("u")
	if st.CurrentTerms != newSavedTerms {
		t.Fatalf("saved terms overwritten by plan update = %+v, want %+v", st.CurrentTerms, newSavedTerms)
	}
	if st.CurrentTerms == updatedTerms {
		t.Fatalf("current terms must not equal updated definition %+v", updatedTerms)
	}

	// 恰在重新开通瞬间（开通时刻计入订阅期间）上报数量 25：首次接收、归入三月。
	atReopen := Event{
		AccountID: "u", EventID: "e-mar-at-open",
		At: reopenAt, Quantity: 25,
	}
	r, err := s.RecordEvent(atReopen)
	if err != nil || !r.Accepted || r.Period != mar(2026) {
		t.Fatalf("event exactly at reopen instant: %+v %v", r, err)
	}

	// 另一条发生在三月十四日（重新开通之前、同一个 UTC 自然月内）的新事件：
	// 提交时刻账户已经重新开通，但事件时刻不属任何实际订阅期间，必须返回现有
	// 的 ErrEventBeforeSubscription，不能因“三月可整月出账”而接收。
	beforeInSameMonth := Event{
		AccountID: "u", EventID: "e-mar-before-open",
		At: utc(2026, 3, 14, 0, 0), Quantity: 9,
	}
	if _, err := s.RecordEvent(beforeInSameMonth); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("march event before reopen = %v, want ErrEventBeforeSubscription", err)
	}
	// 被拒事件不增加三月累计，也不产生二月记录；三月只有开通瞬间的 25。
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 25 {
		t.Fatalf("march usage = %d, want 25", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 0 {
		t.Fatalf("february usage = %d, want 0", u.Total)
	}
	st, _ = s.Status("u")
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 15}) ||
		st.MonthlyUsage[1] != (Usage{Period: mar(2026), Total: 25}) {
		t.Fatalf("monthly usage after reopen = %+v, want [2026-01=15 2026-03=25]", st.MonthlyUsage)
	}

	// 拒收不消耗事件标识：同一标识改报订阅期间内、已发生的时刻应被当作全新
	// 事件接收（若拒收落库，会因时刻/数量不同报 ErrEventConflict）。数量取零，
	// 不改变三月总用量 25，后续出账仍以 25 为准。
	reused := Event{
		AccountID: "u", EventID: "e-mar-before-open",
		At: reopenAt, Quantity: 0,
	}
	r2, err := s.RecordEvent(reused)
	if err != nil || !r2.Accepted || r2.Period != mar(2026) {
		t.Fatalf("rejected id must remain usable: %+v %v", r2, err)
	}
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 25 {
		t.Fatalf("march usage after zero-quantity id reuse = %d, want 25", u.Total)
	}

	// 开通瞬间事件的原样重报仍成功但不再次累计。
	if r3, err := s.RecordEvent(atReopen); err != nil || r3.Accepted || r3.Period != mar(2026) {
		t.Fatalf("identical replay at reopen: %+v %v", r3, err)
	}
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 25 {
		t.Fatalf("march usage after replay = %d, want 25", u.Total)
	}

	// 三月结束后首次出账：整月按重新开通时保存的完整条件，不按 3/15 起的
	// 剩余天数缩减。总用量 25、超额 25-20=5、超额费 5×50=250、
	// 税 (2000+250)×600/10000=135、应付 2385；初始已付 0、余额 2385、未付清。
	clk.t = utc(2026, 4, 1, 0, 0)
	marBill, err := s.CreateBill("u", mar(2026))
	if err != nil {
		t.Fatalf("create march bill: %v", err)
	}
	if marBill.Terms != newSavedTerms {
		t.Fatalf("march bill terms = %+v, want saved snapshot %+v", marBill.Terms, newSavedTerms)
	}
	if marBill.Terms == updatedTerms {
		t.Fatalf("march bill must not use updated definition %+v", updatedTerms)
	}
	if marBill.TotalUsage != 25 || marBill.IncludedUnits != 20 ||
		marBill.OverageUnits != 5 || marBill.MonthlyFee != 2000 ||
		marBill.OverageFee != 250 || marBill.Tax != 135 || marBill.TotalDue != 2385 {
		t.Fatalf("march bill amounts = usage %d included %d over %d fee %d overFee %d tax %d due %d, want 25/20/5/2000/250/135/2385",
			marBill.TotalUsage, marBill.IncludedUnits, marBill.OverageUnits,
			marBill.MonthlyFee, marBill.OverageFee, marBill.Tax, marBill.TotalDue)
	}
	if marBill.Paid != 0 || marBill.Balance != 2385 || marBill.Settled {
		t.Fatalf("march bill initial payment state = paid %d balance %d settled %v, want 0/2385/false",
			marBill.Paid, marBill.Balance, marBill.Settled)
	}
	if !marBill.DueAt.Equal(utc(2026, 4, 8, 0, 0)) {
		t.Fatalf("march bill dueAt = %v, want 2026-04-08", marBill.DueAt)
	}

	// 重复出账得到同一张账单。
	if again, err := s.CreateBill("u", mar(2026)); err != nil || again != marBill {
		t.Fatalf("march bill not idempotent: %+v %v", again, err)
	}

	// 相邻空档月份二月在三月重新开通并出账之后，仍然不能补出月费。
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("february bill after march billed = %v, want ErrBillBeforeSubscription", err)
	}
	if _, err := s.GetBill("u", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("february bill queryable after march billed = %v, want ErrBillNotFound", err)
	}

	// 三月账单查询与账户状态中的账单摘要展示一致的应付与初始余额；
	// 账单列表只含一月与三月（空档二月不在其中），当前套餐仍是保存的快照。
	gotMar, err := s.GetBill("u", mar(2026))
	if err != nil {
		t.Fatalf("get march bill: %v", err)
	}
	if gotMar.TotalDue != 2385 || gotMar.Balance != 2385 || gotMar.Paid != 0 || gotMar.Settled {
		t.Fatalf("get march bill state = due %d paid %d balance %d settled %v, want 2385/0/2385/false",
			gotMar.TotalDue, gotMar.Paid, gotMar.Balance, gotMar.Settled)
	}
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms != newSavedTerms {
		t.Fatalf("status after march bill = %+v, want subscribed with saved snapshot", st.CurrentTerms)
	}
	if len(st.Bills) != 2 {
		t.Fatalf("status bills = %+v, want january and march only", st.Bills)
	}
	sumJan, sumMar := st.Bills[0], st.Bills[1]
	if sumJan.Period != jan(2026) || sumJan.TotalDue != 1650 || sumJan.Paid != 1650 ||
		sumJan.Balance != 0 || !sumJan.Settled {
		t.Fatalf("status january summary = %+v, want due 1650 paid 1650 settled", sumJan)
	}
	if sumMar.Period != mar(2026) || sumMar.TotalDue != 2385 || sumMar.Paid != 0 ||
		sumMar.Balance != 2385 || sumMar.Settled {
		t.Fatalf("status march summary = %+v, want due 2385 balance 2385 unpaid", sumMar)
	}

	// 旧账单固定的部分保持原样：套餐快照、用量明细、各项费用、税额、应付总额与
	// 付款截止都不被重新开通、改价或三月出账改动；付款状态则随结清而变化。
	gotJan, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatalf("get january bill: %v", err)
	}
	if gotJan.Terms != janBill.Terms || gotJan.TotalUsage != janBill.TotalUsage ||
		gotJan.IncludedUnits != janBill.IncludedUnits || gotJan.OverageUnits != janBill.OverageUnits ||
		gotJan.MonthlyFee != janBill.MonthlyFee || gotJan.OverageFee != janBill.OverageFee ||
		gotJan.Tax != janBill.Tax || gotJan.TotalDue != janBill.TotalDue ||
		!gotJan.DueAt.Equal(janBill.DueAt) {
		t.Fatalf("january bill fixed content changed:\ninitial=%+v\nget    =%+v", janBill, gotJan)
	}
	// 原付款原样重报不再次登记，返回首次登记完成时的历史结果（余额 0、已付清）。
	replay, err := s.RecordPayment("u", "pay-jan", jan(2026), 1650)
	if err != nil {
		t.Fatalf("replay january payment: %v", err)
	}
	if replay.Registered || replay.Payment != (Payment{
		AccountID: "u", PaymentID: "pay-jan", Period: jan(2026), Amount: 1650,
	}) || replay.BillBalance != 0 || !replay.Settled {
		t.Fatalf("january payment replay = %+v, want not registered historical 0/settled", replay)
	}
	gotJan, _ = s.GetBill("u", jan(2026))
	if gotJan.TotalDue != 1650 || gotJan.Paid != 1650 || gotJan.Balance != 0 || !gotJan.Settled {
		t.Fatalf("january bill after payment replay = %+v, want unchanged settled 1650", gotJan)
	}
}
