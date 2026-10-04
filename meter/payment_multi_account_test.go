package meter

import (
	"errors"
	"testing"
)

// 本文件回归“付款标识只在所属账户内去重”。
//
// 付款标识的去重、重报与冲突判定都以账户为边界：同一个服务中的不同账户可以
// 对各自账单复用相同的付款标识——即使账期相同、金额不同，两笔也都必须按
// 首次付款接受；某账户已用标识不能让另一账户的同名付款被判为重报或冲突。
// 完全相同的重报只返回该笔在本账户首次登记完成时的历史结果，账户间互不可见；
// 冲突边界同样只看本账户记录。所有付款操作只更新付款状态与停用状态，账单的
// 应付总额、税额与用量明细保持出账时的值不变。
//
// 夹具：两个已生效订阅的账户 a1、a2 在同一已结束月份 2026-01 分别有应付
// 1000 分与 2000 分的账单（套餐月费即应付，额度、超额单价与税率均为零，
// 无用量），当前时刻为 2026-02-08 00:00，恰是一月账单付款截止时刻，
// 两账户均欠费停用，订阅仍有效。

const (
	sharedPaymentID = "pay-shared"
	otherPaymentID  = "pay-top-up"
)

// twoOverdueAccountsFixture 建立上述两个账户及其一月账单，
// 并断言初始状态：a1 应付 1000、a2 应付 2000，均未付款、已停用。
func twoOverdueAccountsFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, planDef("plan1000", 1000, 0, 0, 0))
	mustPlan(t, s, planDef("plan2000", 2000, 0, 0, 0))
	mustAccount(t, s, "a1")
	mustAccount(t, s, "a2")
	mustSubscribe(t, s, "a1", "plan1000", utc(2026, 1, 1, 0, 0))
	mustSubscribe(t, s, "a2", "plan2000", utc(2026, 1, 1, 0, 0))

	// 拨到一月付款截止时刻：账期已结束可出账，且到期未付即停用。
	clk.t = utc(2026, 2, 8, 0, 0)
	b1 := mustBill(t, s, "a1", jan(2026))
	if b1.TotalDue != 1000 || b1.Paid != 0 || b1.Balance != 1000 || b1.Settled {
		t.Fatalf("a1 initial bill: %+v", b1)
	}
	b2 := mustBill(t, s, "a2", jan(2026))
	if b2.TotalDue != 2000 || b2.Paid != 0 || b2.Balance != 2000 || b2.Settled {
		t.Fatalf("a2 initial bill: %+v", b2)
	}
	wantAccountSuspended(t, s, "a1", true)
	wantAccountSuspended(t, s, "a2", true)
	return s, clk
}

// wantAccountBillState 断言某账户一月账单的付款状态，并核对付款操作不改写
// 出账时固定的应付总额、月费、税额与用量明细。
func wantAccountBillState(t *testing.T, s *Service, acct string, totalDue, paid, balance int64, settled bool) {
	t.Helper()
	b, err := s.GetBill(acct, jan(2026))
	if err != nil {
		t.Fatalf("get bill %q: %v", acct, err)
	}
	if b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill %q payment state: paid=%d balance=%d settled=%v, want paid=%d balance=%d settled=%v",
			acct, b.Paid, b.Balance, b.Settled, paid, balance, settled)
	}
	if b.TotalDue != totalDue || b.MonthlyFee != totalDue || b.OverageFee != 0 || b.Tax != 0 {
		t.Fatalf("bill %q amounts changed: %+v", acct, b)
	}
	if b.TotalUsage != 0 || b.IncludedUnits != 0 || b.OverageUnits != 0 {
		t.Fatalf("bill %q usage detail changed: %+v", acct, b)
	}
	if !b.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("bill %q due at = %v", acct, b.DueAt)
	}
}

// wantAccountSuspended 断言某账户的欠费停用状态，并返回完整状态供进一步核对。
func wantAccountSuspended(t *testing.T, s *Service, acct string, want bool) AccountStatus {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	if st.Suspended != want {
		t.Fatalf("account %q suspended = %v, want %v; bills %+v", acct, st.Suspended, want, st.Bills)
	}
	return st
}

// wantAccountJanSummary 在账户状态中核对一月摘要，保证查询口径与账单查询一致。
func wantAccountJanSummary(t *testing.T, st AccountStatus, acct string, totalDue, paid, balance int64, settled bool) {
	t.Helper()
	var found bool
	for _, bs := range st.Bills {
		if bs.Period != jan(2026) {
			continue
		}
		found = true
		if bs.TotalDue != totalDue || bs.Paid != paid || bs.Balance != balance || bs.Settled != settled {
			t.Fatalf("account %q jan summary = %+v, want due=%d paid=%d balance=%d settled=%v",
				acct, bs, totalDue, paid, balance, settled)
		}
	}
	if !found {
		t.Fatalf("account %q status has no jan bill: %+v", acct, st.Bills)
	}
}

// TestPaymentIDDedupScopedToAccount 按完整业务顺序锁定账户边界：
// 两账户复用同一付款标识各自首次登记部分付款；随后第一账户补齐结清；
// 再原样重报两笔原始付款，重报只返回各自的历史结果而不改当前状态；
// 最后核对账户内冲突边界与账单计量字段不变。
func TestPaymentIDDedupScopedToAccount(t *testing.T) {
	s, _ := twoOverdueAccountsFixture(t)

	// 先为 a1 以共享标识登记 400：本次实际登记，余额 600、未付清。
	r1, err := s.RecordPayment("a1", sharedPaymentID, jan(2026), 400)
	if err != nil || !r1.Registered {
		t.Fatalf("a1 first 400 payment: r=%+v err=%v", r1, err)
	}
	if r1.Payment != (Payment{AccountID: "a1", PaymentID: sharedPaymentID, Period: jan(2026), Amount: 400}) {
		t.Fatalf("a1 first payment content: %+v", r1.Payment)
	}
	if r1.BillBalance != 600 || r1.Settled {
		t.Fatalf("a1 first payment result: balance=%d settled=%v, want 600/false", r1.BillBalance, r1.Settled)
	}

	// 再为 a2 以同一标识、同一账期但不同金额登记 500：必须同样按首次付款接受，
	// 不能因为 a1 已使用该标识而返回重报（Registered=false）或冲突。
	r2, err := s.RecordPayment("a2", sharedPaymentID, jan(2026), 500)
	if err != nil {
		t.Fatalf("a2 same-id 500 payment must be accepted independently: %v", err)
	}
	if !r2.Registered {
		t.Fatalf("a2 same-id payment reported as duplicate of a1: %+v", r2)
	}
	if r2.Payment != (Payment{AccountID: "a2", PaymentID: sharedPaymentID, Period: jan(2026), Amount: 500}) {
		t.Fatalf("a2 first payment content: %+v", r2.Payment)
	}
	if r2.BillBalance != 1500 || r2.Settled {
		t.Fatalf("a2 first payment result: balance=%d settled=%v, want 1500/false", r2.BillBalance, r2.Settled)
	}

	// 两账户的账单与状态各自对应自己的账单；第二笔成功不影响 a1。
	wantAccountBillState(t, s, "a1", 1000, 400, 600, false)
	wantAccountBillState(t, s, "a2", 2000, 500, 1500, false)
	st1 := wantAccountSuspended(t, s, "a1", true)
	st2 := wantAccountSuspended(t, s, "a2", true)
	if !st1.Subscribed || !st2.Subscribed {
		t.Fatalf("subscriptions must stay active while suspended: a1=%v a2=%v", st1.Subscribed, st2.Subscribed)
	}
	wantAccountJanSummary(t, st1, "a1", 1000, 400, 600, false)
	wantAccountJanSummary(t, st2, "a2", 2000, 500, 1500, false)

	// 用另一标识为 a1 补齐 600：a1 付清、停用解除；a2 维持原状继续停用。
	topUp, err := s.RecordPayment("a1", otherPaymentID, jan(2026), 600)
	if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
		t.Fatalf("a1 top-up 600: r=%+v err=%v", topUp, err)
	}
	wantAccountBillState(t, s, "a1", 1000, 1000, 0, true)
	wantAccountBillState(t, s, "a2", 2000, 500, 1500, false)
	st1 = wantAccountSuspended(t, s, "a1", false)
	st2 = wantAccountSuspended(t, s, "a2", true)
	if !st1.Subscribed {
		t.Fatalf("a1 subscription must remain active after settling: %+v", st1)
	}
	wantAccountJanSummary(t, st1, "a1", 1000, 1000, 0, true)
	wantAccountJanSummary(t, st2, "a2", 2000, 500, 1500, false)

	// 原样重报两笔早先的部分付款：都成功但不再次登记，分别返回各自首次登记
	// 完成时的历史结果——a1 那笔仍是余额 600、未付清，a2 那笔余额 1500、未付清。
	replay1, err := s.RecordPayment("a1", sharedPaymentID, jan(2026), 400)
	if err != nil || replay1.Registered {
		t.Fatalf("a1 replay 400: r=%+v err=%v", replay1, err)
	}
	if replay1.Payment != (Payment{AccountID: "a1", PaymentID: sharedPaymentID, Period: jan(2026), Amount: 400}) {
		t.Fatalf("a1 replay payment content: %+v", replay1.Payment)
	}
	if replay1.BillBalance != 600 || replay1.Settled {
		t.Fatalf("a1 replay must return historical 600/false, got balance=%d settled=%v",
			replay1.BillBalance, replay1.Settled)
	}
	replay2, err := s.RecordPayment("a2", sharedPaymentID, jan(2026), 500)
	if err != nil || replay2.Registered {
		t.Fatalf("a2 replay 500: r=%+v err=%v", replay2, err)
	}
	if replay2.Payment != (Payment{AccountID: "a2", PaymentID: sharedPaymentID, Period: jan(2026), Amount: 500}) {
		t.Fatalf("a2 replay payment content: %+v", replay2.Payment)
	}
	if replay2.BillBalance != 1500 || replay2.Settled {
		t.Fatalf("a2 replay must return historical 1500/false, got balance=%d settled=%v",
			replay2.BillBalance, replay2.Settled)
	}

	// 历史付款结果不能替代账单查询与账户状态的当前结果，也不能让 a1 重新停用。
	wantAccountBillState(t, s, "a1", 1000, 1000, 0, true)
	wantAccountBillState(t, s, "a2", 2000, 500, 1500, false)
	wantAccountSuspended(t, s, "a1", false)
	wantAccountSuspended(t, s, "a2", true)

	// 账户内冲突边界：已成功使用的标识改成另一金额，按该账户记录判冲突，
	// 即使 a1 账单已结清、即使新金额不超过其曾经的余额。
	if _, err := s.RecordPayment("a1", sharedPaymentID, jan(2026), 1); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a1 same id with changed amount: %v, want ErrPaymentConflict", err)
	}
	// 改指向尚未出账的另一月份（二月尚未结束、也无账单）：去重先于账单查找，
	// 仍按该账户冲突拒绝。
	if _, err := s.RecordPayment("a1", sharedPaymentID, feb(2026), 400); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a1 same id repointed to unbilled month: %v, want ErrPaymentConflict", err)
	}
	// a2 的同名记录独立保持自己的冲突边界。
	if _, err := s.RecordPayment("a2", sharedPaymentID, jan(2026), 501); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a2 same id with changed amount: %v, want ErrPaymentConflict", err)
	}
	if _, err := s.RecordPayment("a2", sharedPaymentID, feb(2026), 500); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a2 same id repointed to unbilled month: %v, want ErrPaymentConflict", err)
	}

	// 冲突拒绝不改变任何账户的已付金额、余额或停用状态，也不消耗标识：
	// 此后原样重报仍返回各自的历史结果。
	wantAccountBillState(t, s, "a1", 1000, 1000, 0, true)
	wantAccountBillState(t, s, "a2", 2000, 500, 1500, false)
	wantAccountSuspended(t, s, "a1", false)
	wantAccountSuspended(t, s, "a2", true)
	if r, err := s.RecordPayment("a1", sharedPaymentID, jan(2026), 400); err != nil ||
		r.Registered || r.BillBalance != 600 || r.Settled {
		t.Fatalf("a1 replay after conflicts: r=%+v err=%v, want historical 600/false", r, err)
	}
	if r, err := s.RecordPayment("a2", sharedPaymentID, jan(2026), 500); err != nil ||
		r.Registered || r.BillBalance != 1500 || r.Settled {
		t.Fatalf("a2 replay after conflicts: r=%+v err=%v, want historical 1500/false", r, err)
	}
}

// TestPaymentConflictComparesOnlyLocalAccountRecord 用另一账户的“同款”参数
// 试探账户边界：两账户各自登记同名付款（a1 400、a2 500）后，在 a1 用 a2 的
// 金额 500、在 a2 用 a1 的金额 400 重放同一标识，都必须对照本账户记录判冲突，
// 而不能拿到另一账户的记录当重报接受；反之各自原样重报仍是本账户历史结果。
func TestPaymentConflictComparesOnlyLocalAccountRecord(t *testing.T) {
	s, _ := twoOverdueAccountsFixture(t)

	r1, err := s.RecordPayment("a1", sharedPaymentID, jan(2026), 400)
	if err != nil || !r1.Registered || r1.BillBalance != 600 || r1.Settled {
		t.Fatalf("a1 register 400: r=%+v err=%v", r1, err)
	}
	r2, err := s.RecordPayment("a2", sharedPaymentID, jan(2026), 500)
	if err != nil || !r2.Registered || r2.BillBalance != 1500 || r2.Settled {
		t.Fatalf("a2 register 500: r=%+v err=%v", r2, err)
	}

	// 500 恰是 a2 已登记的金额、且不超过 a1 当前余额 600：
	// 若去重误跨账户，这里可能被当成重报；正确行为是按 a1 的 400 记录判冲突。
	if _, err := s.RecordPayment("a1", sharedPaymentID, jan(2026), 500); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a1 using a2's amount 500: %v, want ErrPaymentConflict", err)
	}
	// 400 恰是 a1 已登记的金额、也不超过 a2 当前余额 1500：同理按 a2 记录判冲突。
	if _, err := s.RecordPayment("a2", sharedPaymentID, jan(2026), 400); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("a2 using a1's amount 400: %v, want ErrPaymentConflict", err)
	}

	// 冲突尝试不改动两账户；各自原样重报仍返回本账户首次登记的历史结果。
	wantAccountBillState(t, s, "a1", 1000, 400, 600, false)
	wantAccountBillState(t, s, "a2", 2000, 500, 1500, false)
	wantAccountSuspended(t, s, "a1", true)
	wantAccountSuspended(t, s, "a2", true)
	if r, err := s.RecordPayment("a1", sharedPaymentID, jan(2026), 400); err != nil ||
		r.Registered || r.BillBalance != 600 || r.Settled {
		t.Fatalf("a1 exact replay: r=%+v err=%v, want historical 600/false", r, err)
	}
	if r, err := s.RecordPayment("a2", sharedPaymentID, jan(2026), 500); err != nil ||
		r.Registered || r.BillBalance != 1500 || r.Settled {
		t.Fatalf("a2 exact replay: r=%+v err=%v, want historical 1500/false", r, err)
	}
}
