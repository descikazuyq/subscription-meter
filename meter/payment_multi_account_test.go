package meter

import (
	"errors"
	"testing"
)

// 本文件回归“付款标识只在所属账户内去重”。
//
// 付款标识的去重域是单个账户：同一个服务中不同账户对各自账期相同的账单
// 使用同一个付款标识，必须分别作为首次付款接受，互不干扰——即使两笔金额
// 不同、且两张账单都已过付款截止时刻。同时账户内的既有边界不能被破坏：
// 同标识改金额或改账期仍是本账户的冲突；原样重报只返回本账户首次登记时
// 的历史结果，不改变任何账户账单与停用状态的当前值。
//
// 场景：两个各有一份已生效订阅的账户 a、b，2026-01 账单应付分别为
// 1000 分与 2000 分（税额、用量均为零），付款截止时刻均为 2026-02-08。
// 当前时钟已到截止时刻，两账户都因欠费停用，订阅仍有效。

// multiAccountPaymentFixture 构造上述两个账户及其一月账单，并把时钟拨到
// 2026-02-08 00:00（恰好到达付款截止时刻），两账户均处于欠费停用状态。
func multiAccountPaymentFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustPlan(t, s, Plan{ID: "plan2000", MonthlyFee: 2000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustAccount(t, s, "b")
	if err := s.Subscribe("a", "plan1000", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe a: %v", err)
	}
	if err := s.Subscribe("b", "plan2000", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe b: %v", err)
	}

	clk.t = utc(2026, 2, 8, 0, 0)
	ba, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatalf("create bill a: %v", err)
	}
	if ba.TotalDue != 1000 || ba.Paid != 0 || ba.Balance != 1000 || ba.Settled || ba.Tax != 0 || ba.TotalUsage != 0 {
		t.Fatalf("bill a initial state: %+v", ba)
	}
	bb, err := s.CreateBill("b", jan(2026))
	if err != nil {
		t.Fatalf("create bill b: %v", err)
	}
	if bb.TotalDue != 2000 || bb.Paid != 0 || bb.Balance != 2000 || bb.Settled || bb.Tax != 0 || bb.TotalUsage != 0 {
		t.Fatalf("bill b initial state: %+v", bb)
	}
	return s, clk
}

// wantAccountBillState 校验一个账户一月账单的当前付款状态，并保证付款操作
// 不改变出账时即固定的应付总额、税额与用量明细。
func wantAccountBillState(t *testing.T, s *Service, acct string, totalDue, paid, balance int64, settled bool) {
	t.Helper()
	b, err := s.GetBill(acct, jan(2026))
	if err != nil {
		t.Fatalf("get bill %q: %v", acct, err)
	}
	if b.TotalDue != totalDue || b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill %q state: due=%d paid=%d balance=%d settled=%v, want due=%d paid=%d balance=%d settled=%v",
			acct, b.TotalDue, b.Paid, b.Balance, b.Settled, totalDue, paid, balance, settled)
	}
	// 所有付款操作都只更新付款状态：应付总额、税额与用量保持出账时的值。
	if b.MonthlyFee != totalDue || b.OverageFee != 0 || b.Tax != 0 {
		t.Fatalf("bill %q amounts changed: %+v", acct, b)
	}
	if b.TotalUsage != 0 || b.IncludedUnits != 0 || b.OverageUnits != 0 {
		t.Fatalf("bill %q usage detail changed: %+v", acct, b)
	}

	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	if len(st.Bills) != 1 {
		t.Fatalf("status %q bills = %+v", acct, st.Bills)
	}
	bs := st.Bills[0]
	if bs.Period != jan(2026) || bs.TotalDue != totalDue || bs.Paid != paid ||
		bs.Balance != balance || bs.Settled != settled {
		t.Fatalf("status %q bill summary = %+v, want due=%d paid=%d balance=%d settled=%v",
			acct, bs, totalDue, paid, balance, settled)
	}
}

// wantAccountFlags 校验账户的停用状态：欠费停用与订阅是否生效分别表示。
func wantAccountFlags(t *testing.T, s *Service, acct string, suspended bool) AccountStatus {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	if st.Suspended != suspended {
		t.Fatalf("account %q suspended = %v, want %v; bills: %+v", acct, st.Suspended, suspended, st.Bills)
	}
	// 欠费停用不影响订阅本身：两段订阅在整个场景中始终有效。
	if !st.Subscribed {
		t.Fatalf("account %q subscription must remain active: %+v", acct, st)
	}
	return st
}

// TestPaymentIDDedupScopedToAccount 覆盖跨账户复用同一付款标识的完整回归：
// 两账户各自的首次登记、状态独立性、补齐结清、历史重报与账户内冲突边界。
func TestPaymentIDDedupScopedToAccount(t *testing.T) {
	s, clk := multiAccountPaymentFixture(t)
	_ = clk

	janDue := utc(2026, 2, 8, 0, 0)

	// 起点：两账户都已到截止时刻、欠费停用，各自账单独立。
	wantAccountFlags(t, s, "a", true)
	wantAccountFlags(t, s, "b", true)
	wantAccountBillState(t, s, "a", 1000, 0, 1000, false)
	wantAccountBillState(t, s, "b", 2000, 0, 2000, false)

	// 账户 a 以共享标识 pay-shared 登记 400：首次登记成功，余额 600，未付清。
	ra, err := s.RecordPayment("a", "pay-shared", jan(2026), 400)
	if err != nil || !ra.Registered {
		t.Fatalf("a pay-shared 400: r=%+v err=%v", ra, err)
	}
	if ra.Payment.AccountID != "a" || ra.Payment.PaymentID != "pay-shared" ||
		ra.Payment.Period != jan(2026) || ra.Payment.Amount != 400 {
		t.Fatalf("a first payment content: %+v", ra.Payment)
	}
	if ra.BillBalance != 600 || ra.Settled {
		t.Fatalf("a first payment result: balance=%d settled=%v, want 600/false", ra.BillBalance, ra.Settled)
	}

	// 账户 b 用完全相同的标识登记 500（账期相同、金额不同）：
	// 必须作为 b 自己的首次付款接受，而不是重报（Registered 仍为 true），
	// 也不能因为 a 已用过该标识而返回冲突。
	rb, err := s.RecordPayment("b", "pay-shared", jan(2026), 500)
	if err != nil {
		t.Fatalf("b pay-shared 500 must be accepted as first payment: %v", err)
	}
	if !rb.Registered {
		t.Fatalf("b pay-shared must register anew, got replay: %+v", rb)
	}
	if rb.Payment.AccountID != "b" || rb.Payment.PaymentID != "pay-shared" ||
		rb.Payment.Period != jan(2026) || rb.Payment.Amount != 500 {
		t.Fatalf("b first payment content: %+v", rb.Payment)
	}
	if rb.BillBalance != 1500 || rb.Settled {
		t.Fatalf("b first payment result: balance=%d settled=%v, want 1500/false", rb.BillBalance, rb.Settled)
	}

	// 两账户查询中的已付金额、余额、停用状态各自对应自己的账单；
	// b 的成功登记不能影响 a 的记录，两账户都未付清、继续停用。
	wantAccountBillState(t, s, "a", 1000, 400, 600, false)
	wantAccountBillState(t, s, "b", 2000, 500, 1500, false)
	wantAccountFlags(t, s, "a", true)
	wantAccountFlags(t, s, "b", true)

	// 用另一标识为 a 补齐 600：a 已付清、停用解除；b 仍欠费停用。
	topUp, err := s.RecordPayment("a", "pay-topup", jan(2026), 600)
	if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
		t.Fatalf("a top-up 600: r=%+v err=%v", topUp, err)
	}
	wantAccountBillState(t, s, "a", 1000, 1000, 0, true)
	wantAccountBillState(t, s, "b", 2000, 500, 1500, false)
	wantAccountFlags(t, s, "a", false)
	wantAccountFlags(t, s, "b", true)

	// 此后原样重报前面的两笔：都成功但不再次登记，各自返回首次登记完成时
	// 的历史结果——a 仍是余额 600、未付清；b 仍是余额 1500、未付清。
	replayA, err := s.RecordPayment("a", "pay-shared", jan(2026), 400)
	if err != nil {
		t.Fatalf("replay a 400: %v", err)
	}
	if replayA.Registered || replayA.Payment.AccountID != "a" ||
		replayA.BillBalance != 600 || replayA.Settled {
		t.Fatalf("replay a historical result: %+v, want Registered=false balance=600 settled=false", replayA)
	}
	replayB, err := s.RecordPayment("b", "pay-shared", jan(2026), 500)
	if err != nil {
		t.Fatalf("replay b 500: %v", err)
	}
	if replayB.Registered || replayB.Payment.AccountID != "b" ||
		replayB.BillBalance != 1500 || replayB.Settled {
		t.Fatalf("replay b historical result: %+v, want Registered=false balance=1500 settled=false", replayB)
	}

	// 历史付款结果不能替代账单查询与账户状态中的当前结果，也不能让 a 重新
	// 停用：时钟仍停在截止时刻，a 当前已结清、未停用，b 当前仍欠费停用。
	if !clk.t.Equal(janDue) {
		t.Fatalf("clock moved unexpectedly: %v", clk.t)
	}
	wantAccountBillState(t, s, "a", 1000, 1000, 0, true)
	wantAccountBillState(t, s, "b", 2000, 500, 1500, false)
	wantAccountFlags(t, s, "a", false)
	wantAccountFlags(t, s, "b", true)

	// 账户内冲突边界仍按各自账户独立裁决：已成功标识在 a 改金额，
	// 或改指向尚未出账的另一月份，都按 a 的付款冲突拒绝。
	if _, err := s.RecordPayment("a", "pay-shared", jan(2026), 401); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a same id changed amount: %v", err)
	}
	if _, err := s.RecordPayment("a", "pay-shared", mar(2026), 400); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a same id repointed to unbilled month: %v", err)
	}
	// b 对同一标识的冲突判定同样只看 b 自己的登记（金额 500、一月）：
	// 改成 501 或指向未出账月份都是 b 的冲突，而不是命中 a 的记录。
	if _, err := s.RecordPayment("b", "pay-shared", jan(2026), 501); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("b same id changed amount: %v", err)
	}
	if _, err := s.RecordPayment("b", "pay-shared", mar(2026), 500); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("b same id repointed to unbilled month: %v", err)
	}
	// a 的补齐标识同样保留账户内冲突边界。
	if _, err := s.RecordPayment("a", "pay-topup", jan(2026), 1); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a top-up id changed amount: %v", err)
	}

	// 冲突拒绝不改变任何账户的已付金额、余额或停用状态。
	wantAccountBillState(t, s, "a", 1000, 1000, 0, true)
	wantAccountBillState(t, s, "b", 2000, 500, 1500, false)
	wantAccountFlags(t, s, "a", false)
	wantAccountFlags(t, s, "b", true)

	// b 用自己的新标识补齐剩余 1500：b 也结清并恢复，不影响 a。
	finalB, err := s.RecordPayment("b", "pay-b-final", jan(2026), 1500)
	if err != nil || !finalB.Registered || finalB.BillBalance != 0 || !finalB.Settled {
		t.Fatalf("b final 1500: r=%+v err=%v", finalB, err)
	}
	wantAccountBillState(t, s, "a", 1000, 1000, 0, true)
	wantAccountBillState(t, s, "b", 2000, 2000, 0, true)
	wantAccountFlags(t, s, "a", false)
	wantAccountFlags(t, s, "b", false)

	// 两账户都结清后重报共享标识，仍各自返回首次登记时的历史未付清结果。
	replayA2, err := s.RecordPayment("a", "pay-shared", jan(2026), 400)
	if err != nil || replayA2.Registered || replayA2.BillBalance != 600 || replayA2.Settled {
		t.Fatalf("replay a after both settled: %+v err=%v", replayA2, err)
	}
	replayB2, err := s.RecordPayment("b", "pay-shared", jan(2026), 500)
	if err != nil || replayB2.Registered || replayB2.BillBalance != 1500 || replayB2.Settled {
		t.Fatalf("replay b after both settled: %+v err=%v", replayB2, err)
	}
	wantAccountBillState(t, s, "a", 1000, 1000, 0, true)
	wantAccountBillState(t, s, "b", 2000, 2000, 0, true)
}

// TestPaymentIDReusedAcrossAccountsConcurrent 并发版本：两个账户在同一时刻
// 用同一标识对各自账单登记付款。去重判定按账户各自的临界区独立进行，两笔
// 都必须首次登记成功，不能因为同服务中的并发碰撞而把其中一笔判成重报或
// 冲突，也不能把一笔的金额记到另一个账户的账单上。
func TestPaymentIDReusedAcrossAccountsConcurrent(t *testing.T) {
	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		s, _ := newTestService(utc(2026, 2, 8, 0, 0))
		mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})
		mustPlan(t, s, Plan{ID: "plan2000", MonthlyFee: 2000, TaxRateBasisPoints: 0})
		a := "ca"
		b := "cb"
		mustAccount(t, s, a)
		mustAccount(t, s, b)
		if err := s.Subscribe(a, "plan1000", utc(2026, 1, 1, 0, 0)); err != nil {
			t.Fatalf("iter %d subscribe a: %v", iter, err)
		}
		if err := s.Subscribe(b, "plan2000", utc(2026, 1, 1, 0, 0)); err != nil {
			t.Fatalf("iter %d subscribe b: %v", iter, err)
		}
		if _, err := s.CreateBill(a, jan(2026)); err != nil {
			t.Fatalf("iter %d bill a: %v", iter, err)
		}
		if _, err := s.CreateBill(b, jan(2026)); err != nil {
			t.Fatalf("iter %d bill b: %v", iter, err)
		}

		type res struct {
			r   PaymentResult
			err error
		}
		resA, resB := make(chan res, 1), make(chan res, 1)
		start := make(chan struct{})
		go func() {
			<-start
			r, err := s.RecordPayment(a, "shared", jan(2026), 400)
			resA <- res{r, err}
		}()
		go func() {
			<-start
			r, err := s.RecordPayment(b, "shared", jan(2026), 500)
			resB <- res{r, err}
		}()
		close(start)
		ra := <-resA
		rb := <-resB

		if ra.err != nil || !ra.r.Registered || ra.r.Payment.AccountID != a ||
			ra.r.BillBalance != 600 || ra.r.Settled {
			t.Fatalf("iter %d concurrent a: %+v %v", iter, ra.r, ra.err)
		}
		if rb.err != nil || !rb.r.Registered || rb.r.Payment.AccountID != b ||
			rb.r.BillBalance != 1500 || rb.r.Settled {
			t.Fatalf("iter %d concurrent b: %+v %v", iter, rb.r, rb.err)
		}
		wantAccountBillState(t, s, a, 1000, 400, 600, false)
		wantAccountBillState(t, s, b, 2000, 500, 1500, false)
	}
}
