package meter

import (
	"errors"
	"testing"
	"time"
)

// 多张账单并存时的欠费停用/付款恢复回归：
// 账户是否恢复只取决于“是否还存在已到付款截止时刻且余额大于零的账单”。
// 付清一张账单不替其他账单还款；尚未到期的账单余额不阻止恢复；
// 到期时刻一到即承认欠费，无需先发生其他业务操作。
//
// 场景：一份持续有效的订阅，套餐月费 1000 分、超额单价与税率均为零，
// 2026 年 1、2、3 月各出一张 1000 分账单。付款截止时刻（均为 UTC）：
//
//	一月账单 2026-02-08 00:00
//	二月账单 2026-03-08 00:00
//	三月账单 2026-04-08 00:00
//
// 当前时刻为 2026-04-01 00:00 时，一月、二月均已到期，三月尚未到期。

// multiBillFixture 构造上述账户：一月初开通的持续订阅，并在时钟拨到
// 四月一日零点后补齐一、二、三月三张账单。
func multiBillFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	clk.t = utc(2026, 4, 1, 0, 0)
	for _, m := range []Month{jan(2026), feb(2026), mar(2026)} {
		b, err := s.CreateBill("a", m)
		if err != nil {
			t.Fatalf("create bill %s: %v", m, err)
		}
		if b.TotalDue != 1000 || b.Tax != 0 || b.Balance != 1000 || b.Paid != 0 || b.Settled {
			t.Fatalf("bill %s initial state: %+v", m, b)
		}
	}
	return s, clk
}

// wantBill 检查指定账单的付款状态，以及出账时即固定的应付总额、税额、
// 用量明细与截止时刻。
func wantBill(t *testing.T, s *Service, period Month, dueAt time.Time, paid, balance int64, settled bool) {
	t.Helper()
	b, err := s.GetBill("a", period)
	if err != nil {
		t.Fatalf("get bill %s: %v", period, err)
	}
	if b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill %s payment state: paid=%d balance=%d settled=%v, want paid=%d balance=%d settled=%v",
			period, b.Paid, b.Balance, b.Settled, paid, balance, settled)
	}
	// 金额与用量明细保持出账时的值，不随后续付款变化。
	if b.TotalDue != 1000 || b.MonthlyFee != 1000 || b.OverageFee != 0 || b.Tax != 0 {
		t.Fatalf("bill %s amounts changed: %+v", period, b)
	}
	if b.TotalUsage != 0 || b.IncludedUnits != 0 || b.OverageUnits != 0 {
		t.Fatalf("bill %s usage detail changed: %+v", period, b)
	}
	if !b.DueAt.Equal(dueAt) {
		t.Fatalf("bill %s due at = %v, want %v", period, b.DueAt, dueAt)
	}
}

func wantSuspended(t *testing.T, s *Service, want bool) AccountStatus {
	t.Helper()
	st, err := s.Status("a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Suspended != want {
		t.Fatalf("suspended = %v, want %v; bills: %+v", st.Suspended, want, st.Bills)
	}
	return st
}

func TestSuspensionMultipleBillsIndependentPayments(t *testing.T) {
	s, clk := multiBillFixture(t)

	janDue := utc(2026, 2, 8, 0, 0)
	febDue := utc(2026, 3, 8, 0, 0)
	marDue := utc(2026, 4, 8, 0, 0)

	// 四月一日零点：一月、二月已到期欠费，三月未到期；账户停用，订阅仍有效。
	st := wantSuspended(t, s, true)
	if !st.Subscribed {
		t.Fatalf("subscription must remain active while suspended: %+v", st)
	}
	if st.CurrentTerms.MonthlyFee != 1000 || st.CurrentTerms.PlanID != "p" {
		t.Fatalf("current terms changed: %+v", st.CurrentTerms)
	}

	// 即使用量发生在有效订阅期间、所属四月尚未出账，停用期间也必须拒绝。
	aprEvent := Event{AccountID: "a", EventID: "evt-apr", At: utc(2026, 4, 1, 0, 0), Quantity: 4}
	if _, err := s.RecordEvent(aprEvent); !errors.Is(err, ErrSuspended) {
		t.Fatalf("apr event while suspended: %v", err)
	}
	// 被拒绝的用量不增加当月累计量。
	if u, _ := s.MonthlyUsage("a", apr(2026)); u.Total != 0 {
		t.Fatalf("apr usage after rejection = %d, want 0", u.Total)
	}

	// 先向二月账单登记 300：只改变二月本身，一月、三月不受影响，继续停用。
	r, err := s.RecordPayment("a", "pay-feb-1", feb(2026), 300)
	if err != nil || !r.Registered || r.BillBalance != 700 || r.Settled {
		t.Fatalf("feb partial payment: %+v %v", r, err)
	}
	wantBill(t, s, jan(2026), janDue, 0, 1000, false)
	wantBill(t, s, feb(2026), febDue, 300, 700, false)
	wantBill(t, s, mar(2026), marDue, 0, 1000, false)
	wantSuspended(t, s, true)

	// 再付清一月：二月仍欠 700 且已到期，账户必须继续停用。
	// 付清一月没有替二月还款。
	if r, err := s.RecordPayment("a", "pay-jan", jan(2026), 1000); err != nil ||
		!r.Registered || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("jan payment in full: %+v %v", r, err)
	}
	wantBill(t, s, jan(2026), janDue, 1000, 0, true)
	wantBill(t, s, feb(2026), febDue, 300, 700, false)
	wantSuspended(t, s, true)
	// 停用仍生效：四月新用量继续被拒。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-blocked", At: utc(2026, 4, 1, 0, 0), Quantity: 1}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("event with jan settled but feb overdue: %v", err)
	}

	// 结清二月剩余 700：到期账单全部付清，账户立即恢复；
	// 三月仍欠 1000 但截止时刻（四月八日）未到，不阻止恢复。
	if r, err := s.RecordPayment("a", "pay-feb-2", feb(2026), 700); err != nil ||
		!r.Registered || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("feb final payment: %+v %v", r, err)
	}
	st = wantSuspended(t, s, false)
	if !st.Subscribed {
		t.Fatalf("subscription must remain active after recovery: %+v", st)
	}
	wantBill(t, s, jan(2026), janDue, 1000, 0, true)
	wantBill(t, s, feb(2026), febDue, 1000, 0, true)
	wantBill(t, s, mar(2026), marDue, 0, 1000, false)

	// Status 的账单摘要按账期排列并反映各账期独立的付款结果。
	if len(st.Bills) != 3 {
		t.Fatalf("status bills = %+v", st.Bills)
	}
	wantSummary := []struct {
		period        Month
		paid, balance int64
		settled       bool
	}{
		{jan(2026), 1000, 0, true},
		{feb(2026), 1000, 0, true},
		{mar(2026), 0, 1000, false},
	}
	for i, w := range wantSummary {
		got := st.Bills[i]
		if got.Period != w.period || got.TotalDue != 1000 || got.Paid != w.paid ||
			got.Balance != w.balance || got.Settled != w.settled || !got.DueAt.Equal(w.period.End().AddDate(0, 0, 7)) {
			t.Fatalf("status bills[%d] = %+v, want %+v", i, got, w)
		}
	}

	// 结清到期欠款后，停用期间被拒的同一事件以相同内容再次提交应被首次接收，
	// 且只增加这一个事件的数量（证明此前被拒不占用事件标识、不累计用量）。
	r2, err := s.RecordEvent(aprEvent)
	if err != nil || !r2.Accepted || r2.Period != apr(2026) {
		t.Fatalf("apr event re-submitted after recovery: %+v %v", r2, err)
	}
	if u, _ := s.MonthlyUsage("a", apr(2026)); u.Total != 4 {
		t.Fatalf("apr usage = %d, want 4 (rejected attempt must not count)", u.Total)
	}
	// 再报同一条为去重重报，不重复累计。
	if r3, err := s.RecordEvent(aprEvent); err != nil || r3.Accepted {
		t.Fatalf("identical replay accumulates again: %+v %v", r3, err)
	}
	if u, _ := s.MonthlyUsage("a", apr(2026)); u.Total != 4 {
		t.Fatalf("apr usage after replay = %d, want 4", u.Total)
	}

	// 三月账单仍有余额，但四月八日零点前账户保持可接收新用量。
	clk.t = utc(2026, 4, 7, 23, 59)
	wantSuspended(t, s, false)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-apr-2", At: utc(2026, 4, 7, 23, 59), Quantity: 5}); err != nil {
		t.Fatalf("event just before mar due: %v", err)
	}
	if u, _ := s.MonthlyUsage("a", apr(2026)); u.Total != 9 {
		t.Fatalf("apr usage = %d, want 9", u.Total)
	}

	// 恰到三月截止时刻：无需先发生其他业务操作，Status 即显示重新停用。
	clk.t = utc(2026, 4, 8, 0, 0)
	st = wantSuspended(t, s, true)
	if !st.Subscribed {
		t.Fatalf("subscription must still be active at mar due time: %+v", st)
	}
	// 停用再次拒绝新增用量；被拒用量不增加累计、不占用标识（同路径规则）。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-apr-3", At: utc(2026, 4, 8, 0, 0), Quantity: 2}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("event exactly at mar due: %v", err)
	}
	if u, _ := s.MonthlyUsage("a", apr(2026)); u.Total != 9 {
		t.Fatalf("apr usage = %d after due rejection, want 9", u.Total)
	}
	// 一月、二月已结清状态不被三月到期影响。
	wantBill(t, s, jan(2026), janDue, 1000, 0, true)
	wantBill(t, s, feb(2026), febDue, 1000, 0, true)
	wantBill(t, s, mar(2026), marDue, 0, 1000, false)
}

// TestSuspensionRecoveryOrderIndependent 反向付款顺序：先全额付清二月、
// 再付清一月，恢复规则必须与先付一月一致——只要仍有一张到期账单有余额，
// 就不能因付款顺序而提前恢复；未到期的三月同样不参与判定。
func TestSuspensionRecoveryOrderIndependent(t *testing.T) {
	s, _ := multiBillFixture(t)

	// 两张到期账单并存：先付清二月，一月仍到期欠费，不能恢复。
	if r, err := s.RecordPayment("a", "pay-feb", feb(2026), 1000); err != nil ||
		!r.Registered || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("feb paid first: %+v %v", r, err)
	}
	wantBill(t, s, jan(2026), utc(2026, 2, 8, 0, 0), 0, 1000, false)
	wantBill(t, s, feb(2026), utc(2026, 3, 8, 0, 0), 1000, 0, true)
	wantSuspended(t, s, true)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-apr", At: utc(2026, 4, 1, 0, 0), Quantity: 4}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("event with only jan overdue: %v", err)
	}

	// 再付清一月：到期账单全部结清，立即恢复；三月未到期不阻止。
	if r, err := s.RecordPayment("a", "pay-jan", jan(2026), 1000); err != nil ||
		!r.Registered || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("jan paid second: %+v %v", r, err)
	}
	st := wantSuspended(t, s, false)
	if !st.Subscribed {
		t.Fatalf("subscription must remain active: %+v", st)
	}
	wantBill(t, s, mar(2026), utc(2026, 4, 8, 0, 0), 0, 1000, false)

	// 恢复后四月用量可被首次接收（此前被拒不累计、不占标识）。
	if r2, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-apr", At: utc(2026, 4, 1, 0, 0), Quantity: 4}); err != nil ||
		!r2.Accepted || r2.Period != apr(2026) {
		t.Fatalf("apr event after reverse-order recovery: %+v %v", r2, err)
	}
	if u, _ := s.MonthlyUsage("a", apr(2026)); u.Total != 4 {
		t.Fatalf("apr usage = %d, want 4", u.Total)
	}
}
