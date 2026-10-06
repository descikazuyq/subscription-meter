package meter

import (
	"errors"
	"testing"
)

// TestRebillPaidBillKeepsAmountsAndReflectsPayments 是“已付款账单再次出账”的
// 端到端回归：CreateBill 为同一账户、同一账期再次出账时必须返回已经存在的
// 那张账单——计费条件、用量明细与各项金额保持首次出账时的内容，付款状态
// 反映再次出账请求时刻的真实记录，不能把账单恢复成刚生成时的未付款状态。
//
// 时间线（全部 UTC）：
//
//	2026-01-01 开通套餐：月费 1000、额度 10、超额单价 100、税率 10%。
//	1 月累计用量 15 单位。
//	2026-02-01 一月账期结束，首次出账：超额 5 单位、超额费 500、税 150、
//	           应付 1650，付款截止 2026-02-08 00:00 UTC。
//	之后       登记付款 p1=400，再次出账应显示已付 400、余额 1250；
//	           再用 p2 登记 1250 结清，再次出账应显示已付 1650、余额零、
//	           已付清。结清后以新付款标识 p3 再登记 1 分按超额付款拒绝。
//	2026-02-15 登记按月取消，2026-03-01 00:00 终止。
//	2026-03-10 订阅已终止，已结清的一月账单仍能再次出账取得。
func TestRebillPaidBillKeepsAmountsAndReflectsPayments(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))

	plan := Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000}
	mustPlan(t, s, plan)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "p", utc(2026, 1, 1, 0, 0))

	// 一月累计用量 15 单位：超出包含额度 10 单位的部分为 5 单位。
	clk.t = utc(2026, 1, 20, 12, 0)
	if r, err := s.RecordEvent(Event{AccountID: "u", EventID: "usage-jan", At: utc(2026, 1, 20, 0, 0), Quantity: 15}); err != nil ||
		!r.Accepted || r.Period != jan(2026) {
		t.Fatalf("jan usage: %+v %v", r, err)
	}

	// 进入二月，一月账期已结束：首次出账。
	clk.t = utc(2026, 2, 1, 0, 0)
	first, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	wantJanBillFixed(t, first)
	wantJanBillPayment(t, first, 0, 1650, false)

	// 登记首笔付款 400 分后再次出账：返回同一张账单，付款状态推进到
	// 已付 400、余额 1250、尚未付清，其余内容保持首次出账时的取值。
	if pr, err := s.RecordPayment("u", "p1", jan(2026), 400); err != nil ||
		!pr.Registered || pr.BillBalance != 1250 || pr.Settled {
		t.Fatalf("payment p1: %+v %v", pr, err)
	}
	rebill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("rebill after p1: %v", err)
	}
	wantJanBillFixed(t, rebill)
	wantJanBillPayment(t, rebill, 400, 1250, false)
	wantJanBillConsistentEverywhere(t, s, 400, 1250, false)

	// 用另一付款标识结清剩余 1250 分，再次出账：已付 1650、余额零、已付清。
	if pr, err := s.RecordPayment("u", "p2", jan(2026), 1250); err != nil ||
		!pr.Registered || pr.BillBalance != 0 || !pr.Settled {
		t.Fatalf("payment p2: %+v %v", pr, err)
	}
	rebill, err = s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("rebill after p2: %v", err)
	}
	wantJanBillFixed(t, rebill)
	wantJanBillPayment(t, rebill, 1650, 0, true)
	wantJanBillConsistentEverywhere(t, s, 1650, 0, true)

	// 调用方此前保存的首次出账结果仍保留取得时的状态：已付零、余额 1650、
	// 未付清；后续付款与再次出账不会把那份已返回的结果改成最新状态。
	wantJanBillPayment(t, first, 0, 1650, false)

	// 结清后以新的付款标识再登记 1 分：按超额付款规则拒绝，失败不消耗
	// 付款标识；再次出账仍显示原应付与已结清状态，不产生新的欠款。
	if _, err := s.RecordPayment("u", "p3", jan(2026), 1); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("overpayment after settled: %v", err)
	}
	rebill, err = s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("rebill after rejected overpayment: %v", err)
	}
	wantJanBillFixed(t, rebill)
	wantJanBillPayment(t, rebill, 1650, 0, true)
	wantJanBillConsistentEverywhere(t, s, 1650, 0, true)

	// 登记按月取消并于 3/1 零点终止后：已生成且付过款的一月账单仍能再次
	// 取得，返回原账单当前付款状态，不要求账户重新开通订阅。
	clk.t = utc(2026, 2, 15, 12, 0)
	cancelRes := mustCancel(t, s, "u")
	if !cancelRes.Cancelled || !cancelRes.Cancellation.EndAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("cancel result = %+v, want end 2026-03-01", cancelRes)
	}
	clk.t = utc(2026, 3, 10, 12, 0)
	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status after termination: %v", err)
	}
	if st.Subscribed {
		t.Fatalf("subscribed after termination: %+v", st)
	}
	rebill, err = s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("rebill after subscription ended: %v", err)
	}
	wantJanBillFixed(t, rebill)
	wantJanBillPayment(t, rebill, 1650, 0, true)
	wantJanBillConsistentEverywhere(t, s, 1650, 0, true)
}

// wantJanBillFixed 断言一月账单在出账时固定的内容：账户与账期标识、套餐
// 快照、包含额度、用量明细、各项费用、税额、应付总额与付款截止。再次出账
// 返回的账单这些字段必须与首次出账完全一致。
func wantJanBillFixed(t *testing.T, b Bill) {
	t.Helper()
	terms := PlanTerms{PlanID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000}
	if b.AccountID != "u" || b.Period != jan(2026) {
		t.Fatalf("bill identity = %s/%s, want u/2026-01", b.AccountID, b.Period)
	}
	if b.Terms != terms {
		t.Fatalf("bill terms = %+v, want %+v", b.Terms, terms)
	}
	if b.TotalUsage != 15 || b.IncludedUnits != 10 || b.OverageUnits != 5 {
		t.Fatalf("bill usage detail = total %d included %d overage %d, want 15/10/5",
			b.TotalUsage, b.IncludedUnits, b.OverageUnits)
	}
	if b.MonthlyFee != 1000 || b.OverageFee != 500 || b.Tax != 150 || b.TotalDue != 1650 {
		t.Fatalf("bill amounts = fee %d overageFee %d tax %d due %d, want 1000/500/150/1650",
			b.MonthlyFee, b.OverageFee, b.Tax, b.TotalDue)
	}
	if !b.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("bill due at = %v, want 2026-02-08T00:00:00Z", b.DueAt)
	}
}

// wantJanBillPayment 断言一月账单的付款状态：已付、余额与是否付清。
func wantJanBillPayment(t *testing.T, b Bill, paid, balance int64, settled bool) {
	t.Helper()
	if b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill payment state = paid %d balance %d settled %v, want %d/%d/%v",
			b.Paid, b.Balance, b.Settled, paid, balance, settled)
	}
}

// wantJanBillConsistentEverywhere 断言同一时刻 GetBill 与账户状态中的一月
// 账单摘要与再次出账的结果一致，且账户里始终只有这一张一月账单。
func wantJanBillConsistentEverywhere(t *testing.T, s *Service, paid, balance int64, settled bool) {
	t.Helper()
	got, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatalf("get jan bill: %v", err)
	}
	wantJanBillFixed(t, got)
	wantJanBillPayment(t, got, paid, balance, settled)

	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Bills) != 1 {
		t.Fatalf("status bills = %+v, want exactly the jan bill", st.Bills)
	}
	sum := st.Bills[0]
	if sum.Period != jan(2026) || sum.TotalDue != 1650 ||
		sum.Paid != paid || sum.Balance != balance || sum.Settled != settled {
		t.Fatalf("status bill summary = %+v, want 2026-01 due 1650 paid %d balance %d settled %v",
			sum, paid, balance, settled)
	}
	if !sum.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("status bill due at = %v, want 2026-02-08T00:00:00Z", sum.DueAt)
	}
}

