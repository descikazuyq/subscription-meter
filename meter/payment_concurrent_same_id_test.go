package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 本文件回归“同一账户、两份内容不同的请求使用此前从未登记过的同一个
// 付款标识同时到达”的行为。
//
// 规则是：付款标识只在所属账户内对应一笔成功登记，金额或账期不同都属于
// 冲突——这条规则在同时提交时同样成立。实现把“去重判定 → 账单查找 →
// 余额校验 → 扣款落库 → 付款标识落库”放在同一把互斥锁的一个临界区内
// （见 RecordPayment），两份同标识请求必然串行裁决：恰好一份作为首次
// 付款登记成功（Registered 为 true，返回的付款内容与该笔请求一致），
// 另一份返回 ErrPaymentConflict。允许任意一份先成功。
//
// 去重判定先于账单查找与余额校验，因此即使先成功的一笔已经使账单余额
// 不足以接收另一份请求，后者仍按标识冲突拒绝，不能改报
// ErrPaymentExceedsBalance；冲突失败不落账、不消耗付款标识、不改变任何
// 账单的已付金额、余额与付清状态。
//
// 这里锁住两种确实会影响入账归属的内容差异：
//   - 同一张账单、金额不同（700 与 400，账单应付 1000 且此前未收款）：
//     账单只增加成功那一笔，余额为 300 或 600，仍未付清；
//   - 相同金额（各 400）但指向同账户两张不同账单（2026-01 与 2026-02，
//     两张账单均应付 1000、尚未收款）：只有成功请求指向的账单增加已付
//     金额，另一张账单的已付金额、余额及付清状态保持原样——标识约束
//     不因账期不同而放宽。
//
// 争用结束后，标识始终对应首次成功登记的内容：原样重报成功请求仍成功，
// 但 Registered 为 false，付款所属账期、金额以及首次登记后的余额与付清
// 状态保持原历史结果，不再次入账；原样重报冲突请求仍然冲突，不能覆盖
// 已经登记的付款。用另一个标识把成功请求对应的账单结清后，两种重报仍
// 遵循同一规则，而 GetBill 与 Status 的账单摘要显示当前已付金额、零余额
// 与已付清，不被旧付款返回的历史结果覆盖。

// samePaymentReq 是一笔参与同标识并发争用的付款请求：账期与金额由调用
// 方指定，账户与付款标识在发起时统一给出。
type samePaymentReq struct {
	period Month
	amount int64
}

// samePaymentOutcome 记录一笔争用请求的账期、金额与返回结果。
type samePaymentOutcome struct {
	period Month
	amount int64
	r      PaymentResult
	err    error
}

// contendSamePayment 用启动屏障让两笔请求真正并发地（而非顺序提交）以
// 同一付款标识在同一账户上首次登记，返回与传入顺序一致的两笔结果。
func contendSamePayment(s *Service, acct, paymentID string, first, second samePaymentReq) []samePaymentOutcome {
	reqs := []samePaymentReq{first, second}
	res := make([]samePaymentOutcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i, q := range reqs {
		go func(i int, q samePaymentReq) {
			defer wg.Done()
			<-start
			r, err := s.RecordPayment(acct, paymentID, q.period, q.amount)
			res[i] = samePaymentOutcome{period: q.period, amount: q.amount, r: r, err: err}
		}(i, q)
	}
	close(start)
	wg.Wait()
	return res
}

// resolveSamePaymentContention 断言并发结果恰为“一笔首次登记成功 + 一笔
// ErrPaymentConflict”，返回赢家（实际登记）与输家（冲突）。赢家返回的
// 付款内容必须与它自己的请求一致，输家必须是标识冲突而非余额不足；
// 冲突失败不产生 Registered=false 的幂等成功。
func resolveSamePaymentContention(t *testing.T, acct, paymentID string, res []samePaymentOutcome) (winner, loser samePaymentOutcome) {
	t.Helper()
	var w, l *samePaymentOutcome
	for i := range res {
		oc := &res[i]
		switch {
		case oc.err == nil && oc.r.Registered:
			w = oc
		case errors.Is(oc.err, ErrPaymentConflict):
			l = oc
		default:
			t.Fatalf("%q contention payment %s %d unexpected: r=%+v err=%v",
				acct, oc.period, oc.amount, oc.r, oc.err)
		}
	}
	if w == nil || l == nil {
		t.Fatalf("%q contention for %q want exactly one registered and one conflict, got %+v",
			acct, paymentID, res)
	}
	// 赢家的付款内容必须与它自己那笔请求一致，不能被输家的内容覆盖。
	if w.r.Payment.AccountID != acct || w.r.Payment.PaymentID != paymentID ||
		w.r.Payment.Period != w.period || w.r.Payment.Amount != w.amount {
		t.Fatalf("%q winner payment content %+v does not match its own request %s %d",
			acct, w.r.Payment, w.period, w.amount)
	}
	// 输家必须是标识冲突：即使赢家扣款后账单余额已不足以接收它，也不能
	// 改报余额不足。
	if errors.Is(l.err, ErrPaymentExceedsBalance) {
		t.Fatalf("%q loser %s %d reported exceeds-balance instead of payment conflict",
			acct, l.period, l.amount)
	}
	return *w, *l
}

// wantBillStateAndStatus 校验指定账期账单当前的付款状态：GetBill 与 Status
// 中的账单摘要必须一致；付款只更新付款状态，不改变应付总额与税额。
func wantBillStateAndStatus(t *testing.T, s *Service, acct string, m Month, totalDue, paid, balance int64, settled bool) {
	t.Helper()
	b, err := s.GetBill(acct, m)
	if err != nil {
		t.Fatalf("get bill %q %s: %v", acct, m, err)
	}
	if b.TotalDue != totalDue || b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill %q %s state: due=%d paid=%d balance=%d settled=%v, want due=%d paid=%d balance=%d settled=%v",
			acct, m, b.TotalDue, b.Paid, b.Balance, b.Settled, totalDue, paid, balance, settled)
	}
	if b.Tax != 0 {
		t.Fatalf("bill %q %s tax changed: %+v", acct, m, b)
	}
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	bs := statusBillSummary(t, st, m)
	if bs.TotalDue != totalDue || bs.Paid != paid || bs.Balance != balance || bs.Settled != settled {
		t.Fatalf("status %q %s summary = %+v, want due=%d paid=%d balance=%d settled=%v",
			acct, m, bs, totalDue, paid, balance, settled)
	}
}

// assertWinnerHistoricalReplay 断言原样重报赢家请求：成功但 Registered 为
// false、付款内容与首次一致，BillBalance/Settled 是首次登记完成时的历史
// 结果（由调用方给出），不再次入账。
func assertWinnerHistoricalReplay(t *testing.T, s *Service, acct, paymentID string, w samePaymentOutcome, historicalBalance int64) {
	t.Helper()
	replay, err := s.RecordPayment(acct, paymentID, w.period, w.amount)
	if err != nil {
		t.Fatalf("%q replay winner %s %d: %v", acct, w.period, w.amount, err)
	}
	if replay.Registered {
		t.Fatalf("%q replay winner must not register again: %+v", acct, replay)
	}
	if replay.Payment.AccountID != acct || replay.Payment.PaymentID != paymentID ||
		replay.Payment.Period != w.period || replay.Payment.Amount != w.amount {
		t.Fatalf("%q replay winner content %+v, want %s %d", acct, replay.Payment, w.period, w.amount)
	}
	if replay.BillBalance != historicalBalance || replay.Settled != (historicalBalance == 0) {
		t.Fatalf("%q replay winner historical result: balance=%d settled=%v, want balance=%d",
			acct, replay.BillBalance, replay.Settled, historicalBalance)
	}
}

// assertLoserStillConflicts 断言原样重报输家请求仍返回 ErrPaymentConflict，
// 不能被当成原样重报接受，也不能覆盖已登记的付款。
func assertLoserStillConflicts(t *testing.T, s *Service, acct, paymentID string, l samePaymentOutcome) {
	t.Helper()
	if _, err := s.RecordPayment(acct, paymentID, l.period, l.amount); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("%q replay loser %s %d should still conflict, got %v",
			acct, l.period, l.amount, err)
	}
}

// TestConcurrentSamePaymentIDSameBillDifferentAmount 覆盖第一种内容差异：
// 同一张 2026-01 账单（应付 1000、此前未收款）上，700 与 400 两笔请求
// 使用同一新标识同时登记。恰好一笔首次登记成功，另一笔返回冲突；即使
// 赢家扣款后余额已不足以接收输家，输家仍是标识冲突而非余额不足。
func TestConcurrentSamePaymentIDSameBillDifferentAmount(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("same-bill-%d", iter)
		mustBill1000(t, s, acct)

		res := contendSamePayment(s, acct, "dup-pay",
			samePaymentReq{period: jan(2026), amount: 700},
			samePaymentReq{period: jan(2026), amount: 400},
		)
		winner, loser := resolveSamePaymentContention(t, acct, "dup-pay", res)

		// 只允许成功那一笔落账：已付等于赢家金额，余额 300 或 600，未付清。
		remaining := int64(1000) - winner.amount
		if remaining != 300 && remaining != 600 {
			t.Fatalf("iter %d unexpected winner amount %d", iter, winner.amount)
		}
		if winner.r.BillBalance != remaining || winner.r.Settled {
			t.Fatalf("iter %d winner result: %+v, want balance=%d settled=false",
				iter, winner.r, remaining)
		}
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, winner.amount, remaining, false)

		// 争用后立刻按各自原内容重报：赢家是幂等历史返回，输家仍冲突，
		// 账单状态不变。
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay", winner, remaining)
		assertLoserStillConflicts(t, s, acct, "dup-pay", loser)
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, winner.amount, remaining, false)

		// 用另一个付款标识把赢家账单的剩余余额结清。
		topUp, err := s.RecordPayment(acct, "settle-rest", jan(2026), remaining)
		if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
			t.Fatalf("iter %d settle remaining %d: r=%+v err=%v", iter, remaining, topUp, err)
		}

		// 结清后两种重报规则不变：旧付款仍返回首次登记时的历史结果
		// （余额 300/600、未付清），冲突请求仍冲突；账单查询与账户状态
		// 摘要显示当前已付 1000、零余额、已付清。
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay", winner, remaining)
		assertLoserStillConflicts(t, s, acct, "dup-pay", loser)
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 1000, 0, true)
	}
}

// mustTwoBills1000 在指定账户上连续出两张账单（2026-01 与 2026-02）：
// 各自应付与余额均为 1000 分、已付 0、未付清，且尚未收到任何付款。
// 订阅自 2026-01-01 起生效，同一订阅连续覆盖这两个账期。
func mustTwoBills1000(t *testing.T, s *Service, acct string) {
	t.Helper()
	mustAccount(t, s, acct)
	if err := s.Subscribe(acct, "plan1000", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe %q: %v", acct, err)
	}
	for _, m := range []Month{jan(2026), feb(2026)} {
		b, err := s.CreateBill(acct, m)
		if err != nil {
			t.Fatalf("create bill %q %s: %v", acct, m, err)
		}
		if b.TotalDue != 1000 || b.Paid != 0 || b.Balance != 1000 || b.Settled || b.Tax != 0 {
			t.Fatalf("bill %q %s initial state: %+v", acct, m, b)
		}
	}
}

// TestConcurrentSamePaymentIDDifferentBillSameAmount 覆盖第二种内容差异：
// 同一账户两张均应付 1000、尚未收款的账单（2026-01、2026-02）上，两笔
// 金额相同（各 400）的请求使用同一新标识同时登记，账期不同即为冲突。
// 只有赢家所指向的账单增加已付金额，输家指向的账单已付金额、余额与
// 付清状态保持原样。
func TestConcurrentSamePaymentIDDifferentBillSameAmount(t *testing.T) {
	s, _ := newTestService(utc(2026, 3, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("cross-bill-%d", iter)
		mustTwoBills1000(t, s, acct)

		res := contendSamePayment(s, acct, "dup-pay",
			samePaymentReq{period: jan(2026), amount: 400},
			samePaymentReq{period: feb(2026), amount: 400},
		)
		winner, loser := resolveSamePaymentContention(t, acct, "dup-pay", res)

		if winner.period != jan(2026) && winner.period != feb(2026) {
			t.Fatalf("iter %d winner points to unexpected period %s", iter, winner.period)
		}
		if winner.period == loser.period {
			t.Fatalf("iter %d winner and loser share period %s", iter, winner.period)
		}
		// 赢家结果：所属账期是它自己请求的账期，已付 400、余额 600、未付清。
		if winner.r.BillBalance != 600 || winner.r.Settled {
			t.Fatalf("iter %d winner %s result: %+v, want balance=600 settled=false",
				iter, winner.period, winner.r)
		}
		// 只有赢家指向的账单增加 400；输家指向的账单保持 0/1000/未付清。
		wantBillStateAndStatus(t, s, acct, winner.period, 1000, 400, 600, false)
		wantBillStateAndStatus(t, s, acct, loser.period, 1000, 0, 1000, false)

		// 两种原内容重报：赢家幂等历史返回、输家仍冲突；两张账单状态不变。
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay", winner, 600)
		assertLoserStillConflicts(t, s, acct, "dup-pay", loser)
		wantBillStateAndStatus(t, s, acct, winner.period, 1000, 400, 600, false)
		wantBillStateAndStatus(t, s, acct, loser.period, 1000, 0, 1000, false)

		// 用另一个标识只把赢家账单结清：输家账单仍保持原样。
		topUp, err := s.RecordPayment(acct, "settle-winner", winner.period, 600)
		if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
			t.Fatalf("iter %d settle winner %s: r=%+v err=%v", iter, winner.period, topUp, err)
		}

		// 结清后两种重报规则不变：旧付款返回首次登记时的历史结果
		// （余额 600、未付清），冲突请求仍冲突；赢家账单当前 1000/0/已付清，
		// 输家账单仍 0/1000/未付清。
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay", winner, 600)
		assertLoserStillConflicts(t, s, acct, "dup-pay", loser)
		wantBillStateAndStatus(t, s, acct, winner.period, 1000, 1000, 0, true)
		wantBillStateAndStatus(t, s, acct, loser.period, 1000, 0, 1000, false)
	}
}

// TestConcurrentSamePaymentIDSameBillBothOrdersDeterministic 以确定顺序锁住
// 同一账单争用的两种可能获胜结果（并发测试不保证每种先后顺序都被调度
// 到）：去重先于余额校验，所以无论哪笔先成功，输家都返回标识冲突而不是
// 余额不足，且不占用标识、不落账；结清后两种重报仍按历史结果与冲突
// 处理。
func TestConcurrentSamePaymentIDSameBillBothOrdersDeterministic(t *testing.T) {
	// 顺序一：700 先成功（余额 300），400 后到——此时 400 已超过余额
	// 300，但必须报标识冲突而非余额不足。
	t.Run("700-first", func(t *testing.T) {
		s, _ := newTestService(utc(2026, 2, 1, 0, 0))
		mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})
		acct := "det-big-first"
		mustBill1000(t, s, acct)

		r700, err := s.RecordPayment(acct, "dup-pay", jan(2026), 700)
		if err != nil || !r700.Registered || r700.BillBalance != 300 || r700.Settled {
			t.Fatalf("700 first: r=%+v err=%v", r700, err)
		}
		if _, err := s.RecordPayment(acct, "dup-pay", jan(2026), 400); !errors.Is(err, ErrPaymentConflict) {
			t.Fatalf("400 after 700 should conflict (not exceeds-balance): %v", err)
		}
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 700, 300, false)

		// 赢家重报返回历史结果；输家重报仍冲突。
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 700}, 300)
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 400})

		// 用另一标识结清后规则不变。
		topUp, err := s.RecordPayment(acct, "settle-rest", jan(2026), 300)
		if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
			t.Fatalf("settle 300: r=%+v err=%v", topUp, err)
		}
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 700}, 300)
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 400})
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 1000, 0, true)
	})

	// 顺序二：400 先成功（余额 600），700 后到——700 已超过余额 600，
	// 同样必须报标识冲突而非余额不足。
	t.Run("400-first", func(t *testing.T) {
		s, _ := newTestService(utc(2026, 2, 1, 0, 0))
		mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})
		acct := "det-small-first"
		mustBill1000(t, s, acct)

		r400, err := s.RecordPayment(acct, "dup-pay", jan(2026), 400)
		if err != nil || !r400.Registered || r400.BillBalance != 600 || r400.Settled {
			t.Fatalf("400 first: r=%+v err=%v", r400, err)
		}
		if _, err := s.RecordPayment(acct, "dup-pay", jan(2026), 700); !errors.Is(err, ErrPaymentConflict) {
			t.Fatalf("700 after 400 should conflict (not exceeds-balance): %v", err)
		}
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 400, 600, false)

		assertWinnerHistoricalReplay(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 400}, 600)
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 700})

		topUp, err := s.RecordPayment(acct, "settle-rest", jan(2026), 600)
		if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
			t.Fatalf("settle 600: r=%+v err=%v", topUp, err)
		}
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 400}, 600)
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 700})
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 1000, 0, true)
	})
}

// TestConcurrentSamePaymentIDDifferentBillBothOrdersDeterministic 以确定顺序
// 锁住跨账单争用的两种可能获胜结果：同标识、同金额但账期不同必然冲突，
// 只有先成功者指向的账单落账；两种重报分别为历史幂等返回与持续冲突，
// 赢家账单结清后规则不变、输家账单始终保持原样。
func TestConcurrentSamePaymentIDDifferentBillBothOrdersDeterministic(t *testing.T) {
	// 顺序一：1 月请求先成功，2 月请求冲突。
	t.Run("january-first", func(t *testing.T) {
		s, _ := newTestService(utc(2026, 3, 1, 0, 0))
		mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})
		acct := "det-jan-first"
		mustTwoBills1000(t, s, acct)

		rJan, err := s.RecordPayment(acct, "dup-pay", jan(2026), 400)
		if err != nil || !rJan.Registered || rJan.Payment.Period != jan(2026) ||
			rJan.BillBalance != 600 || rJan.Settled {
			t.Fatalf("jan first: r=%+v err=%v", rJan, err)
		}
		if _, err := s.RecordPayment(acct, "dup-pay", feb(2026), 400); !errors.Is(err, ErrPaymentConflict) {
			t.Fatalf("february after january should conflict: %v", err)
		}
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 400, 600, false)
		wantBillStateAndStatus(t, s, acct, feb(2026), 1000, 0, 1000, false)

		assertWinnerHistoricalReplay(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 400}, 600)
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: feb(2026), amount: 400})

		// 只结清赢家（1 月）账单：2 月账单保持原样，两种重报规则不变。
		topUp, err := s.RecordPayment(acct, "settle-jan", jan(2026), 600)
		if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
			t.Fatalf("settle jan: r=%+v err=%v", topUp, err)
		}
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 400}, 600)
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: feb(2026), amount: 400})
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 1000, 0, true)
		wantBillStateAndStatus(t, s, acct, feb(2026), 1000, 0, 1000, false)

		// 冲突不消耗标识之外，2 月账单仍可用自己的新标识正常收款：
		// 付 400 后余额 600、未付清，且不影响 1 月账单。
		febPay, err := s.RecordPayment(acct, "feb-own", feb(2026), 400)
		if err != nil || !febPay.Registered || febPay.BillBalance != 600 || febPay.Settled {
			t.Fatalf("february own payment: r=%+v err=%v", febPay, err)
		}
		wantBillStateAndStatus(t, s, acct, feb(2026), 1000, 400, 600, false)
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 1000, 0, true)
		// 此前的冲突重报结论不被新付款改变。
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: feb(2026), amount: 400})
		wantBillStateAndStatus(t, s, acct, feb(2026), 1000, 400, 600, false)
	})

	// 顺序二：2 月请求先成功，1 月请求冲突。
	t.Run("february-first", func(t *testing.T) {
		s, _ := newTestService(utc(2026, 3, 1, 0, 0))
		mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})
		acct := "det-feb-first"
		mustTwoBills1000(t, s, acct)

		rFeb, err := s.RecordPayment(acct, "dup-pay", feb(2026), 400)
		if err != nil || !rFeb.Registered || rFeb.Payment.Period != feb(2026) ||
			rFeb.BillBalance != 600 || rFeb.Settled {
			t.Fatalf("feb first: r=%+v err=%v", rFeb, err)
		}
		if _, err := s.RecordPayment(acct, "dup-pay", jan(2026), 400); !errors.Is(err, ErrPaymentConflict) {
			t.Fatalf("january after february should conflict: %v", err)
		}
		wantBillStateAndStatus(t, s, acct, feb(2026), 1000, 400, 600, false)
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 0, 1000, false)

		assertWinnerHistoricalReplay(t, s, acct, "dup-pay",
			samePaymentOutcome{period: feb(2026), amount: 400}, 600)
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 400})

		// 只结清赢家（2 月）账单：1 月账单保持原样，两种重报规则不变。
		topUp, err := s.RecordPayment(acct, "settle-feb", feb(2026), 600)
		if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
			t.Fatalf("settle feb: r=%+v err=%v", topUp, err)
		}
		assertWinnerHistoricalReplay(t, s, acct, "dup-pay",
			samePaymentOutcome{period: feb(2026), amount: 400}, 600)
		assertLoserStillConflicts(t, s, acct, "dup-pay",
			samePaymentOutcome{period: jan(2026), amount: 400})
		wantBillStateAndStatus(t, s, acct, feb(2026), 1000, 1000, 0, true)
		wantBillStateAndStatus(t, s, acct, jan(2026), 1000, 0, 1000, false)
	})
}
