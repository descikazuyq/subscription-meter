package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 本文件回归“同一账户把同一个付款标识用于两笔内容不同、且同时到达的请求”。
//
// 已有保障覆盖了：顺序分次付款、不同标识并发争用同一笔余额、同一标识的并发
// 原样重报只入账一次，以及付款标识的去重域是单个账户。这里补充的是去重规则在
// 并发冲突下的不变量：
//
// 付款标识在账户内只能对应一笔成功登记，金额或账期不同都属于冲突；两笔请求
// 同时提交也必须保持这条规则。实现把“去重判定 → 账单查找 → 余额校验 → 扣款
// 落库 → 付款标识落库”全部放在同一把互斥锁的一个临界区内（见
// payment_concurrent_contention_test.go 的说明），所以并发的两笔必然串行裁决：
//
//   - 恰好一笔首次登记成功（Registered 为 true），返回的付款内容（账户、标识、
//     账期、金额）与该笔请求逐字一致，BillBalance/Settled 是它登记完成时的结果；
//   - 另一笔返回 ErrPaymentConflict，即使先成功的一笔已使账单余额不足以接收它
//     （700 先成功后 400 超额，或 400 先成功后 700 超额），也只能报标识冲突，
//     不能改报 ErrPaymentExceedsBalance；
//   - 冲突不落任何账：账单已付金额只增加成功的那一笔，另一张账单原样不动，
//     冲突不消耗付款标识，但冲突的内容永远不能覆盖已登记的内容；
//   - 此后原样重报成功请求：Registered 为 false，付款所属账期、金额以及首次
//     登记后的余额与付清状态原样返回，不再次入账；原样重报冲突请求：仍然冲突。
//     用另一个付款标识把成功请求的账单结清后，两种重报仍遵循同样规则，旧付款
//     返回首次登记时的历史结果，GetBill 与 Status 的账单摘要反映当前（已付清）
//     状态。

// sameIDPaymentCall 描述一笔并发付款请求：账期与金额（标识与账户由调用方统一给出）。
type sameIDPaymentCall struct {
	period Month
	amount int64
}

// sameIDPaymentOutcome 记录一笔并发付款的请求内容与返回结果。
type sameIDPaymentOutcome struct {
	period Month
	amount int64
	r      PaymentResult
	err    error
}

// contendSameID 用启动屏障让两笔使用同一付款标识的请求在同一时刻并发到达，
// 返回与传入顺序一致的两笔结果。屏障保证两笔真正并发，而非顺序提交。
func contendPaymentSameID(s *Service, acct, id string, first, second sameIDPaymentCall) [2]sameIDPaymentOutcome {
	calls := [2]sameIDPaymentCall{first, second}
	var res [2]sameIDPaymentOutcome
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i, q := range calls {
		go func(i int, q sameIDPaymentCall) {
			defer wg.Done()
			<-start
			r, err := s.RecordPayment(acct, id, q.period, q.amount)
			res[i] = sameIDPaymentOutcome{period: q.period, amount: q.amount, r: r, err: err}
		}(i, q)
	}
	close(start)
	wg.Wait()
	return res
}

// wantSameIDWinnerContent 断言成功结果的付款内容与该笔请求逐字一致：
// 账户、付款标识、账期、金额全部相同，Registered 为 true。
func wantSameIDWinnerContent(t *testing.T, oc sameIDPaymentOutcome, acct, id string) {
	t.Helper()
	want := Payment{AccountID: acct, PaymentID: id, Period: oc.period, Amount: oc.amount}
	if oc.r.Payment != want {
		t.Fatalf("winner payment content = %+v, want %+v", oc.r.Payment, want)
	}
}

// wantBill1000State 校验一张应付 1000 分账单的当前付款状态，并保证付款、冲突与
// 重报都不改变出账时即固定的应付总额、税额与用量明细。
func wantBill1000State(t *testing.T, s *Service, acct string, period Month, paid, balance int64, settled bool) {
	t.Helper()
	b, err := s.GetBill(acct, period)
	if err != nil {
		t.Fatalf("get bill %q %s: %v", acct, period, err)
	}
	if b.TotalDue != 1000 || b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill %q %s: due=%d paid=%d balance=%d settled=%v, want due=1000 paid=%d balance=%d settled=%v",
			acct, period, b.TotalDue, b.Paid, b.Balance, b.Settled, paid, balance, settled)
	}
	if b.MonthlyFee != 1000 || b.OverageFee != 0 || b.Tax != 0 ||
		b.TotalUsage != 0 || b.IncludedUnits != 0 || b.OverageUnits != 0 {
		t.Fatalf("bill %q %s immutable fields changed: %+v", acct, period, b)
	}
}

// wantBillSummary1000 校验账户状态中指定账期摘要与 GetBill 的当前状态一致。
func wantBillSummary1000(t *testing.T, s *Service, acct string, period Month, paid, balance int64, settled bool) {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	for _, bs := range st.Bills {
		if bs.Period == period {
			if bs.TotalDue != 1000 || bs.Paid != paid || bs.Balance != balance || bs.Settled != settled {
				t.Fatalf("status %q %s summary: due=%d paid=%d balance=%d settled=%v, want due=1000 paid=%d balance=%d settled=%v",
					acct, period, bs.TotalDue, bs.Paid, bs.Balance, bs.Settled, paid, balance, settled)
			}
			return
		}
	}
	t.Fatalf("status %q has no %s bill: %+v", acct, period, st.Bills)
}

// pickSameIDWinnerLoser 从两笔并发结果中找出唯一一笔首次登记成功的请求与唯一
// 一笔冲突请求；出现任何其它结果（两笔都成功、超额、账单不存在等）即失败。
func pickSameIDWinnerLoser(t *testing.T, res [2]sameIDPaymentOutcome, iter int) (winner, loser sameIDPaymentOutcome) {
	t.Helper()
	nRegistered, nConflict := 0, 0
	for i := range res {
		oc := &res[i]
		switch {
		case oc.err == nil && oc.r.Registered:
			nRegistered++
			winner = *oc
		case errors.Is(oc.err, ErrPaymentConflict):
			// 标识冲突是这里唯一允许的失败原因：不能是余额不足，也不能是
			// 账单不存在等其它错误。
			nConflict++
			loser = *oc
		default:
			t.Fatalf("iter %d payment period=%s amount=%d unexpected: r=%+v err=%v",
				iter, oc.period, oc.amount, oc.r, oc.err)
		}
	}
	if nRegistered != 1 || nConflict != 1 {
		t.Fatalf("iter %d want exactly one registered and one conflict, got %+v", iter, res)
	}
	return winner, loser
}

// TestConcurrentSameIDDifferentAmountsOneConflict 覆盖第一张例子：
// 同一张账单（2026-01，应付 1000、此前未收款）上，同一付款标识的 700 与 400
// 两笔请求同时到达。允许任意一笔先成功，但最终必须只有一笔实际登记：成功者
// Registered=true、付款内容与其请求一致，账单已付只增加该笔（余额 300 或 600，
// 未付清）；失败者返回 ErrPaymentConflict——700 先成时 400 已超额、400 先成时
// 700 已超额，两种情况下都不能改报余额不足。
func TestConcurrentSameIDDifferentAmountsOneConflict(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})

	const rounds = 200
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("amount%d", iter)
		mustBill1000(t, s, acct)

		res := contendPaymentSameID(s, acct, "dup",
			sameIDPaymentCall{period: jan(2026), amount: 700},
			sameIDPaymentCall{period: jan(2026), amount: 400},
		)
		winner, loser := pickSameIDWinnerLoser(t, res, iter)

		// 成功结果中的付款内容必须与实际入账的那笔请求逐字一致。
		wantSameIDWinnerContent(t, winner, acct, "dup")
		remaining := int64(1000) - winner.amount
		if remaining != 300 && remaining != 600 {
			t.Fatalf("iter %d unexpected winner amount %d", iter, winner.amount)
		}
		if winner.r.BillBalance != remaining || winner.r.Settled {
			t.Fatalf("iter %d winner result: %+v, want balance=%d settled=false",
				iter, winner.r, remaining)
		}
		// 失败的一笔必须是标识冲突，而不是余额不足（无论谁先成功，另一笔
		// 单独看都会超额）。
		if !errors.Is(loser.err, ErrPaymentConflict) || errors.Is(loser.err, ErrPaymentExceedsBalance) {
			t.Fatalf("iter %d loser must be conflict not exceeds-balance: %v", iter, loser.err)
		}

		// 账单已付金额只增加成功的那一笔；两种结果都未付清。
		wantBill1000State(t, s, acct, jan(2026), winner.amount, remaining, false)
		wantBillSummary1000(t, s, acct, jan(2026), winner.amount, remaining, false)

		// 原样重报成功请求：成功但 Registered=false，账期、金额与首次登记后的
		// 余额/付清状态保持原结果，不再次入账。
		replayW, err := s.RecordPayment(acct, "dup", jan(2026), winner.amount)
		if err != nil || replayW.Registered {
			t.Fatalf("iter %d replay winner: r=%+v err=%v, want Registered=false", iter, replayW, err)
		}
		want := Payment{AccountID: acct, PaymentID: "dup", Period: jan(2026), Amount: winner.amount}
		if replayW.Payment != want || replayW.BillBalance != remaining || replayW.Settled {
			t.Fatalf("iter %d replay winner historical result: %+v, want content=%+v balance=%d settled=false",
				iter, replayW, want, remaining)
		}

		// 原样重报冲突请求：仍然冲突，不能覆盖已经登记的付款；账单不变。
		_, err = s.RecordPayment(acct, "dup", jan(2026), loser.amount)
		if !errors.Is(err, ErrPaymentConflict) || errors.Is(err, ErrPaymentExceedsBalance) {
			t.Fatalf("iter %d replay loser must stay conflict: %v", iter, err)
		}
		wantBill1000State(t, s, acct, jan(2026), winner.amount, remaining, false)
		wantBillSummary1000(t, s, acct, jan(2026), winner.amount, remaining, false)
	}
}

// mustTwoBills1000 在指定账户上生成 2026-01、2026-02 两张账单：
// 应付与余额均为 1000、已付 0、未付清，此前均未收到任何付款。
// 调用前需已创建 plan1000，且服务时钟不早于 2026-03-01（两月均已结束）。
func mustTwoBills1000(t *testing.T, s *Service, acct string) {
	t.Helper()
	mustAccount(t, s, acct)
	if err := s.Subscribe(acct, "plan1000", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe %q: %v", acct, err)
	}
	for _, p := range []Month{jan(2026), feb(2026)} {
		b, err := s.CreateBill(acct, p)
		if err != nil {
			t.Fatalf("create bill %q %s: %v", acct, p, err)
		}
		if b.TotalDue != 1000 || b.Paid != 0 || b.Balance != 1000 || b.Settled {
			t.Fatalf("bill %q %s initial state: %+v", acct, p, b)
		}
	}
}

// TestConcurrentSameIDSameAmountDifferentBillsOneConflict 覆盖第二张例子：
// 同一账户两张都已生成、尚未收款的账单（2026-01、2026-02，各应付 1000），
// 两笔金额相同（各 400）、账期不同的请求使用同一付款标识同时登记。金额一致
// 不放宽账户内的标识约束（账期不同即冲突）：恰好一笔成功（成功请求所指向的
// 账单已付 400、余额 600、未付清），另一笔返回同一个 ErrPaymentConflict，
// 它所指向的账单已付金额、余额与付清状态保持原样。
func TestConcurrentSameIDSameAmountDifferentBillsOneConflict(t *testing.T) {
	s, _ := newTestService(utc(2026, 3, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})

	const rounds = 200
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("bills%d", iter)
		mustTwoBills1000(t, s, acct)

		res := contendPaymentSameID(s, acct, "dup",
			sameIDPaymentCall{period: jan(2026), amount: 400},
			sameIDPaymentCall{period: feb(2026), amount: 400},
		)
		winner, loser := pickSameIDWinnerLoser(t, res, iter)

		// 成功者的付款内容指向它自己请求的那张账单、金额 400；登记后该账单
		// 已付 400、余额 600、未付清。
		wantSameIDWinnerContent(t, winner, acct, "dup")
		if winner.amount != 400 || winner.r.BillBalance != 600 || winner.r.Settled {
			t.Fatalf("iter %d winner result: %+v, want amount=400 balance=600 settled=false",
				iter, winner.r)
		}
		if winner.period != jan(2026) && winner.period != feb(2026) {
			t.Fatalf("iter %d unexpected winner period %s", iter, winner.period)
		}
		other := feb(2026)
		if winner.period == feb(2026) {
			other = jan(2026)
		}

		// 只有成功请求所指向的账单增加已付金额；另一张账单原样不动。
		wantBill1000State(t, s, acct, winner.period, 400, 600, false)
		wantBillSummary1000(t, s, acct, winner.period, 400, 600, false)
		wantBill1000State(t, s, acct, other, 0, 1000, false)
		wantBillSummary1000(t, s, acct, other, 0, 1000, false)

		// 原样重报成功请求：Registered=false、历史结果为余额 600、未付清。
		replayW, err := s.RecordPayment(acct, "dup", winner.period, 400)
		if err != nil || replayW.Registered ||
			replayW.Payment.Period != winner.period || replayW.Payment.Amount != 400 ||
			replayW.BillBalance != 600 || replayW.Settled {
			t.Fatalf("iter %d replay winner: %+v err=%v, want historical 600/false on %s",
				iter, replayW, err, winner.period)
		}
		// 原样重报指向另一张账单的冲突请求：仍然冲突；两张账单都不变。
		_, err = s.RecordPayment(acct, "dup", loser.period, 400)
		if !errors.Is(err, ErrPaymentConflict) {
			t.Fatalf("iter %d replay loser against other bill must conflict: %v", iter, err)
		}
		wantBill1000State(t, s, acct, winner.period, 400, 600, false)
		wantBill1000State(t, s, acct, other, 0, 1000, false)
	}
}

// TestSameIDConflictBothOrdersAndSettlementDeterministic 以确定顺序锁住并发测试
// 不保证每次都调度出来的两种先后路径，并补充结清后的重报规则：
//
//   - 同账单不同金额：700 先成（400 再报已超额）与 400 先成（700 再报已超额），
//     后到的一笔都只能是 ErrPaymentConflict，不是 ErrPaymentExceedsBalance；
//   - 同金额不同账单：一月先成、二月先成两条路径，另一张账单都保持原样；
//   - 用另一个付款标识结清成功请求的账单后，原样重报成功请求仍返回首次登记时
//     的历史结果（旧余额、未付清、Registered=false），原样重报冲突请求仍冲突；
//     GetBill 与 Status 账单摘要显示当前已付 1000、余额 0、已付清。
func TestSameIDConflictBothOrdersAndSettlementDeterministic(t *testing.T) {
	s, _ := newTestService(utc(2026, 3, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})

	// assertLoserConflictAndReplays 复用并发用例中的裁决后规则：冲突请求原样
	// 重报仍冲突；随后用 settleID 把 period 账单结清，再验证两种重报规则与
	// 账单/状态摘要的当前值。
	assertLoserConflictAndReplays := func(acct string, winnerPeriod Month, winnerAmount, remaining int64, loserPeriod Month, loserAmount int64) {
		t.Helper()
		// 后到的冲突请求：标识冲突，不能因余额已不足而改报超额。
		if _, err := s.RecordPayment(acct, "dup", loserPeriod, loserAmount); !errors.Is(err, ErrPaymentConflict) ||
			errors.Is(err, ErrPaymentExceedsBalance) {
			t.Fatalf("%q loser period=%s amount=%d must be conflict (not exceeds): %v",
				acct, loserPeriod, loserAmount, err)
		}
		// 成功请求原样重报：返回首次登记完成时的历史结果，不再次入账。
		rw, err := s.RecordPayment(acct, "dup", winnerPeriod, winnerAmount)
		if err != nil || rw.Registered ||
			rw.Payment.Period != winnerPeriod || rw.Payment.Amount != winnerAmount ||
			rw.BillBalance != remaining || rw.Settled {
			t.Fatalf("%q winner replay before settle: %+v %v, want %s/%d historical balance=%d settled=false",
				acct, rw, err, winnerPeriod, winnerAmount, remaining)
		}

		// 用另一个付款标识把成功请求对应的账单结清。
		topUp, err := s.RecordPayment(acct, "settle", winnerPeriod, remaining)
		if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
			t.Fatalf("%q settle %s with %d: %+v %v", acct, winnerPeriod, remaining, topUp, err)
		}

		// 结清后原样重报成功请求：历史结果不变（旧余额、未付清），不再次入账。
		rw2, err := s.RecordPayment(acct, "dup", winnerPeriod, winnerAmount)
		if err != nil || rw2.Registered ||
			rw2.Payment.Period != winnerPeriod || rw2.Payment.Amount != winnerAmount ||
			rw2.BillBalance != remaining || rw2.Settled {
			t.Fatalf("%q winner replay after settle: %+v %v, want historical balance=%d settled=false",
				acct, rw2, err, remaining)
		}
		// 结清后原样重报冲突请求：仍然冲突，不能覆盖已登记付款。
		// 此时账单余额为零，也绝不能改报余额不足。
		_, err = s.RecordPayment(acct, "dup", loserPeriod, loserAmount)
		if !errors.Is(err, ErrPaymentConflict) || errors.Is(err, ErrPaymentExceedsBalance) {
			t.Fatalf("%q loser replay after settle must stay conflict: %v", acct, err)
		}

		// 账单查询与账户状态摘要显示当前状态：已付 1000、余额 0、已付清。
		wantBill1000State(t, s, acct, winnerPeriod, 1000, 0, true)
		wantBillSummary1000(t, s, acct, winnerPeriod, 1000, 0, true)
	}

	// 顺序一（同账单不同金额）：700 先成功，400 后到——此时余额只剩 300。
	mustBill1000(t, s, "big-first")
	r700, err := s.RecordPayment("big-first", "dup", jan(2026), 700)
	if err != nil || !r700.Registered || r700.Payment.Period != jan(2026) ||
		r700.Payment.Amount != 700 || r700.BillBalance != 300 || r700.Settled {
		t.Fatalf("big-first 700 first: %+v %v", r700, err)
	}
	assertLoserConflictAndReplays("big-first", jan(2026), 700, 300, jan(2026), 400)

	// 顺序二（同账单不同金额）：400 先成功，700 后到——此时余额只剩 600。
	mustBill1000(t, s, "small-first")
	r400, err := s.RecordPayment("small-first", "dup", jan(2026), 400)
	if err != nil || !r400.Registered || r400.Payment.Period != jan(2026) ||
		r400.Payment.Amount != 400 || r400.BillBalance != 600 || r400.Settled {
		t.Fatalf("small-first 400 first: %+v %v", r400, err)
	}
	assertLoserConflictAndReplays("small-first", jan(2026), 400, 600, jan(2026), 700)

	// 顺序三（同金额不同账单）：一月先成功，指向二月的同标识 400 冲突；
	// 二月账单自始至终保持 0/1000/未付清，且一月结清不影响它。
	mustTwoBills1000(t, s, "jan-first")
	rj, err := s.RecordPayment("jan-first", "dup", jan(2026), 400)
	if err != nil || !rj.Registered || rj.BillBalance != 600 || rj.Settled {
		t.Fatalf("jan-first jan 400 first: %+v %v", rj, err)
	}
	assertLoserConflictAndReplays("jan-first", jan(2026), 400, 600, feb(2026), 400)
	wantBill1000State(t, s, "jan-first", feb(2026), 0, 1000, false)
	wantBillSummary1000(t, s, "jan-first", feb(2026), 0, 1000, false)

	// 顺序四（同金额不同账单）：二月先成功，指向一月的同标识 400 冲突；
	// 一月账单保持原样，二月结清不影响它。
	mustTwoBills1000(t, s, "feb-first")
	rf, err := s.RecordPayment("feb-first", "dup", feb(2026), 400)
	if err != nil || !rf.Registered || rf.Payment.Period != feb(2026) ||
		rf.Payment.Amount != 400 || rf.BillBalance != 600 || rf.Settled {
		t.Fatalf("feb-first feb 400 first: %+v %v", rf, err)
	}
	assertLoserConflictAndReplays("feb-first", feb(2026), 400, 600, jan(2026), 400)
	wantBill1000State(t, s, "feb-first", jan(2026), 0, 1000, false)
	wantBillSummary1000(t, s, "feb-first", jan(2026), 0, 1000, false)
}
