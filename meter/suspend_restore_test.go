package meter

import (
	"errors"
	"testing"
	"time"
)

// 多账单并存场景的公共准备：月费 1000 分、超额单价与税率均为零的套餐，
// 自 2026-01-01 00:00 UTC 起持续有效的订阅，并在 2026-04-01 00:00 UTC
// 生成一月、二月、三月三张账单（各应付 1000 分）。
// 一月截止 2026-02-08 00:00、二月截止 2026-03-08 00:00（均已过去），
// 三月截止 2026-04-08 00:00（尚未到期）。
func newMultiBillAccount(t *testing.T, accountID string) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 4, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, accountID)
	if err := s.Subscribe(accountID, "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	for _, m := range []Month{jan(2026), feb(2026), mar(2026)} {
		b, err := s.CreateBill(accountID, m)
		if err != nil {
			t.Fatalf("create bill %s: %v", m, err)
		}
		if b.TotalDue != 1000 || b.Balance != 1000 || b.Paid != 0 || b.Settled {
			t.Fatalf("bill %s initial state: %+v", m, b)
		}
		if b.MonthlyFee != 1000 || b.OverageFee != 0 || b.Tax != 0 || b.TotalUsage != 0 {
			t.Fatalf("bill %s amounts: %+v", m, b)
		}
	}
	return s, clk
}

func mustStatus(t *testing.T, s *Service, accountID string) AccountStatus {
	t.Helper()
	st, err := s.Status(accountID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return st
}

func billSummaryOf(t *testing.T, st AccountStatus, m Month) BillSummary {
	t.Helper()
	for _, b := range st.Bills {
		if b.Period == m {
			return b
		}
	}
	t.Fatalf("status missing bill %s: %+v", m, st.Bills)
	return BillSummary{}
}

// TestMultiBillPartialPaymentsKeepSuspended 多张账单并存时，恢复取决于是否还存在
// 已到截止时刻且余额大于零的账单：付清其中一张不替其他账单还款，
// 尚未到期的余额也不阻止恢复。
func TestMultiBillPartialPaymentsKeepSuspended(t *testing.T) {
	s, _ := newMultiBillAccount(t, "a")

	// 三张账单均未付款：一月、二月已到期，账户欠费停用；订阅本身仍有效。
	st := mustStatus(t, s, "a")
	if !st.Suspended || !st.Subscribed {
		t.Fatalf("initial status: suspended=%v subscribed=%v", st.Suspended, st.Subscribed)
	}

	// 停用期间上报用量被拒绝：用量发生在有效订阅期间、所属四月尚未出账，
	// 唯一的拒绝理由是欠费停用。
	ev := Event{AccountID: "a", EventID: "e1", At: utc(2026, 4, 1, 0, 0), Quantity: 5}
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrSuspended) {
		t.Fatalf("event while suspended: %v", err)
	}
	// 被拒绝的用量不增加当月累计量。
	if u, err := s.MonthlyUsage("a", Month{Year: 2026, Month: time.April}); err != nil || u.Total != 0 {
		t.Fatalf("usage after rejected event: %+v %v", u, err)
	}

	// 向二月账单登记 300 分：二月余额变 700，一月、三月仍各欠 1000，账户继续停用。
	r, err := s.RecordPayment("a", "pay-feb-1", feb(2026), 300)
	if err != nil || !r.Registered || r.BillBalance != 700 || r.Settled {
		t.Fatalf("feb partial payment: %+v %v", r, err)
	}
	st = mustStatus(t, s, "a")
	if !st.Suspended {
		t.Fatalf("restored with jan and feb still due: %+v", st)
	}
	if b := billSummaryOf(t, st, jan(2026)); b.Paid != 0 || b.Balance != 1000 || b.Settled {
		t.Fatalf("jan after feb payment: %+v", b)
	}
	if b := billSummaryOf(t, st, feb(2026)); b.Paid != 300 || b.Balance != 700 || b.Settled {
		t.Fatalf("feb after partial payment: %+v", b)
	}
	if b := billSummaryOf(t, st, mar(2026)); b.Paid != 0 || b.Balance != 1000 || b.Settled {
		t.Fatalf("mar after feb payment: %+v", b)
	}

	// 付清一月账单：只改变一月的付款状态，二月剩余欠款仍使账户停用。
	r, err = s.RecordPayment("a", "pay-jan", jan(2026), 1000)
	if err != nil || !r.Registered || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("jan payment: %+v %v", r, err)
	}
	if st = mustStatus(t, s, "a"); !st.Suspended {
		t.Fatalf("restored while feb still owes 700: %+v", st)
	}

	// 结清二月剩余 700 分：账户立即恢复；三月仍欠 1000 分但尚未到期，
	// 不应被当作恢复条件。
	r, err = s.RecordPayment("a", "pay-feb-2", feb(2026), 700)
	if err != nil || !r.Registered || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("feb final payment: %+v %v", r, err)
	}
	st = mustStatus(t, s, "a")
	if st.Suspended || !st.Subscribed {
		t.Fatalf("status after settling due bills: suspended=%v subscribed=%v", st.Suspended, st.Subscribed)
	}

	// 账单查询与账户状态反映各自的付款结果；原应付总额、税额与用量明细保持出账时的值。
	for m, want := range map[Month]struct {
		paid, balance int64
		settled       bool
	}{
		jan(2026): {1000, 0, true},
		feb(2026): {1000, 0, true},
		mar(2026): {0, 1000, false},
	} {
		b, err := s.GetBill("a", m)
		if err != nil {
			t.Fatalf("get bill %s: %v", m, err)
		}
		if b.Paid != want.paid || b.Balance != want.balance || b.Settled != want.settled {
			t.Fatalf("bill %s payment state: %+v", m, b)
		}
		if b.TotalDue != 1000 || b.Tax != 0 || b.TotalUsage != 0 || b.MonthlyFee != 1000 || b.OverageFee != 0 {
			t.Fatalf("bill %s amounts changed: %+v", m, b)
		}
		sum := billSummaryOf(t, st, m)
		if sum.Paid != want.paid || sum.Balance != want.balance || sum.Settled != want.settled || sum.TotalDue != 1000 {
			t.Fatalf("status bill %s: %+v", m, sum)
		}
	}

	// 停用期间被拒绝的事件不占用事件标识：结清到期欠款后以相同内容再次提交，
	// 应被首次接收，且只增加该事件的数量。
	res, err := s.RecordEvent(ev)
	if err != nil || !res.Accepted || res.Period != (Month{Year: 2026, Month: time.April}) {
		t.Fatalf("resubmit after restore: %+v %v", res, err)
	}
	if u, _ := s.MonthlyUsage("a", Month{Year: 2026, Month: time.April}); u.Total != 5 {
		t.Fatalf("usage after resubmit: %+v", u)
	}
	// 完全相同重报不再累计。
	res, err = s.RecordEvent(ev)
	if err != nil || res.Accepted {
		t.Fatalf("identical replay: %+v %v", res, err)
	}
	if u, _ := s.MonthlyUsage("a", Month{Year: 2026, Month: time.April}); u.Total != 5 {
		t.Fatalf("usage after replay: %+v", u)
	}
}

// TestMultiBillRestoreOrderIndependent 先结清一月或先结清二月都遵守同一规则：
// 只要还有另一张已到期的账单有余额，账户就不恢复，付款顺序不决定是否提前恢复。
func TestMultiBillRestoreOrderIndependent(t *testing.T) {
	s, _ := newMultiBillAccount(t, "a")

	// 与 TestMultiBillPartialPaymentsKeepSuspended 相反的顺序：先结清一月。
	if _, err := s.RecordPayment("a", "pay-jan", jan(2026), 1000); err != nil {
		t.Fatal(err)
	}
	if st := mustStatus(t, s, "a"); !st.Suspended {
		t.Fatalf("restored after settling jan only: %+v", st)
	}
	// 再结清二月，账户恢复；三月未到期余额不影响。
	if _, err := s.RecordPayment("a", "pay-feb", feb(2026), 1000); err != nil {
		t.Fatal(err)
	}
	st := mustStatus(t, s, "a")
	if st.Suspended {
		t.Fatalf("still suspended after settling both due bills: %+v", st)
	}
	if b := billSummaryOf(t, st, mar(2026)); b.Balance != 1000 || b.Settled {
		t.Fatalf("mar should remain unpaid: %+v", b)
	}
}

// TestMultiBillNotYetDueBalanceDoesNotSuspend 三月账单仍有余额时，
// 四月八日零点（截止时刻）前账户保持可接收新用量；恰到截止时刻即重新欠费停用
// 并拒绝新增用量，无需先发生其他业务操作才承认到期。
func TestMultiBillNotYetDueBalanceDoesNotSuspend(t *testing.T) {
	s, clk := newMultiBillAccount(t, "a")

	// 结清一月、二月两张已到期账单，只留未到期的三月余额。
	if _, err := s.RecordPayment("a", "pay-jan", jan(2026), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPayment("a", "pay-feb", feb(2026), 1000); err != nil {
		t.Fatal(err)
	}

	// 截止时刻前一刻：未到期余额不阻止接收新用量。
	clk.t = utc(2026, 4, 7, 23, 59)
	if st := mustStatus(t, s, "a"); st.Suspended {
		t.Fatalf("suspended before mar due: %+v", st)
	}
	res, err := s.RecordEvent(Event{AccountID: "a", EventID: "before-due", At: utc(2026, 4, 7, 12, 0), Quantity: 2})
	if err != nil || !res.Accepted {
		t.Fatalf("event before mar due: %+v %v", res, err)
	}

	// 恰到三月截止时刻：仅推进时钟即重新停用，无需其他业务操作。
	clk.t = utc(2026, 4, 8, 0, 0)
	st := mustStatus(t, s, "a")
	if !st.Suspended || !st.Subscribed {
		t.Fatalf("status at mar due: suspended=%v subscribed=%v", st.Suspended, st.Subscribed)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "at-due", At: utc(2026, 4, 7, 13, 0), Quantity: 1}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("event at mar due: %v", err)
	}
	// 到期前已接收的用量不受影响，被拒绝的事件不累计。
	if u, _ := s.MonthlyUsage("a", Month{Year: 2026, Month: time.April}); u.Total != 2 {
		t.Fatalf("usage at mar due: %+v", u)
	}

	// 结清三月账单后恢复；整个过程中订阅持续有效。
	if _, err := s.RecordPayment("a", "pay-mar", mar(2026), 1000); err != nil {
		t.Fatal(err)
	}
	st = mustStatus(t, s, "a")
	if st.Suspended || !st.Subscribed {
		t.Fatalf("status after settling mar: suspended=%v subscribed=%v", st.Suspended, st.Subscribed)
	}
}
