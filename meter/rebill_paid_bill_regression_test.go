package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归“已付款账单再次出账”：CreateBill 为同一账户、同一账期再次出账时，
// 必须返回已经存在的那张账单，付款状态反映再次出账请求时刻的真实记录。
// 出账后固定的是计费条件快照、用量与应付金额；已登记的付款继续有效，
// 再次出账不能把账单恢复成刚生成时的未付款状态。
//
// 时间线（全部 UTC），账单同时包含月费、超额费用与税额：
//
//	2026-01-01 开通 plan-a：月费 1000、包含 10 单位、超额单价 100、税率 10%。
//	2026-01-10 一月用量 15 单位（超额 5）。
//	2026-02-01 一月账期结束，首次出账：超额费用 500、税额 150、应付 1650，
//	           付款截止 2026-02-08 00:00。保存首次返回结果。
//	           登记付款 pay-1 400 分后再次出账：已付 400、余额 1250、未付清。
//	           再用 pay-2 结清 1250 后再次出账：已付 1650、余额 0、已付清。
//	           每次返回的账户、账期、套餐快照、额度、用量明细、各项费用、
//	           税额、应付总额与付款截止都保持首次出账时的内容；GetBill 与
//	           Status 摘要与再次出账结果一致，账户里始终只有这一张一月账单；
//	           调用方保存的首次出账结果仍是已付 0、余额 1650、未付清。
//	           结清后以新标识 pay-3 再登记 1 分：ErrPaymentExceedsBalance，
//	           再次出账仍显示原应付与已结清，不产生新的欠款或额外收款。
//	2026-02-10 登记按月取消，2026-03-01 00:00 终止时刻到达后：已生成且
//	           付过款的一月账单仍能再次出账取得，返回原账单当前付款状态，
//	           不要求账户重新开通订阅。

// wantRebillStatic 校验再次出账返回的一月账单中“出账后固定”的部分：
// 账户、账期、套餐条件快照、包含额度、用量明细、各项费用、税额、应付总额
// 与付款截止，全部保持首次出账时的内容。
func wantRebillStatic(t *testing.T, b Bill) {
	t.Helper()
	if b.AccountID != "acct-rebill" || b.Period != jan(2026) {
		t.Fatalf("bill identity: account=%q period=%s", b.AccountID, b.Period)
	}
	wantTerms := PlanTerms{
		PlanID: "plan-a", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 1000,
	}
	if b.Terms != wantTerms {
		t.Fatalf("bill terms = %+v, want plan-a snapshot %+v", b.Terms, wantTerms)
	}
	if b.TotalUsage != 15 || b.IncludedUnits != 10 || b.OverageUnits != 5 {
		t.Fatalf("bill usage: total=%d included=%d overage=%d, want 15/10/5",
			b.TotalUsage, b.IncludedUnits, b.OverageUnits)
	}
	if b.MonthlyFee != 1000 || b.OverageFee != 500 || b.Tax != 150 || b.TotalDue != 1650 {
		t.Fatalf("bill amounts: fee=%d overageFee=%d tax=%d totalDue=%d, want 1000/500/150/1650",
			b.MonthlyFee, b.OverageFee, b.Tax, b.TotalDue)
	}
	if !b.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("bill dueAt = %s, want 2026-02-08T00:00:00Z", b.DueAt.Format(time.RFC3339))
	}
}

// wantRebillPayment 校验再次出账返回的付款状态（已付、余额、是否付清）。
func wantRebillPayment(t *testing.T, b Bill, paid, balance int64, settled bool) {
	t.Helper()
	if b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill payment state: paid=%d balance=%d settled=%v, want %d/%d/%v",
			b.Paid, b.Balance, b.Settled, paid, balance, settled)
	}
}

// wantJanBillConsistent 校验同一时刻 GetBill 与 Status 中该月账单摘要的付款
// 信息与再次出账的结果一致，且账户里始终只有这一张一月账单。
func wantJanBillConsistent(t *testing.T, s *Service, rebill Bill, paid, balance int64, settled bool) {
	t.Helper()
	got, err := s.GetBill("acct-rebill", jan(2026))
	if err != nil {
		t.Fatalf("get jan bill: %v", err)
	}
	if got != rebill {
		t.Fatalf("GetBill = %+v, CreateBill rebill = %+v, must be identical", got, rebill)
	}
	st, err := s.Status("acct-rebill")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Bills) != 1 {
		t.Fatalf("status bills = %+v, want exactly the single january bill", st.Bills)
	}
	sum := st.Bills[0]
	if sum.Period != jan(2026) || sum.TotalDue != 1650 ||
		sum.Paid != paid || sum.Balance != balance || sum.Settled != settled ||
		!sum.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("status jan summary = %+v, want due=1650 paid=%d balance=%d settled=%v dueAt=2026-02-08",
			sum, paid, balance, settled)
	}
}

func TestRebillPaidBillReflectsCurrentPayments(t *testing.T) {
	// 一月中旬：账户开通 plan-a，一月累计用量 15 单位。
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID: "plan-a", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 1000,
	})
	mustAccount(t, s, "acct-rebill")
	if err := s.Subscribe("acct-rebill", "plan-a", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{
		AccountID: "acct-rebill", EventID: "e-jan",
		At: utc(2026, 1, 10, 0, 0), Quantity: 15,
	}); err != nil {
		t.Fatal(err)
	}

	// 进入二月、一月账期已经结束：首次为一月出账。超额 5×100=500，
	// 税 (1000+500)×10%=150，应付 1650，截止 2026-02-08 00:00 UTC。
	clk.t = utc(2026, 2, 1, 0, 0)
	first, err := s.CreateBill("acct-rebill", jan(2026))
	if err != nil {
		t.Fatalf("first bill: %v", err)
	}
	wantRebillStatic(t, first)
	wantRebillPayment(t, first, 0, 1650, false)

	// 首次登记 400 分付款后再次出账：返回同一张账单，付款状态反映这次
	// 请求时的真实记录——已付 400、余额 1250、尚未付清；出账时固定的
	// 部分全部保持首次出账时的内容。
	p1, err := s.RecordPayment("acct-rebill", "pay-1", jan(2026), 400)
	if err != nil || !p1.Registered || p1.BillBalance != 1250 || p1.Settled {
		t.Fatalf("pay-1 400: %+v %v", p1, err)
	}
	rebill, err := s.CreateBill("acct-rebill", jan(2026))
	if err != nil {
		t.Fatalf("rebill after pay-1: %v", err)
	}
	wantRebillStatic(t, rebill)
	wantRebillPayment(t, rebill, 400, 1250, false)
	wantJanBillConsistent(t, s, rebill, 400, 1250, false)

	// 用另一付款标识结清剩余 1250 分，再次出账：已付 1650、余额零、
	// 已经付清；已经登记的付款继续有效，再次出账不能把账单恢复成
	// 刚生成时的未付款状态。
	p2, err := s.RecordPayment("acct-rebill", "pay-2", jan(2026), 1250)
	if err != nil || !p2.Registered || p2.BillBalance != 0 || !p2.Settled {
		t.Fatalf("pay-2 1250: %+v %v", p2, err)
	}
	rebill, err = s.CreateBill("acct-rebill", jan(2026))
	if err != nil {
		t.Fatalf("rebill after pay-2: %v", err)
	}
	wantRebillStatic(t, rebill)
	wantRebillPayment(t, rebill, 1650, 0, true)
	wantJanBillConsistent(t, s, rebill, 1650, 0, true)

	// 调用方此前保存的首次出账结果仍保留取得时的状态：已付零、
	// 余额 1650、未付清；后续付款与再次出账不会把那份已返回的结果
	// 改成最新状态。
	wantRebillPayment(t, first, 0, 1650, false)

	// 结清后以新的付款标识再登记 1 分：按现有超额付款规则拒绝，
	// 不能产生新的欠款或额外收款。
	if _, err := s.RecordPayment("acct-rebill", "pay-3", jan(2026), 1); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("pay-3 1 on settled bill: %v", err)
	}
	rebill, err = s.CreateBill("acct-rebill", jan(2026))
	if err != nil {
		t.Fatalf("rebill after rejected pay-3: %v", err)
	}
	wantRebillStatic(t, rebill)
	wantRebillPayment(t, rebill, 1650, 0, true)
	wantJanBillConsistent(t, s, rebill, 1650, 0, true)

	// 登记按月取消（请求时刻 2026-02-10，终止时刻 2026-03-01 00:00 UTC）。
	clk.t = utc(2026, 2, 10, 0, 0)
	cancel, err := s.CancelSubscription("acct-rebill")
	if err != nil || !cancel.Cancelled || !cancel.Cancellation.EndAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("cancel: %+v %v", cancel, err)
	}

	// 终止时刻已经到达：订阅已终止，但已生成且付过款的一月账单仍能
	// 再次出账取得，返回原账单当前付款状态，不要求账户重新开通订阅。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, err := s.Status("acct-rebill")
	if err != nil {
		t.Fatalf("status after termination: %v", err)
	}
	if st.Subscribed {
		t.Fatalf("subscription must be terminated: %+v", st)
	}
	rebill, err = s.CreateBill("acct-rebill", jan(2026))
	if err != nil {
		t.Fatalf("rebill after subscription terminated: %v", err)
	}
	wantRebillStatic(t, rebill)
	wantRebillPayment(t, rebill, 1650, 0, true)
	wantJanBillConsistent(t, s, rebill, 1650, 0, true)
	// 首次出账结果在取消与终止之后依然保持取得时的未付款状态。
	wantRebillPayment(t, first, 0, 1650, false)
}
