package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// mustBill1000 新建账户并出一张应付与余额均为 1000 分、尚未收到付款的账单。
func mustBill1000(t *testing.T, s *Service, accountID string) {
	t.Helper()
	mustAccount(t, s, accountID)
	if err := s.Subscribe(accountID, "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBill(accountID, jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.TotalDue != 1000 || b.Balance != 1000 || b.Paid != 0 || b.Settled {
		t.Fatalf("initial bill: %+v", b)
	}
}

// payOutcome 收集一次并发付款的结果。
type payOutcome struct {
	r   PaymentResult
	err error
}

// racePayments 在同一账单上同时提交两笔不同标识的付款，返回各自结果。
func racePayments(s *Service, accountID string, id1 string, amount1 int64, id2 string, amount2 int64) [2]payOutcome {
	var out [2]payOutcome
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		r, err := s.RecordPayment(accountID, id1, jan(2026), amount1)
		out[0] = payOutcome{r, err}
	}()
	go func() {
		defer wg.Done()
		<-start
		r, err := s.RecordPayment(accountID, id2, jan(2026), amount2)
		out[1] = payOutcome{r, err}
	}()
	close(start)
	wg.Wait()
	return out
}

// TestConcurrentDistinctPaymentsExceedingBalance 覆盖两笔不同付款争用同一笔余额：
// 余额 1000 的账单同时收到 700 与 400 两笔付款，单独提交都合法，但不能一起
// 登记成功——恰好一笔首次登记成功，另一笔返回 ErrPaymentExceedsBalance。
// 允许任意一笔先成功；失败付款不占用标识，也不把任何部分算入已付金额。
func TestConcurrentDistinctPaymentsExceedingBalance(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})

	ids := [2]string{"p700", "p400"}
	amounts := [2]int64{700, 400}

	const n = 32
	for i := 0; i < n; i++ {
		acc := fmt.Sprintf("a%d", i)
		mustBill1000(t, s, acc)

		out := racePayments(s, acc, ids[0], amounts[0], ids[1], amounts[1])

		// 恰好一笔首次登记成功，另一笔报付款超过余额。
		win, lose := -1, -1
		for k, o := range out {
			switch {
			case o.err == nil && o.r.Registered:
				if win != -1 {
					t.Fatalf("iter %d: both payments registered", i)
				}
				win = k
			case errors.Is(o.err, ErrPaymentExceedsBalance):
				lose = k
			default:
				t.Fatalf("iter %d: unexpected outcome for %s: %+v %v", i, ids[k], o.r, o.err)
			}
		}
		if win == -1 || lose == -1 || win == lose {
			t.Fatalf("iter %d: win=%d lose=%d, outcomes=%+v", i, win, lose, out)
		}

		winAmount := amounts[win]
		balance := 1000 - winAmount
		// 胜者返回其登记后的余额，未付清；不得误报付清。
		if out[win].r.BillBalance != balance || out[win].r.Settled {
			t.Fatalf("iter %d: winner result: %+v, want balance=%d settled=false", i, out[win].r, balance)
		}

		// 账单查询反映同一种实际结果：只计入胜者的全额付款，
		// 失败付款不计入任何部分；余额不为负，未付清；应付、税额与用量不变。
		b, err := s.GetBill(acc, jan(2026))
		if err != nil {
			t.Fatal(err)
		}
		if b.Paid != winAmount || b.Balance != balance || b.Settled {
			t.Fatalf("iter %d: bill after race: paid=%d balance=%d settled=%v, want %d/%d/false",
				i, b.Paid, b.Balance, b.Settled, winAmount, balance)
		}
		if b.TotalDue != 1000 || b.Tax != 0 || b.TotalUsage != 0 {
			t.Fatalf("iter %d: bill amounts changed: %+v", i, b)
		}
		st, err := s.Status(acc)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Bills) != 1 || st.Bills[0].Paid != winAmount || st.Bills[0].Balance != balance ||
			st.Bills[0].Settled || st.Bills[0].TotalDue != 1000 {
			t.Fatalf("iter %d: status bill summary: %+v", i, st.Bills)
		}

		// 被拒绝的付款不占用标识：用原标识改付此时的剩余余额，
		// 应作为首次成功付款登记并结清账单，不报标识冲突。
		r3, err := s.RecordPayment(acc, ids[lose], jan(2026), balance)
		if err != nil || !r3.Registered || r3.BillBalance != 0 || !r3.Settled {
			t.Fatalf("iter %d: loser retry with remaining balance: %+v %v", i, r3, err)
		}

		// 原样重报最先成功的付款：按已有规则返回其首次登记后的历史余额与
		// 未付清状态，不再次增加已付金额。
		r4, err := s.RecordPayment(acc, ids[win], jan(2026), winAmount)
		if err != nil || r4.Registered || r4.BillBalance != balance || r4.Settled {
			t.Fatalf("iter %d: winner replay: %+v %v, want balance=%d settled=false",
				i, r4, err, balance)
		}

		// 账单当前状态：已付 1000、余额零、已付清，不被旧付款的历史结果覆盖。
		b, err = s.GetBill(acc, jan(2026))
		if err != nil {
			t.Fatal(err)
		}
		if b.Paid != 1000 || b.Balance != 0 || !b.Settled {
			t.Fatalf("iter %d: bill after settle+replay: paid=%d balance=%d settled=%v",
				i, b.Paid, b.Balance, b.Settled)
		}
		if b.TotalDue != 1000 || b.Tax != 0 || b.TotalUsage != 0 {
			t.Fatalf("iter %d: bill amounts changed after settle: %+v", i, b)
		}
		st, err = s.Status(acc)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Bills) != 1 || st.Bills[0].Paid != 1000 || st.Bills[0].Balance != 0 ||
			!st.Bills[0].Settled || st.Bills[0].TotalDue != 1000 {
			t.Fatalf("iter %d: status after settle: %+v", i, st.Bills)
		}
	}
}

// TestConcurrentDistinctPaymentsExactSettlement 保留金额恰好够结清的边界：
// 余额 1000 的账单同时收到不同标识的 700 与 300，两笔都应首次登记成功并
// 最终付清；只有实际结清余额的那笔返回付清，另一笔返回其登记后的剩余余额。
// 不能把合法的分次付款当成超额付款拒绝。
func TestConcurrentDistinctPaymentsExactSettlement(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})

	ids := [2]string{"p700", "p300"}
	amounts := [2]int64{700, 300}

	const n = 32
	for i := 0; i < n; i++ {
		acc := fmt.Sprintf("a%d", i)
		mustBill1000(t, s, acc)

		out := racePayments(s, acc, ids[0], amounts[0], ids[1], amounts[1])

		// 两笔都必须首次登记成功，不得报超额。
		settledCount := 0
		for k, o := range out {
			if o.err != nil || !o.r.Registered {
				t.Fatalf("iter %d: payment %s rejected: %+v %v", i, ids[k], o.r, o.err)
			}
			if o.r.Settled {
				settledCount++
				// 实际结清余额的那笔：返回余额零、已付清。
				if o.r.BillBalance != 0 {
					t.Fatalf("iter %d: settling payment %s balance=%d, want 0", i, ids[k], o.r.BillBalance)
				}
			} else {
				// 另一笔返回其登记后的剩余余额，未付清。
				if want := 1000 - amounts[k]; o.r.BillBalance != want {
					t.Fatalf("iter %d: payment %s balance=%d, want %d", i, ids[k], o.r.BillBalance, want)
				}
			}
		}
		// 恰好一笔（后登记、实际结清余额的那笔）返回付清。
		if settledCount != 1 {
			t.Fatalf("iter %d: settled results = %d, want 1; outcomes=%+v", i, settledCount, out)
		}

		// 最终付清：已付 1000、余额零；账单查询与账户状态一致；
		// 应付、税额与用量明细保持不变。
		b, err := s.GetBill(acc, jan(2026))
		if err != nil {
			t.Fatal(err)
		}
		if b.Paid != 1000 || b.Balance != 0 || !b.Settled {
			t.Fatalf("iter %d: bill after exact race: paid=%d balance=%d settled=%v",
				i, b.Paid, b.Balance, b.Settled)
		}
		if b.TotalDue != 1000 || b.Tax != 0 || b.TotalUsage != 0 {
			t.Fatalf("iter %d: bill amounts changed: %+v", i, b)
		}
		st, err := s.Status(acc)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Bills) != 1 || st.Bills[0].Paid != 1000 || st.Bills[0].Balance != 0 ||
			!st.Bills[0].Settled || st.Bills[0].TotalDue != 1000 {
			t.Fatalf("iter %d: status bill summary: %+v", i, st.Bills)
		}
	}
}
