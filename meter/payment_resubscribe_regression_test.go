package meter

import (
	"errors"
	"testing"
)

// 本文件回归“付款标识与首次付款结果在取消订阅、终止后重新开通时仍然保留”。
//
// 付款标识只在所属账户内去重：旧订阅按月终止、账户用同一入口重新开通另一套餐
// 之后，账户内的旧付款记录不能随订阅更换被清除——原样重报旧付款必须仍是首次
// 登记完成那一刻的历史结果（Registered=false、当时的余额与未付清状态），不能
// 再次增加已付金额；旧标识改金额或改指向新账期一律按 ErrPaymentConflict 处理，
// 即使新账期尚未出账，也不能改报成账单不存在。正常的分次付款与按月取消行为
// 必须保持可用。
//
// 时间线（全部 UTC）：
//
//	2026-01 内 旧订阅（plan1200，月费 1200、无税、无超额）有效，
//	          一月账单应付 1200，先用 pay-old 登记 500：实际登记、
//	          余额 700、尚未付清。
//	2026-02   登记按月取消，订阅于 2026-03-01 00:00 实际终止。
//	2026-03-01 重新开通 plan2400（月费 2400），新订阅实际生效。
//	          重新开通后用 pay-rest 补齐一月剩余 700，一月账单结清。
//	此后      原样重报最早的 500 付款：成功但 Registered=false，
//	          BillBalance=700、Settled=false 仍是首次登记时的结果。
//	2026-04   旧标识指向三月：出账前冲突（而非账单不存在），出账后仍冲突；
//	          未使用的新标识可正常为三月账单登记 500。

// resubscribePaymentFixture 构造账户 acct-reup：2026-01 开通月费 1200 的套餐
// （无税、无超额）。时钟拨到 2026-02-01（一月账期恰好结束）后立即出账并登记
// 首笔 500 分——此刻旧订阅仍有效（终止时刻尚未登记），属于“首次付款发生在
// 旧订阅仍有效时”的使用情形。返回服务与时钟，后续步骤自行推进时间。
func resubscribePaymentFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1200", MonthlyFee: 1200, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustPlan(t, s, Plan{ID: "plan2400", MonthlyFee: 2400, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "acct-reup")
	mustSubscribe(t, s, "acct-reup", "plan1200", utc(2026, 1, 1, 0, 0))

	// 一月结束后立即出账：应付 1200、已付 0、余额 1200、未付清。
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill("acct-reup", jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	if b.TotalDue != 1200 || b.Paid != 0 || b.Balance != 1200 || b.Settled {
		t.Fatalf("jan bill initial state: %+v", b)
	}

	// 旧订阅仍有效期间（终止时刻为之后登记的 3/1）登记首笔 500：
	// 实际登记、余额 700、尚未付清。
	first, err := s.RecordPayment("acct-reup", "pay-old", jan(2026), 500)
	if err != nil {
		t.Fatalf("first payment 500: %v", err)
	}
	if !first.Registered || first.Payment.PaymentID != "pay-old" ||
		first.Payment.Period != jan(2026) || first.Payment.Amount != 500 {
		t.Fatalf("first payment content: %+v", first)
	}
	if first.BillBalance != 700 || first.Settled {
		t.Fatalf("first payment result: balance=%d settled=%v, want 700/false",
			first.BillBalance, first.Settled)
	}
	return s, clk
}

// wantReupBillState 校验一月账单的当前付款状态，并重报/冲突类操作不应改动
// 出账时即固定的应付金额。
func wantReupBillState(t *testing.T, s *Service, period Month, totalDue, paid, balance int64, settled bool) {
	t.Helper()
	b, err := s.GetBill("acct-reup", period)
	if err != nil {
		t.Fatalf("get bill %s: %v", period, err)
	}
	if b.TotalDue != totalDue || b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill %s state: due=%d paid=%d balance=%d settled=%v, want due=%d paid=%d balance=%d settled=%v",
			period, b.TotalDue, b.Paid, b.Balance, b.Settled, totalDue, paid, balance, settled)
	}
}

// wantReupSummary 在账户状态中找到指定账期的账单摘要并校验其当前付款状态。
func wantReupSummary(t *testing.T, st AccountStatus, period Month, totalDue, paid, balance int64, settled bool) BillSummary {
	t.Helper()
	for _, bs := range st.Bills {
		if bs.Period == period {
			if bs.TotalDue != totalDue || bs.Paid != paid || bs.Balance != balance || bs.Settled != settled {
				t.Fatalf("summary %s: due=%d paid=%d balance=%d settled=%v, want due=%d paid=%d balance=%d settled=%v",
					period, bs.TotalDue, bs.Paid, bs.Balance, bs.Settled, totalDue, paid, balance, settled)
			}
			return bs
		}
	}
	t.Fatalf("status has no summary for %s: %+v", period, st.Bills)
	return BillSummary{}
}

// TestPaymentRecordsSurviveCancelAndResubscribe 是端到端回归：旧订阅取消、
// 终止并以另一套餐重新开通后，旧付款标识与首次登记结果原样保留，分次付款、
// 按月取消与重新开通的正常行为不受影响。
func TestPaymentRecordsSurviveCancelAndResubscribe(t *testing.T) {
	s, clk := resubscribePaymentFixture(t)

	// 首次登记后一月账单：累计已付 500、余额 700、未付清。
	wantReupBillState(t, s, jan(2026), 1200, 500, 700, false)

	// 二月登记按月取消：终止时刻为 2026-03-01 00:00，二月仍按完整月计费，
	// 但本场景不为二月出账（与付款回归无关）。
	clk.t = utc(2026, 2, 15, 12, 0)
	cancelRes := mustCancel(t, s, "acct-reup")
	if !cancelRes.Cancelled || !cancelRes.Cancellation.EndAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("cancel result = %+v, want end 2026-03-01", cancelRes)
	}

	// 三月月初订阅已实际终止：无有效订阅、当前套餐为零值，历史账单仍可查。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, err := s.Status("acct-reup")
	if err != nil {
		t.Fatalf("status at termination: %v", err)
	}
	if st.Subscribed || st.CurrentTerms != (PlanTerms{}) || st.ScheduledEnd != nil {
		t.Fatalf("status after termination: %+v", st)
	}
	wantReupBillState(t, s, jan(2026), 1200, 500, 700, false)

	// 同一入口重新开通月费 2400 的另一套餐，开通时刻恰为终止时刻，立即生效；
	// 重新开通不清除账户内旧付款标识，也不改变旧账单应付金额。
	mustSubscribe(t, s, "acct-reup", "plan2400", utc(2026, 3, 1, 0, 0))
	st, _ = s.Status("acct-reup")
	if !st.Subscribed || st.CurrentTerms.PlanID != "plan2400" || st.CurrentTerms.MonthlyFee != 2400 {
		t.Fatalf("status after resubscribe: %+v", st)
	}
	wantReupBillState(t, s, jan(2026), 1200, 500, 700, false)

	// 重新开通后用另一付款标识补齐一月剩余 700：分次付款正常成功，一月结清，
	// 旧订阅账单的应付金额仍是 1200，不受新套餐月费影响。
	rest, err := s.RecordPayment("acct-reup", "pay-rest", jan(2026), 700)
	if err != nil {
		t.Fatalf("top-up 700 after resubscribe: %v", err)
	}
	if !rest.Registered || rest.Payment.PaymentID != "pay-rest" ||
		rest.Payment.Period != jan(2026) || rest.Payment.Amount != 700 ||
		rest.BillBalance != 0 || !rest.Settled {
		t.Fatalf("top-up result: %+v", rest)
	}
	wantReupBillState(t, s, jan(2026), 1200, 1200, 0, true)

	// 原样重报最早的 500 付款：成功但本次不再登记（Registered=false），
	// 付款仍属于原账户的一月账单；BillBalance=700、Settled=false 是首次登记
	// 完成时的历史结果，不能改成后来结清时的结果。
	replay, err := s.RecordPayment("acct-reup", "pay-old", jan(2026), 500)
	if err != nil {
		t.Fatalf("replay old payment after settlement: %v", err)
	}
	if replay.Registered {
		t.Fatalf("replay must not register again: %+v", replay)
	}
	if replay.Payment.AccountID != "acct-reup" || replay.Payment.PaymentID != "pay-old" ||
		replay.Payment.Period != jan(2026) || replay.Payment.Amount != 500 {
		t.Fatalf("replay payment content: %+v, want pay-old 500 on 2026-01", replay.Payment)
	}
	if replay.BillBalance != 700 || replay.Settled {
		t.Fatalf("replay historical result: balance=%d settled=%v, want 700/false (first-registration snapshot)",
			replay.BillBalance, replay.Settled)
	}

	// 历史重报不能再次增加已付金额，也不能把已结清账单恢复成欠款：
	// GetBill 与 Status 摘要都显示累计已付 1200、余额为零、已经付清。
	wantReupBillState(t, s, jan(2026), 1200, 1200, 0, true)
	st, _ = s.Status("acct-reup")
	wantReupSummary(t, st, jan(2026), 1200, 1200, 0, true)
	// 重新开通后实际生效的套餐不被任何付款操作改变。
	if !st.Subscribed || st.CurrentTerms.PlanID != "plan2400" || st.CurrentTerms.MonthlyFee != 2400 {
		t.Fatalf("current plan changed by payment replay: %+v", st.CurrentTerms)
	}

	// 旧付款标识不能拿来登记新付款：保持一月账期但金额改成 501，冲突。
	if _, err := s.RecordPayment("acct-reup", "pay-old", jan(2026), 501); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("old id with changed amount: %v", err)
	}
	// 保持 500 分但改指向三月账期：同样冲突——此时三月尚未出账，
	// 也不能改报成 ErrBillNotFound。
	if _, err := s.RecordPayment("acct-reup", "pay-old", mar(2026), 500); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("old id repointed to unbilled march: %v", err)
	}
	if _, err := s.GetBill("acct-reup", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("march bill must not exist before billing: %v", err)
	}

	// 两次冲突都不消耗标识、不改动数据：一月仍结清，原付款历史结果不变。
	wantReupBillState(t, s, jan(2026), 1200, 1200, 0, true)
	replay2, err := s.RecordPayment("acct-reup", "pay-old", jan(2026), 500)
	if err != nil || replay2.Registered || replay2.BillBalance != 700 || replay2.Settled {
		t.Fatalf("old payment snapshot after conflicts: %+v err=%v", replay2, err)
	}

	// 四月为三月账期出账：三月按重新开通后的 plan2400 计，应付 2400、未付款；
	// 一月应付仍为 1200，两张账单各自独立。
	clk.t = utc(2026, 4, 1, 0, 0)
	marBill, err := s.CreateBill("acct-reup", mar(2026))
	if err != nil {
		t.Fatalf("create march bill: %v", err)
	}
	if marBill.TotalDue != 2400 || marBill.Paid != 0 || marBill.Balance != 2400 || marBill.Settled {
		t.Fatalf("march bill initial state: %+v", marBill)
	}
	if marBill.Terms.PlanID != "plan2400" || marBill.MonthlyFee != 2400 {
		t.Fatalf("march bill terms = %+v, want plan2400 snapshot", marBill.Terms)
	}

	// 三月账单生成后，沿用旧标识（无论改金额还是保持 500 指向三月）仍应冲突，
	// 不能把旧付款记到新账单上。
	if _, err := s.RecordPayment("acct-reup", "pay-old", mar(2026), 500); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("old id repointed to billed march: %v", err)
	}
	if _, err := s.RecordPayment("acct-reup", "pay-old", mar(2026), 501); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("old id on march with changed amount: %v", err)
	}

	// 两张账单与原付款结果均保持原样。
	wantReupBillState(t, s, jan(2026), 1200, 1200, 0, true)
	wantReupBillState(t, s, mar(2026), 2400, 0, 2400, false)
	replay3, err := s.RecordPayment("acct-reup", "pay-old", jan(2026), 500)
	if err != nil || replay3.Registered || replay3.Payment.Period != jan(2026) ||
		replay3.BillBalance != 700 || replay3.Settled {
		t.Fatalf("old payment snapshot after march billing: %+v err=%v", replay3, err)
	}

	// 换用未使用过的付款标识为三月账单登记 500：正常首次登记成功，
	// 三月已付 500、余额 1900、未付清；一月仍保持结清。
	marchPay, err := s.RecordPayment("acct-reup", "pay-march", mar(2026), 500)
	if err != nil {
		t.Fatalf("march payment with fresh id: %v", err)
	}
	if !marchPay.Registered || marchPay.Payment.PaymentID != "pay-march" ||
		marchPay.Payment.Period != mar(2026) || marchPay.Payment.Amount != 500 ||
		marchPay.BillBalance != 1900 || marchPay.Settled {
		t.Fatalf("march payment result: %+v, want registered balance=1900 settled=false", marchPay)
	}
	wantReupBillState(t, s, mar(2026), 2400, 500, 1900, false)
	wantReupBillState(t, s, jan(2026), 1200, 1200, 0, true)

	// 账户状态同时展示两张账单摘要：一月累计已付 1200、余额零、已付清；
	// 三月已付 500、余额 1900、未付清；当前生效套餐仍是 plan2400。
	st, _ = s.Status("acct-reup")
	if !st.Subscribed || st.CurrentTerms.PlanID != "plan2400" || st.CurrentTerms.MonthlyFee != 2400 {
		t.Fatalf("current plan at end: %+v", st.CurrentTerms)
	}
	if len(st.Bills) != 2 {
		t.Fatalf("status bills = %+v, want jan and mar", st.Bills)
	}
	wantReupSummary(t, st, jan(2026), 1200, 1200, 0, true)
	wantReupSummary(t, st, mar(2026), 2400, 500, 1900, false)
}
