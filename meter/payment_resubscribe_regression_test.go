package meter

import (
	"errors"
	"testing"
)

// 本文件回归“取消订阅、终止后重新开通不清除账户内旧付款标识与首次登记结果”。
//
// 付款标识只在所属账户内去重，付款记录保存在账户维度：按月取消让旧订阅在
// 三月月初实际终止、随后用同一 Subscribe 入口开通另一套餐，不能清掉旧订阅
// 期间登记的付款，也不能改动首次登记完成时保存的历史结果（BillBalance、
// Settled）。正常的分次付款与按月取消行为必须同时保留。
//
// 时间线（全部 UTC）：
//
//	2026-01-01 开通套餐 plan1200：月费 1200，无税、无超额。
//	2026-02-01 为 2026-01 出账：应付 1200；先用付款标识 pay-old 登记 500，
//	           首次结果为实际登记、余额 700、尚未付清。
//	2026-02-15 登记按月取消，2026-03-01 00:00 实际终止（二月仍按完整月计费）。
//	2026-03-01 重新开通月费 2400 的另一套餐 plan2400（无税、无超额）。
//	           重新开通后用另一标识补齐一月剩余 700，一月账单结清。
//	           此后原样重报 pay-old 的 500：Registered=false，付款仍属于
//	           原账户的一月账单，BillBalance=700、Settled=false 是首次登记
//	           完成时的结果，不随后来结清而变化，也不再次增加已付金额。
//	2026-04-01 为 2026-03 出账：应付 2400。旧标识改金额（501）或改指向三月
//	           账期始终是 ErrPaymentConflict——即使三月账单尚未生成，也不能
//	           改报成账单不存在；冲突不改动两张账单与原付款结果。未使用过的
//	           新标识登记三月 500 分正常成功，一月保持结清。

// paymentResubscribeFixture 搭好账户与两份套餐：plan1200 一月已开通，
// 时钟停在 2026-02-01 00:00（一月账期恰好结束）。
func paymentResubscribeFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1200", MonthlyFee: 1200, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustPlan(t, s, Plan{ID: "plan2400", MonthlyFee: 2400, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "plan1200", utc(2026, 1, 1, 0, 0))

	clk.t = utc(2026, 2, 1, 0, 0)
	return s, clk
}

// wantBillPaymentState 校验指定账单的当前付款状态，并保证应付总额不被付款、
// 重报或重新开通改动。
func wantBillPaymentState(t *testing.T, s *Service, period Month, totalDue, paid, balance int64, settled bool) Bill {
	t.Helper()
	b, err := s.GetBill("u", period)
	if err != nil {
		t.Fatalf("get bill %s: %v", period, err)
	}
	if b.TotalDue != totalDue || b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill %s state: due=%d paid=%d balance=%d settled=%v, want due=%d paid=%d balance=%d settled=%v",
			period, b.TotalDue, b.Paid, b.Balance, b.Settled, totalDue, paid, balance, settled)
	}
	return b
}

// TestPaymentHistorySurvivesCancelResubscribe 端到端回归：同一账户取消旧订阅、
// 终止后重新开通另一套餐，旧付款标识与首次登记结果必须原样保留。
func TestPaymentHistorySurvivesCancelResubscribe(t *testing.T) {
	s, clk := paymentResubscribeFixture(t)

	plan1200Terms := termsOf(Plan{ID: "plan1200", MonthlyFee: 1200})
	plan2400Terms := termsOf(Plan{ID: "plan2400", MonthlyFee: 2400})

	// 一月账单应付 1200：无超额、无税。
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	if janBill.TotalDue != 1200 || janBill.Paid != 0 || janBill.Balance != 1200 || janBill.Settled {
		t.Fatalf("jan bill initial state: %+v", janBill)
	}

	// 分次付款第一笔：pay-old 登记 500，首次实际登记，余额 700，未付清。
	first, err := s.RecordPayment("u", "pay-old", jan(2026), 500)
	if err != nil {
		t.Fatalf("first payment 500: %v", err)
	}
	if !first.Registered || first.Payment.AccountID != "u" || first.Payment.PaymentID != "pay-old" ||
		first.Payment.Period != jan(2026) || first.Payment.Amount != 500 {
		t.Fatalf("first payment content/registered: %+v", first)
	}
	if first.BillBalance != 700 || first.Settled {
		t.Fatalf("first payment result: balance=%d settled=%v, want 700/false",
			first.BillBalance, first.Settled)
	}
	wantBillPaymentState(t, s, jan(2026), 1200, 500, 700, false)

	// 二月中旬登记按月取消：终止时刻为三月月初零点，当月仍按完整月计费。
	clk.t = utc(2026, 2, 15, 12, 0)
	cancelRes := mustCancel(t, s, "u")
	if !cancelRes.Cancelled || !cancelRes.Cancellation.EndAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("cancel result = %+v, want end 2026-03-01", cancelRes)
	}

	// 三月月初终止已实际发生：无有效订阅、无待取消时刻；历史账单继续可查，
	// 旧付款结果此刻也仍然保留。
	clk.t = utc(2026, 3, 1, 0, 0)
	gapStatus, err := s.Status("u")
	if err != nil {
		t.Fatalf("status at termination: %v", err)
	}
	if gapStatus.Subscribed || gapStatus.ScheduledEnd != nil || gapStatus.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("status after termination: %+v", gapStatus)
	}
	wantBillPaymentState(t, s, jan(2026), 1200, 500, 700, false)

	// 同一入口重新开通月费 2400 的另一套餐：新订阅保存自己的开通快照。
	mustSubscribe(t, s, "u", "plan2400", utc(2026, 3, 1, 0, 0))
	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status after resubscribe: %v", err)
	}
	if !st.Subscribed || st.CurrentTerms != plan2400Terms {
		t.Fatalf("current terms after resubscribe = %+v, want plan2400", st.CurrentTerms)
	}

	// 重新开通后用另一标识补齐一月剩余 700：分次付款的第二笔正常登记并结清。
	rest, err := s.RecordPayment("u", "pay-rest", jan(2026), 700)
	if err != nil {
		t.Fatalf("pay remaining 700 after resubscribe: %v", err)
	}
	if !rest.Registered || rest.Payment.PaymentID != "pay-rest" || rest.Payment.Amount != 700 ||
		rest.BillBalance != 0 || !rest.Settled {
		t.Fatalf("remaining payment result: %+v", rest)
	}

	// 原样重报最早的 500：成功但本次不再登记；付款仍属于原账户的一月账单。
	// BillBalance=700、Settled=false 是首次登记完成时的历史结果，不能改成
	// 后来结清时的 0/true，也不能再次增加已付金额。
	replay, err := s.RecordPayment("u", "pay-old", jan(2026), 500)
	if err != nil {
		t.Fatalf("verbatim replay after settle: %v", err)
	}
	if replay.Registered {
		t.Fatalf("replay must not register again: %+v", replay)
	}
	if replay.Payment != (Payment{AccountID: "u", PaymentID: "pay-old", Period: jan(2026), Amount: 500}) {
		t.Fatalf("replay payment content changed: %+v", replay.Payment)
	}
	if replay.BillBalance != 700 || replay.Settled {
		t.Fatalf("replay historical result: balance=%d settled=%v, want first-time 700/false",
			replay.BillBalance, replay.Settled)
	}

	// 一月账单当前状态：累计已付 1200、余额为零、已经付清；应付仍是旧套餐
	// 的 1200，条款仍是旧套餐快照，不受新套餐影响。
	janNow := wantBillPaymentState(t, s, jan(2026), 1200, 1200, 0, true)
	if janNow.Terms != plan1200Terms {
		t.Fatalf("jan bill terms = %+v, want plan1200 snapshot", janNow.Terms)
	}

	// Status 的账单摘要与 GetBill 同源，反映查询时点：一月已付 1200、结清。
	// 重报不能把已结清账单恢复成欠款；当前生效套餐仍是重新开通的 plan2400。
	st, err = s.Status("u")
	if err != nil {
		t.Fatalf("status after replay: %v", err)
	}
	if !st.Subscribed || st.CurrentTerms != plan2400Terms {
		t.Fatalf("current terms changed by replay: %+v", st.CurrentTerms)
	}
	if st.Suspended {
		t.Fatalf("settled account must not be suspended: %+v", st.Bills)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) ||
		st.Bills[0].TotalDue != 1200 || st.Bills[0].Paid != 1200 ||
		st.Bills[0].Balance != 0 || !st.Bills[0].Settled {
		t.Fatalf("status jan summary after replay: %+v", st.Bills)
	}

	// 旧标识不能拿来登记新付款：保持一月账期但金额改成 501，冲突。
	if _, err := s.RecordPayment("u", "pay-old", jan(2026), 501); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("old id with changed amount: %v", err)
	}
	// 保持 500 但改指向三月账期：同样冲突。此时三月尚未出账，
	// 也不能改报成 ErrBillNotFound。
	if _, err := s.RecordPayment("u", "pay-old", mar(2026), 500); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("old id repointed to unbilled march: %v", err)
	} else if errors.Is(err, ErrBillNotFound) {
		t.Fatalf("old id repointed to unbilled march must be conflict, got bill not found")
	}

	// 冲突不消耗标识、不改数据：原样重报仍返回首次结果，一月仍结清，
	// 重新开通生效的套餐不变。
	replay2, err := s.RecordPayment("u", "pay-old", jan(2026), 500)
	if err != nil || replay2.Registered || replay2.BillBalance != 700 || replay2.Settled {
		t.Fatalf("replay after conflicts: %+v %v", replay2, err)
	}
	wantBillPaymentState(t, s, jan(2026), 1200, 1200, 0, true)

	// 四月月初为三月出账：按重新开通的 plan2400 计 2400，一月金额不受影响。
	clk.t = utc(2026, 4, 1, 0, 0)
	marBill, err := s.CreateBill("u", mar(2026))
	if err != nil {
		t.Fatalf("create march bill: %v", err)
	}
	if marBill.TotalDue != 2400 || marBill.Paid != 0 || marBill.Balance != 2400 || marBill.Settled {
		t.Fatalf("march bill initial state: %+v", marBill)
	}
	if marBill.Terms != plan2400Terms {
		t.Fatalf("march bill terms = %+v, want plan2400 snapshot", marBill.Terms)
	}

	// 三月账单生成后沿用旧标识：改金额、改账期两种方式仍都冲突。
	if _, err := s.RecordPayment("u", "pay-old", jan(2026), 501); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("old id changed amount after march billed: %v", err)
	}
	if _, err := s.RecordPayment("u", "pay-old", mar(2026), 500); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("old id repointed to billed march: %v", err)
	}

	// 两张账单与原付款结果均保持冲突前的样子。
	wantBillPaymentState(t, s, jan(2026), 1200, 1200, 0, true)
	wantBillPaymentState(t, s, mar(2026), 2400, 0, 2400, false)
	replay3, err := s.RecordPayment("u", "pay-old", jan(2026), 500)
	if err != nil || replay3.Registered || replay3.Payment.Amount != 500 ||
		replay3.Payment.Period != jan(2026) || replay3.BillBalance != 700 || replay3.Settled {
		t.Fatalf("original payment result after march conflicts: %+v %v", replay3, err)
	}

	// 换用未使用过的标识登记三月 500：正常成功，三月已付 500、余额 1900。
	marPay, err := s.RecordPayment("u", "pay-mar-new", mar(2026), 500)
	if err != nil {
		t.Fatalf("fresh id march payment 500: %v", err)
	}
	if !marPay.Registered || marPay.Payment.PaymentID != "pay-mar-new" ||
		marPay.Payment.Period != mar(2026) || marPay.Payment.Amount != 500 ||
		marPay.BillBalance != 1900 || marPay.Settled {
		t.Fatalf("fresh march payment result: %+v", marPay)
	}
	wantBillPaymentState(t, s, mar(2026), 2400, 500, 1900, false)
	// 一月仍保持结清，不被三月付款或任何重报/冲突改动。
	wantBillPaymentState(t, s, jan(2026), 1200, 1200, 0, true)

	// Status 同时给出两张账单摘要（按账期排列）与重新开通后生效的套餐。
	st, err = s.Status("u")
	if err != nil {
		t.Fatalf("final status: %v", err)
	}
	if !st.Subscribed || st.CurrentTerms != plan2400Terms {
		t.Fatalf("final current terms = %+v, want plan2400", st.CurrentTerms)
	}
	wantSummaries := []struct {
		period         Month
		due, paid, bal int64
		settled        bool
	}{
		{jan(2026), 1200, 1200, 0, true},
		{mar(2026), 2400, 500, 1900, false},
	}
	if len(st.Bills) != len(wantSummaries) {
		t.Fatalf("final status bills = %+v, want %d summaries", st.Bills, len(wantSummaries))
	}
	for i, w := range wantSummaries {
		got := st.Bills[i]
		if got.Period != w.period || got.TotalDue != w.due || got.Paid != w.paid ||
			got.Balance != w.bal || got.Settled != w.settled {
			t.Fatalf("status bills[%d] = %+v, want period=%s due=%d paid=%d balance=%d settled=%v",
				i, got, w.period, w.due, w.paid, w.bal, w.settled)
		}
	}

	// 补一笔边界：补齐标识 pay-rest 也只在本账户、一月、700 的原样重报时幂等，
	// 改指三月同样冲突，不影响三月账单。
	if _, err := s.RecordPayment("u", "pay-rest", mar(2026), 700); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("pay-rest repointed to march: %v", err)
	}
	wantBillPaymentState(t, s, mar(2026), 2400, 500, 1900, false)
}
