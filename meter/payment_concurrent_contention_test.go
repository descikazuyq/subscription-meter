package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 本文件回归“两笔不同付款标识争用同一张账单的同一笔余额”。
//
// 已有保障覆盖了顺序分次付款，以及同一付款标识并发重报只扣款一次；这里补充的是
// 标识不同、各自单独看都合法的两笔付款并发到达的情况。付款能否登记必须以登记
// 那一刻账单的实际剩余应付为准：实现把“去重判定 → 账单查找 → 余额校验 → 扣款
// 落库 → 付款标识落库”全部放在同一把互斥锁的一个临界区内，因此两笔请求必然
// 串行裁决，后到的一笔看到的是先到一笔扣款后的真实余额。
//
// 这些测试锁住该行为，防止日后把余额检查与扣款拆成两个步骤（TOCTOU）而出现：
// 两笔都登记成功、已付金额超出应付、负余额、误报付清，或失败的一笔占用了付款
// 标识。

// paymentOutcome 记录一笔并发付款的标识、金额与返回结果。
type paymentOutcome struct {
	id     string
	amount int64
	r      PaymentResult
	err    error
}

// mustBill1000 在指定账户上出一张 2026-01 的账单：应付与余额均为 1000 分、
// 已付 0、未付清、税额 0、用量 0，且尚未收到任何付款。
func mustBill1000(t *testing.T, s *Service, acct string) {
	t.Helper()
	mustAccount(t, s, acct)
	if err := s.Subscribe(acct, "plan1000", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe %q: %v", acct, err)
	}
	b, err := s.CreateBill(acct, jan(2026))
	if err != nil {
		t.Fatalf("create bill %q: %v", acct, err)
	}
	if b.TotalDue != 1000 || b.Paid != 0 || b.Balance != 1000 || b.Settled || b.Tax != 0 || b.TotalUsage != 0 {
		t.Fatalf("bill %q initial state: %+v", acct, b)
	}
}

// contendOnce 用启动屏障让两笔不同标识的付款在同一时刻争用同一张账单，
// 返回与传入顺序一致的两笔结果。屏障保证两笔真正并发，而非顺序提交。
func contendOnce(s *Service, acct string, first paymentOutcome, second paymentOutcome) []paymentOutcome {
	reqs := []paymentOutcome{first, second}
	res := make([]paymentOutcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i, q := range reqs {
		go func(i int, q paymentOutcome) {
			defer wg.Done()
			<-start
			r, err := s.RecordPayment(acct, q.id, jan(2026), q.amount)
			res[i] = paymentOutcome{id: q.id, amount: q.amount, r: r, err: err}
		}(i, q)
	}
	close(start)
	wg.Wait()
	return res
}

// billSummary1000 返回账户状态中 2026-01 账单的摘要，测试断言其与账单查询一致。
func billSummary1000(t *testing.T, s *Service, acct string) BillSummary {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	for _, bs := range st.Bills {
		if bs.Period == jan(2026) {
			return bs
		}
	}
	t.Fatalf("status %q has no Jan bill: %+v", acct, st.Bills)
	return BillSummary{}
}

// TestConcurrentDistinctPaymentsOnlyOneWithinBalance 覆盖第一张例子：
// 初始余额 1000，两笔标识不同的付款 700 与 400 并发到达。它们单独提交都合法，
// 但不能一起登记成功：恰好一笔首次登记成功，另一笔返回 ErrPaymentExceedsBalance。
// 允许任意一笔先成功，两种结果都未付清。
func TestConcurrentDistinctPaymentsOnlyOneWithinBalance(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("a%d", iter)
		mustBill1000(t, s, acct)

		res := contendOnce(s, acct,
			paymentOutcome{id: "p700", amount: 700},
			paymentOutcome{id: "p400", amount: 400},
		)

		// 恰好一笔首次登记成功，恰好一笔因超额被拒；不允许两笔都成功，
		// 也不允许返回标识冲突等其它错误。
		var winner paymentOutcome
		var loser *paymentOutcome
		nRegistered, nExceeded := 0, 0
		for i := range res {
			oc := &res[i]
			switch {
			case oc.err == nil && oc.r.Registered:
				nRegistered++
				winner = *oc
			case errors.Is(oc.err, ErrPaymentExceedsBalance):
				nExceeded++
				loser = oc
			default:
				t.Fatalf("iter %d payment %q unexpected: r=%+v err=%v", iter, oc.id, oc.r, oc.err)
			}
		}
		if nRegistered != 1 || nExceeded != 1 || loser == nil {
			t.Fatalf("iter %d want exactly one registered and one exceeds-balance, got %+v", iter, res)
		}

		// 按实际先成功者确定账单剩余余额；两种结果都未付清。
		var remaining int64
		switch winner.id {
		case "p700":
			remaining = 300 // 已付 700、余额 300。
		case "p400":
			remaining = 600 // 已付 400、余额 600。
		default:
			t.Fatalf("iter %d unknown winner %q", iter, winner.id)
		}
		if winner.r.BillBalance != remaining || winner.r.Settled {
			t.Fatalf("iter %d winner %q result: %+v, want balance=%d settled=false",
				iter, winner.id, winner.r, remaining)
		}

		// 失败的一笔不得部分计入已付，也不得产生负余额或误报付清。
		b, err := s.GetBill(acct, jan(2026))
		if err != nil {
			t.Fatalf("iter %d get bill: %v", iter, err)
		}
		if b.Paid != winner.amount || b.Balance != remaining || b.Settled || b.Balance < 0 {
			t.Fatalf("iter %d bill after contention: paid=%d balance=%d settled=%v, want paid=%d balance=%d",
				iter, b.Paid, b.Balance, b.Settled, winner.amount, remaining)
		}
		// 应付、税额与用量明细保持不变。
		if b.TotalDue != 1000 || b.Tax != 0 || b.TotalUsage != 0 ||
			b.MonthlyFee != 1000 || b.OverageFee != 0 {
			t.Fatalf("iter %d bill immutable fields changed: %+v", iter, b)
		}
		// 账户状态中的账单摘要必须反映同一种实际结果。
		if bs := billSummary1000(t, s, acct); bs.TotalDue != 1000 ||
			bs.Paid != winner.amount || bs.Balance != remaining || bs.Settled {
			t.Fatalf("iter %d status summary diverges: %+v", iter, bs)
		}

		// 被拒绝的那笔不占用付款标识：用它原来的标识、把金额改为此时的剩余余额，
		// 应能作为首次成功付款登记并结清，而不是报标识冲突。
		topUp, err := s.RecordPayment(acct, loser.id, jan(2026), remaining)
		if errors.Is(err, ErrPaymentConflict) {
			t.Fatalf("iter %d rejected payment id %q was consumed: %v", iter, loser.id, err)
		}
		if err != nil || !topUp.Registered || topUp.Payment.PaymentID != loser.id ||
			topUp.Payment.Amount != remaining || topUp.BillBalance != 0 || !topUp.Settled {
			t.Fatalf("iter %d reuse rejected id %q for remaining %d: r=%+v err=%v",
				iter, loser.id, remaining, topUp, err)
		}

		// 此后原样重报最先成功的那笔：仍按已有规则返回它首次登记后的余额与未付清
		// 状态（历史结果），Registered 为 false，不再次增加已付金额。
		replay, err := s.RecordPayment(acct, winner.id, jan(2026), winner.amount)
		if err != nil || replay.Registered ||
			replay.BillBalance != remaining || replay.Settled {
			t.Fatalf("iter %d replay winner %q: r=%+v err=%v, want historical balance=%d settled=false",
				iter, winner.id, replay, err, remaining)
		}

		// 账单查询与账户状态显示当前实际状态：已付 1000、余额 0、已付清，
		// 不被旧付款返回的历史结果覆盖。
		b, _ = s.GetBill(acct, jan(2026))
		if b.Paid != 1000 || b.Balance != 0 || !b.Settled {
			t.Fatalf("iter %d final bill: paid=%d balance=%d settled=%v, want 1000/0/true",
				iter, b.Paid, b.Balance, b.Settled)
		}
		if bs := billSummary1000(t, s, acct); bs.Paid != 1000 || bs.Balance != 0 || !bs.Settled {
			t.Fatalf("iter %d final status summary: %+v", iter, bs)
		}
	}
}

// TestConcurrentDistinctPaymentsExactlySettleBill 覆盖结清边界：
// 初始余额 1000，两笔标识不同的付款 700 与 300 并发到达，合计划好等于应付。
// 两笔都应首次登记成功，最终付清；不能把这种合法的分次付款当成超额拒绝。
// 只有实际把余额结清为 0 的那笔返回付清，另一笔返回它登记后的剩余余额。
func TestConcurrentDistinctPaymentsExactlySettleBill(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("b%d", iter)
		mustBill1000(t, s, acct)

		res := contendOnce(s, acct,
			paymentOutcome{id: "p700", amount: 700},
			paymentOutcome{id: "p300", amount: 300},
		)

		byID := make(map[string]paymentOutcome, 2)
		nSettled := 0
		for _, oc := range res {
			// 两笔都必须首次登记成功：恰够结清的分次付款不是超额付款。
			if oc.err != nil || !oc.r.Registered {
				t.Fatalf("iter %d payment %q should register: r=%+v err=%v", iter, oc.id, oc.r, oc.err)
			}
			byID[oc.id] = oc
			if oc.r.Settled {
				if oc.r.BillBalance != 0 {
					t.Fatalf("iter %d settled payment %q has non-zero balance %d", iter, oc.id, oc.r.BillBalance)
				}
				nSettled++
			}
		}
		// 恰好一笔（后登记、实际结清余额的那笔）返回付清。
		if nSettled != 1 {
			t.Fatalf("iter %d want exactly one settled result, got %d: %+v", iter, nSettled, res)
		}

		// 先登记的一笔返回其登记后的剩余余额 = 1000 - 自身金额，且未付清；
		// 后登记的一笔余额为 0、已付清。两种先后顺序都接受。
		for _, oc := range res {
			wantBalance := int64(0)
			if !oc.r.Settled {
				wantBalance = 1000 - oc.amount
			}
			if oc.r.BillBalance != wantBalance || oc.r.Settled != (wantBalance == 0) {
				t.Fatalf("iter %d payment %q result: balance=%d settled=%v, want balance=%d",
					iter, oc.id, oc.r.BillBalance, oc.r.Settled, wantBalance)
			}
		}

		// 最终账单与状态：已付 1000、余额 0、已付清；应付、税额与用量不变。
		b, err := s.GetBill(acct, jan(2026))
		if err != nil {
			t.Fatalf("iter %d get bill: %v", iter, err)
		}
		if b.Paid != 1000 || b.Balance != 0 || !b.Settled {
			t.Fatalf("iter %d bill: paid=%d balance=%d settled=%v, want 1000/0/true",
				iter, b.Paid, b.Balance, b.Settled)
		}
		if b.TotalDue != 1000 || b.Tax != 0 || b.TotalUsage != 0 {
			t.Fatalf("iter %d bill immutable fields changed: %+v", iter, b)
		}
		if bs := billSummary1000(t, s, acct); bs.Paid != 1000 || bs.Balance != 0 || !bs.Settled {
			t.Fatalf("iter %d status summary: %+v", iter, bs)
		}
	}
}

// TestDistinctPaymentContentionBothOrdersDeterministic 以确定的顺序锁住两种可能
// 的获胜结果（并发测试不保证每次都调度出两种顺序，这里把两条路径各自完整走一遍）：
// 失败笔不占标识，可改用剩余余额以原标识首次登记并结清；随后重报最先成功的付款
// 只返回其历史结果，不再次扣款。
func TestDistinctPaymentContentionBothOrdersDeterministic(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, TaxRateBasisPoints: 0})

	// 顺序一：700 先成功（已付 700、余额 300、未付清），400 超额失败。
	mustBill1000(t, s, "big-first")
	r700, err := s.RecordPayment("big-first", "p700", jan(2026), 700)
	if err != nil || !r700.Registered || r700.BillBalance != 300 || r700.Settled {
		t.Fatalf("700 first: r=%+v err=%v", r700, err)
	}
	if _, err := s.RecordPayment("big-first", "p400", jan(2026), 400); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("400 after 700 should exceed balance: %v", err)
	}
	// p400 未被占用：改金额 300 首次成功并结清。
	topUp, err := s.RecordPayment("big-first", "p400", jan(2026), 300)
	if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
		t.Fatalf("reuse p400 for 300: r=%+v err=%v", topUp, err)
	}
	// 重报 p700：返回首次登记后的历史结果（余额 300、未付清），不再次扣款。
	replay, err := s.RecordPayment("big-first", "p700", jan(2026), 700)
	if err != nil || replay.Registered || replay.BillBalance != 300 || replay.Settled {
		t.Fatalf("replay p700: r=%+v err=%v, want historical 300/false", replay, err)
	}
	if b, _ := s.GetBill("big-first", jan(2026)); b.Paid != 1000 || b.Balance != 0 || !b.Settled {
		t.Fatalf("big-first final bill: %+v", b)
	}

	// 顺序二：400 先成功（已付 400、余额 600、未付清），700 超额失败。
	mustBill1000(t, s, "small-first")
	r400, err := s.RecordPayment("small-first", "p400", jan(2026), 400)
	if err != nil || !r400.Registered || r400.BillBalance != 600 || r400.Settled {
		t.Fatalf("400 first: r=%+v err=%v", r400, err)
	}
	if _, err := s.RecordPayment("small-first", "p700", jan(2026), 700); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("700 after 400 should exceed balance: %v", err)
	}
	// p700 未被占用：改金额 600 首次成功并结清。
	topUp, err = s.RecordPayment("small-first", "p700", jan(2026), 600)
	if err != nil || !topUp.Registered || topUp.BillBalance != 0 || !topUp.Settled {
		t.Fatalf("reuse p700 for 600: r=%+v err=%v", topUp, err)
	}
	// 重报 p400：返回首次登记后的历史结果（余额 600、未付清），不再次扣款。
	replay, err = s.RecordPayment("small-first", "p400", jan(2026), 400)
	if err != nil || replay.Registered || replay.BillBalance != 600 || replay.Settled {
		t.Fatalf("replay p400: r=%+v err=%v, want historical 600/false", replay, err)
	}
	if b, _ := s.GetBill("small-first", jan(2026)); b.Paid != 1000 || b.Balance != 0 || !b.Settled {
		t.Fatalf("small-first final bill: %+v", b)
	}
}
