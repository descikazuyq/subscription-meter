package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) get() time.Time { return c.t }

func newTestService(start time.Time) (*Service, *fakeClock) {
	c := &fakeClock{t: start.UTC()}
	return NewServiceWithClock(c.get), c
}

func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

func jan(y int) Month { return Month{Year: y, Month: time.January} }
func feb(y int) Month { return Month{Year: y, Month: time.February} }
func mar(y int) Month { return Month{Year: y, Month: time.March} }

func mustPlan(t *testing.T, s *Service, p Plan) {
	t.Helper()
	if err := s.CreatePlan(p); err != nil {
		t.Fatalf("create plan %q: %v", p.ID, err)
	}
}

func mustAccount(t *testing.T, s *Service, id string) {
	t.Helper()
	if err := s.CreateAccount(id); err != nil {
		t.Fatalf("create account %q: %v", id, err)
	}
}

func TestCreateAccountAndPlanValidation(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustAccount(t, s, "acme")
	if err := s.CreateAccount("acme"); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("dup account: %v", err)
	}
	if err := s.CreateAccount(""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty account: %v", err)
	}

	good := Plan{ID: "p", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000}
	mustPlan(t, s, good)
	if err := s.CreatePlan(good); !errors.Is(err, ErrPlanExists) {
		t.Fatalf("dup plan: %v", err)
	}
	for _, bad := range []Plan{
		{ID: "", TaxRateBasisPoints: 0},
		{ID: "n1", MonthlyFee: -1, TaxRateBasisPoints: 0},
		{ID: "n2", IncludedUnits: -1, TaxRateBasisPoints: 0},
		{ID: "n3", OveragePrice: -1, TaxRateBasisPoints: 0},
		{ID: "n4", TaxRateBasisPoints: -1},
		{ID: "n5", TaxRateBasisPoints: 10001},
	} {
		if err := s.CreatePlan(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("plan %+v expected invalid, got %v", bad, err)
		}
	}
	// 失败不改变已有记录。
	if err := s.UpdatePlan(Plan{ID: "p", TaxRateBasisPoints: 20000}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad update: %v", err)
	}
	if got := s.Status2("p"); got != good {
		t.Fatalf("plan mutated after failed update: %+v", got)
	}
	if err := s.UpdatePlan(Plan{ID: "missing", TaxRateBasisPoints: 1}); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

// TestService 内部用：直接读取套餐当前定义。
func (s *Service) Status2(id string) Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plans[id]
}

func TestSubscribeMissingRefsAndSnapshot(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	mustAccount(t, s, "b")

	if err := s.Subscribe("ghost", "p", utc(2026, 1, 15, 12, 0)); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if err := s.Subscribe("a", "ghost", utc(2026, 1, 15, 12, 0)); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("missing plan: %v", err)
	}
	if err := s.Subscribe("a", "", utc(2026, 1, 15, 12, 0)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty plan: %v", err)
	}
	if err := s.Subscribe("a", "p", time.Time{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero time: %v", err)
	}

	if err := s.Subscribe("a", "p", utc(2026, 1, 15, 12, 0)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := s.Subscribe("a", "p", utc(2026, 1, 15, 12, 0)); !errors.Is(err, ErrSubscriptionExists) {
		t.Fatalf("dup sub: %v", err)
	}

	// 修改套餐不改变已开通订阅的计费条件，但影响新订阅。
	if err := s.UpdatePlan(Plan{ID: "p", MonthlyFee: 999, IncludedUnits: 1, OveragePrice: 9, TaxRateBasisPoints: 2000}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := s.Subscribe("b", "p", utc(2026, 1, 16, 0, 0)); err != nil {
		t.Fatalf("subscribe b: %v", err)
	}
	ta := s.subTerms("a")
	tb := s.subTerms("b")
	if ta != (PlanTerms{PlanID: "p", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000}) {
		t.Fatalf("a terms changed: %+v", ta)
	}
	if tb.MonthlyFee != 999 || tb.IncludedUnits != 1 || tb.OveragePrice != 9 || tb.TaxRateBasisPoints != 2000 {
		t.Fatalf("b terms not updated: %+v", tb)
	}
}

func (s *Service) subTerms(id string) PlanTerms {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accounts[id].sub.terms
}

func TestEventDedupConflictAndTimeRules(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustAccount(t, s, "b")

	ev := Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 14, 0, 0), Quantity: 3}
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("no sub: %v", err)
	}
	if err := s.Subscribe("a", "p", utc(2026, 1, 15, 8, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe("b", "p", utc(2026, 1, 15, 8, 0)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "x", At: utc(2026, 1, 15, 7, 59), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("before activation: %v", err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "x", At: clk.get().Add(time.Second), Quantity: 1}); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("future: %v", err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "x", At: utc(2026, 1, 15, 9, 0), Quantity: -1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative qty: %v", err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "ghost", EventID: "x", At: utc(2026, 1, 15, 9, 0), Quantity: 1}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}

	ev = Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 15, 9, 0), Quantity: 3}
	r1, err := s.RecordEvent(ev)
	if err != nil || !r1.Accepted || r1.Period != jan(2026) {
		t.Fatalf("first accept: %+v %v", r1, err)
	}
	r2, err := s.RecordEvent(ev)
	if err != nil || r2.Accepted {
		t.Fatalf("identical replay must not accumulate: %+v %v", r2, err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: ev.At, Quantity: 4}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("qty conflict: %v", err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: ev.At.Add(time.Second), Quantity: 3}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("time conflict: %v", err)
	}
	// 冲突不改变累计用量。
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 3 {
		t.Fatalf("usage after conflicts = %d", u.Total)
	}

	// 不同账户可复用事件标识，且互不影响。
	clk.t = utc(2026, 1, 16, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "b", EventID: "e1", At: utc(2026, 1, 16, 0, 0), Quantity: 7}); err != nil {
		t.Fatalf("cross-account id: %v", err)
	}
	if u, _ := s.MonthlyUsage("b", jan(2026)); u.Total != 7 {
		t.Fatalf("b usage = %d", u.Total)
	}

	// 事件按发生时刻归账期：1 月 31 日深夜发生、2 月上报仍归入 1 月。
	clk.t = utc(2026, 2, 1, 10, 0)
	r3, err := s.RecordEvent(Event{AccountID: "a", EventID: "late", At: utc(2026, 1, 31, 23, 59), Quantity: 2})
	if err != nil || r3.Period != jan(2026) {
		t.Fatalf("late Jan event: %+v %v", r3, err)
	}
	// UTC 月初属于新一期。
	r4, err := s.RecordEvent(Event{AccountID: "a", EventID: "feb1", At: utc(2026, 2, 1, 0, 0), Quantity: 4})
	if err != nil || r4.Period != feb(2026) {
		t.Fatalf("Feb boundary: %+v %v", r4, err)
	}

	// 出账后：新事件拒绝，同标识重报仍成功。
	bill, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if bill.TotalUsage != 5 {
		t.Fatalf("jan total = %d, want 5", bill.TotalUsage)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "new-after-bill", At: utc(2026, 1, 20, 0, 0), Quantity: 1}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new event after billed: %v", err)
	}
	if r, err := s.RecordEvent(ev); err != nil || r.Accepted {
		t.Fatalf("replay after billed must succeed: %+v %v", r, err)
	}
}

func TestBillComputationFirstMonthFullTerms(t *testing.T) {
	// 月费 10000 分，额度 10，单价 600 分，税率 10%（1000 万分点）。
	s, clk := newTestService(utc(2026, 1, 22, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 10000, IncludedUnits: 10, OveragePrice: 600, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 15, 12, 0)); err != nil {
		t.Fatal(err)
	}
	// 首月即用满 25：完整月费 + 完整额度。
	for i, q := range []int64{10, 15} {
		if _, err := s.RecordEvent(Event{AccountID: "a", EventID: fmt.Sprintf("e%d", i), At: utc(2026, 1, 20+i, 0, 0), Quantity: q}); err != nil {
			t.Fatal(err)
		}
	}

	clk.t = utc(2026, 1, 31, 23, 59)
	if _, err := s.CreateBill("a", jan(2026)); !errors.Is(err, ErrBillMonthNotEnded) {
		t.Fatalf("open month: %v", err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.TotalUsage != 25 || b.OverageUnits != 15 {
		t.Fatalf("usage: total=%d over=%d", b.TotalUsage, b.OverageUnits)
	}
	if b.MonthlyFee != 10000 || b.OverageFee != 9000 {
		t.Fatalf("fees: fee=%d over=%d", b.MonthlyFee, b.OverageFee)
	}
	// (10000+9000)*10% = 1900
	if b.Tax != 1900 || b.TotalDue != 20900 || b.Balance != 20900 || b.Settled {
		t.Fatalf("totals: tax=%d due=%d bal=%d settled=%v", b.Tax, b.TotalDue, b.Balance, b.Settled)
	}
	if !b.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("due at = %v", b.DueAt)
	}
	// 重复出账同一张，后续用量与付款状态变化不改金额（此处 1 月已出账，事件已被封）。
	b2, err := s.CreateBill("a", jan(2026))
	if err != nil || b2 != b {
		t.Fatalf("bill not idempotent: %+v vs %+v err=%v", b, b2, err)
	}

	// 开通当月之前不能出账。
	if _, err := s.CreateBill("a", Month{Year: 2025, Month: time.December}); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("before activation: %v", err)
	}

	// 未用量月份也可出账：零用量、零超额。
	clk.t = utc(2026, 3, 1, 0, 0)
	febBill, err := s.CreateBill("a", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	if febBill.TotalUsage != 0 || febBill.OverageUnits != 0 || febBill.OverageFee != 0 {
		t.Fatalf("empty month bill: %+v", febBill)
	}
}

func TestTaxRounding(t *testing.T) {
	cases := []struct {
		subtotal, rate, want int64
	}{
		{10005, 1000, 1001}, // 1000.5 半数向上
		{10001, 1000, 1000}, // 1000.1
		{10004, 1000, 1000}, // 1000.4
		{10000, 1234, 1234}, // 整除
		{0, 6000, 0},
		{100, 10000, 100}, // 100% 税率
		{3, 3333, 1},      // 0.9999 -> 1
		{1, 3333, 0},      // 0.3333 -> 0
	}
	for _, c := range cases {
		got, ok := rateAmount(c.subtotal, c.rate)
		if !ok || got != c.want {
			t.Fatalf("rateAmount(%d,%d)=%d,%v want %d", c.subtotal, c.rate, got, ok, c.want)
		}
	}
}

func TestPaymentsPartialAndIdempotency(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.TotalDue != 1000 || b.Settled {
		t.Fatalf("bill: %+v", b)
	}

	if _, err := s.RecordPayment("a", "p1", jan(2026), 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero pay: %v", err)
	}
	if _, err := s.RecordPayment("a", "p1", jan(2026), 1001); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("over pay: %v", err)
	}
	// 失败不消耗付款标识，也不扣余额。
	if got, _ := s.GetBill("a", jan(2026)); got.Balance != 1000 {
		t.Fatalf("balance changed after failed pay: %d", got.Balance)
	}
	if _, err := s.RecordPayment("ghost", "p1", jan(2026), 1); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if _, err := s.RecordPayment("a", "p1", feb(2026), 1); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("missing bill: %v", err)
	}

	r1, err := s.RecordPayment("a", "p1", jan(2026), 400)
	if err != nil || !r1.Registered || r1.BillBalance != 600 || r1.Settled {
		t.Fatalf("pay1: %+v %v", r1, err)
	}
	r1b, err := s.RecordPayment("a", "p1", jan(2026), 400)
	if err != nil || r1b.Registered || r1b.BillBalance != 600 {
		t.Fatalf("pay replay: %+v %v", r1b, err)
	}
	if _, err := s.RecordPayment("a", "p1", jan(2026), 401); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("amount conflict: %v", err)
	}
	if _, err := s.RecordPayment("a", "p1", feb(2026), 400); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("bill conflict: %v", err)
	}
	if got, _ := s.GetBill("a", jan(2026)); got.Paid != 400 {
		t.Fatalf("paid changed after conflict: %d", got.Paid)
	}

	r2, err := s.RecordPayment("a", "p2", jan(2026), 600)
	if err != nil || !r2.Registered || r2.BillBalance != 0 || !r2.Settled {
		t.Fatalf("pay2: %+v %v", r2, err)
	}
	if _, err := s.RecordPayment("a", "p3", jan(2026), 1); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("pay settled bill: %v", err)
	}
}

func TestPaymentReplayReturnsFirstRegistrationSnapshot(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBill("a", jan(2026)); err != nil {
		t.Fatal(err)
	}

	// p1 支付部分欠款：登记后余额 600、未付清。
	r1, err := s.RecordPayment("a", "p1", jan(2026), 400)
	if err != nil || !r1.Registered || r1.BillBalance != 600 || r1.Settled {
		t.Fatalf("p1 first: %+v %v", r1, err)
	}
	// p2 结清剩余 600。
	r2, err := s.RecordPayment("a", "p2", jan(2026), 600)
	if err != nil || !r2.Registered || r2.BillBalance != 0 || !r2.Settled {
		t.Fatalf("p2 first: %+v %v", r2, err)
	}

	// 原样重报 p1：不再次登记，返回首次登记时的快照，而非账单当前状态。
	rep1, err := s.RecordPayment("a", "p1", jan(2026), 400)
	if err != nil {
		t.Fatal(err)
	}
	if rep1.Registered {
		t.Fatalf("p1 replay should not register again: %+v", rep1)
	}
	if rep1.Payment.Period != jan(2026) || rep1.Payment.Amount != 400 {
		t.Fatalf("p1 replay content changed: %+v", rep1.Payment)
	}
	if rep1.BillBalance != 600 || rep1.Settled {
		t.Fatalf("p1 replay snapshot = balance %d settled %v, want 600/false", rep1.BillBalance, rep1.Settled)
	}

	// 原样重报 p2：余额 0、已付清。
	rep2, err := s.RecordPayment("a", "p2", jan(2026), 600)
	if err != nil || rep2.Registered || rep2.BillBalance != 0 || !rep2.Settled {
		t.Fatalf("p2 replay: %+v %v", rep2, err)
	}

	// 历史结果不改变账单当前状态：已付仍是 1000、余额 0、已付清。
	b, _ := s.GetBill("a", jan(2026))
	if b.Paid != 1000 || b.Balance != 0 || !b.Settled || b.TotalDue != 1000 {
		t.Fatalf("bill current state after replays: %+v", b)
	}
	st, _ := s.Status("a")
	if len(st.Bills) != 1 || st.Bills[0].Paid != 1000 || st.Bills[0].Balance != 0 || !st.Bills[0].Settled {
		t.Fatalf("status bill summary after replays: %+v", st.Bills)
	}
}

func TestPaymentReplaySnapshotAfterPartialFollowUp(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBill("a", jan(2026)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RecordPayment("a", "p1", jan(2026), 400); err != nil {
		t.Fatal(err)
	}
	// 后续付款只支付部分余额（账单余额 600 -> 300，未付清）。
	if _, err := s.RecordPayment("a", "p2", jan(2026), 300); err != nil {
		t.Fatal(err)
	}
	rep1, err := s.RecordPayment("a", "p1", jan(2026), 400)
	if err != nil || rep1.Registered || rep1.BillBalance != 600 || rep1.Settled {
		t.Fatalf("p1 replay after partial follow-up: %+v %v", rep1, err)
	}
	// 已付金额没有因重报再次增加。
	if b, _ := s.GetBill("a", jan(2026)); b.Paid != 700 || b.Balance != 300 {
		t.Fatalf("bill after replay: paid %d balance %d, want 700/300", b.Paid, b.Balance)
	}

	// 账单尚有 300 余额时：p4 先超额提交失败（不占用标识、不改变账单），
	// 再以该标识提交合法付款，得到首次成功登记的结果。
	if _, err := s.RecordPayment("a", "p4", jan(2026), 301); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("p4 overpay: %v", err)
	}
	if b, _ := s.GetBill("a", jan(2026)); b.Paid != 700 || b.Balance != 300 {
		t.Fatalf("bill changed after failed p4: %+v", b)
	}
	r4, err := s.RecordPayment("a", "p4", jan(2026), 300)
	if err != nil || !r4.Registered || r4.BillBalance != 0 || !r4.Settled {
		t.Fatalf("p4 first success: %+v %v", r4, err)
	}
	// 原样重报返回同一快照，不再次登记。
	rep4, err := s.RecordPayment("a", "p4", jan(2026), 300)
	if err != nil || rep4.Registered || rep4.BillBalance != 0 || !rep4.Settled {
		t.Fatalf("p4 replay: %+v %v", rep4, err)
	}

	// 账单结清后，相同标识改金额或账期仍是冲突；改指向还没有账单的账期也一样。
	for _, req := range []struct {
		period Month
		amount int64
	}{
		{jan(2026), 401},
		{feb(2026), 400},
	} {
		if _, err := s.RecordPayment("a", "p1", req.period, req.amount); !errors.Is(err, ErrPaymentConflict) {
			t.Fatalf("p1 changed to %v/%d: %v", req.period, req.amount, err)
		}
	}
	if b, _ := s.GetBill("a", jan(2026)); b.Paid != 1000 || b.Balance != 0 || !b.Settled {
		t.Fatalf("bill after conflicts: %+v", b)
	}
}

func TestPaymentReplayDoesNotReSuspendRecoveredAccount(t *testing.T) {
	s, clk := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBill("a", jan(2026)); err != nil {
		t.Fatal(err)
	}
	// 过了截止时刻，先欠 600 被停用，再结清恢复。
	clk.t = utc(2026, 2, 8, 0, 0)
	if st, _ := s.Status("a"); !st.Suspended {
		t.Fatalf("should be suspended before payment")
	}
	if _, err := s.RecordPayment("a", "p1", jan(2026), 400); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPayment("a", "p2", jan(2026), 600); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Status("a"); st.Suspended {
		t.Fatalf("should recover after settlement")
	}
	// 重放历史结果显示“未付清”，账户不能因此被重新停用。
	if r, err := s.RecordPayment("a", "p1", jan(2026), 400); err != nil || r.Settled {
		t.Fatalf("p1 replay: %+v %v", r, err)
	}
	if st, _ := s.Status("a"); st.Suspended {
		t.Fatalf("account re-suspended by historical replay: %+v", st)
	}
}

func TestZeroAmountBillSettled(t *testing.T) {
	s, clk := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 0, IncludedUnits: 100, OveragePrice: 1, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e", At: utc(2026, 1, 10, 0, 0), Quantity: 50}); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.TotalDue != 0 || !b.Settled || b.Balance != 0 {
		t.Fatalf("zero bill: %+v", b)
	}

	// 即使过了截止时刻也不停用。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, err := s.Status("a")
	if err != nil || st.Suspended {
		t.Fatalf("zero bill suspended: %+v %v", st, err)
	}
}

func TestSuspensionAndRecovery(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	janEvent := Event{AccountID: "a", EventID: "j1", At: utc(2026, 1, 20, 0, 0), Quantity: 1}
	if _, err := s.RecordEvent(janEvent); err != nil {
		t.Fatal(err)
	}

	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("a", jan(2026)); err != nil {
		t.Fatal(err)
	}

	// 截止前正常接收 2 月用量。
	clk.t = utc(2026, 2, 2, 0, 0)
	febEvent := Event{AccountID: "a", EventID: "f1", At: utc(2026, 2, 2, 0, 0), Quantity: 1}
	if _, err := s.RecordEvent(febEvent); err != nil {
		t.Fatalf("event before due: %v", err)
	}

	// 到达截止时刻仍有余额：停用。
	clk.t = utc(2026, 2, 8, 0, 0)
	st, _ := s.Status("a")
	if !st.Suspended {
		t.Fatalf("should be suspended: %+v", st)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "f2", At: utc(2026, 2, 3, 0, 0), Quantity: 1}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("new event while suspended: %v", err)
	}
	// 已接收事件的相同重报仍成功。
	if r, err := s.RecordEvent(janEvent); err != nil || r.Accepted {
		t.Fatalf("jan replay suspended: %+v %v", r, err)
	}
	if r, err := s.RecordEvent(febEvent); err != nil || r.Accepted {
		t.Fatalf("feb replay suspended: %+v %v", r, err)
	}

	// 部分付款仍欠费，继续停用。
	if _, err := s.RecordPayment("a", "p1", jan(2026), 500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "f3", At: utc(2026, 2, 3, 0, 0), Quantity: 1}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("still suspended after partial pay: %v", err)
	}

	// 结清后立即恢复；2 月账单尚未生成，新用量可继续累计。
	if _, err := s.RecordPayment("a", "p2", jan(2026), 500); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("a")
	if st.Suspended {
		t.Fatalf("should resume: %+v", st)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "f3", At: utc(2026, 2, 3, 0, 0), Quantity: 1}); err != nil {
		t.Fatalf("event after resume: %v", err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 2 {
		t.Fatalf("feb usage = %d want 2", u.Total)
	}

	// Status 中应能看到各月用量与账单余额。
	if len(st.MonthlyUsage) != 2 || st.MonthlyUsage[0].Period != jan(2026) || st.MonthlyUsage[1].Period != feb(2026) {
		t.Fatalf("monthly usage: %+v", st.MonthlyUsage)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) || st.Bills[0].Balance != 0 || !st.Bills[0].Settled {
		t.Fatalf("bills: %+v", st.Bills)
	}
}

func TestOverflowLeavesNoTrace(t *testing.T) {
	s, clk := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 0, IncludedUnits: 0, OveragePrice: 1, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}

	base := maxInt64 - 5
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "base", At: utc(2026, 1, 10, 0, 0), Quantity: base}); err != nil {
		t.Fatal(err)
	}
	// 本次累计将溢出：失败且不消耗事件标识。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "try", At: utc(2026, 1, 11, 0, 0), Quantity: 6}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("usage overflow: %v", err)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != base {
		t.Fatalf("usage after overflow = %d", u.Total)
	}
	// 同一标识以不溢出的数量重试，应当成功。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "try", At: utc(2026, 1, 11, 0, 0), Quantity: 5}); err != nil {
		t.Fatalf("id reused after overflow failure: %v", err)
	}

	// 超额费用溢出：超额用量 maxInt64 * 单价 2。
	mustAccount(t, s, "b")
	mustPlan(t, s, Plan{ID: "p2", MonthlyFee: 0, IncludedUnits: 0, OveragePrice: 2, TaxRateBasisPoints: 0})
	if err := s.Subscribe("b", "p2", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "b", EventID: "big", At: utc(2026, 1, 10, 0, 0), Quantity: maxInt64}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBill("b", jan(2026)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overage overflow: %v", err)
	}
	if _, err := s.GetBill("b", jan(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("partial bill left behind: %v", err)
	}

	// 税额与月费之和溢出（月费 maxInt64，税率 100%）。
	mustAccount(t, s, "c")
	mustPlan(t, s, Plan{ID: "p3", MonthlyFee: maxInt64, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 10000})
	if err := s.Subscribe("c", "p3", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBill("c", jan(2026)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("total overflow: %v", err)
	}

	_ = clk
}

func TestConcurrentDuplicates(t *testing.T) {
	s, clk := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 5, OveragePrice: 10, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	ev := Event{AccountID: "a", EventID: "dup", At: utc(2026, 1, 10, 0, 0), Quantity: 7}

	const n = 64
	var wg sync.WaitGroup
	var accepted int64
	var mu sync.Mutex
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r, err := s.RecordEvent(ev)
			if err != nil {
				t.Errorf("concurrent event: %v", err)
				return
			}
			if r.Accepted {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("accepted = %d, want 1", accepted)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 7 {
		t.Fatalf("usage = %d, want 7", u.Total)
	}

	// 并发出账只生成一张。
	bills := make([]Bill, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			b, err := s.CreateBill("a", jan(2026))
			if err != nil {
				t.Errorf("concurrent bill: %v", err)
				return
			}
			bills[i] = b
		}(i)
	}
	wg.Wait()
	var due int64 = -1
	for _, b := range bills {
		if due == -1 {
			due = b.TotalDue
		} else if b.TotalDue != due {
			t.Fatalf("bills differ: %d vs %d", due, b.TotalDue)
		}
	}
	// fee 1000 + overage (7-5)*10=20 => subtotal 1020, tax 102 => 1122
	if due != 1122 {
		t.Fatalf("due = %d, want 1122", due)
	}

	// 并发同标识付款只扣一次。
	var registered int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r, err := s.RecordPayment("a", "pay", jan(2026), 1122)
			if err != nil {
				t.Errorf("concurrent pay: %v", err)
				return
			}
			if r.Registered {
				mu.Lock()
				registered++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if registered != 1 {
		t.Fatalf("registered = %d, want 1", registered)
	}
	b, _ := s.GetBill("a", jan(2026))
	if b.Paid != 1122 || b.Balance != 0 || !b.Settled {
		t.Fatalf("bill after concurrent pay: paid=%d bal=%d settled=%v", b.Paid, b.Balance, b.Settled)
	}

	// 并发不同标识事件全部累计一次。
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := s.RecordEvent(Event{
				AccountID: "a",
				EventID:   fmt.Sprintf("e%d", i),
				At:        utc(2026, 2, 1, 0, 0),
				Quantity:  1,
			})
			if err != nil {
				t.Errorf("distinct event: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != n {
		t.Fatalf("feb usage = %d, want %d", u.Total, n)
	}

	_ = clk
}
