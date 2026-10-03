package meter

import (
	"errors"
	"testing"
)

// assertHistoricalBill 核对账单采用了期望的套餐条件快照、用量明细与金额，
// 且尚未登记付款时已付为零、余额等于应付。
func assertHistoricalBill(t *testing.T, b Bill, terms PlanTerms, usage, fee, overUnits, overFee, tax, due int64) {
	t.Helper()
	if b.Terms != terms {
		t.Fatalf("bill %s terms = %+v, want %+v", b.Period, b.Terms, terms)
	}
	if b.TotalUsage != usage || b.IncludedUnits != terms.IncludedUnits || b.OverageUnits != overUnits {
		t.Fatalf("bill %s usage detail: total=%d included=%d over=%d", b.Period, b.TotalUsage, b.IncludedUnits, b.OverageUnits)
	}
	if b.MonthlyFee != fee || b.OverageFee != overFee || b.Tax != tax || b.TotalDue != due {
		t.Fatalf("bill %s amounts: fee=%d over=%d tax=%d due=%d", b.Period, b.MonthlyFee, b.OverageFee, b.Tax, b.TotalDue)
	}
	if b.Paid != 0 || b.Balance != due || b.Settled {
		t.Fatalf("bill %s payment state: paid=%d balance=%d settled=%v", b.Period, b.Paid, b.Balance, b.Settled)
	}
}

// TestBackfillBillsAcrossPlanChangeCancelAndResubscribe 覆盖完整链路：
// 开通甲 -> 安排次月换乙（锁定安排时乙的完整条件）-> 修改乙当前定义 ->
// 按月取消（3/1 终止）-> 3 月空档 -> 4/1 按新条件重新开通乙 ->
// 5 月先出 4 月账单，再补开 2 月、1 月账单。
// 旧账期必须采用当时锁定的条件，不被后来的订阅或同一套餐的新价格覆盖。
func TestBackfillBillsAcrossPlanChangeCancelAndResubscribe(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 甲：月费 1000、额度 10、超额单价 100、税 10%。
	mustPlan(t, s, planDef("a", 1000, 10, 100, 1000))
	// 乙（安排时）：月费 2000、额度 20、超额单价 200、税 10%。
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	termsA := PlanTerms{PlanID: "a", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000}
	lockedB := PlanTerms{PlanID: "b", MonthlyFee: 2000, IncludedUnits: 20, OveragePrice: 200, TaxRateBasisPoints: 1000}
	newB := PlanTerms{PlanID: "b", MonthlyFee: 4000, IncludedUnits: 1, OveragePrice: 500, TaxRateBasisPoints: 2000}

	// 1 月中旬安排 2 月起改用乙：锁定安排时乙的完整条件快照。
	r := mustSchedule(t, s, "u", "b")
	if !r.Created || r.Change.Terms != lockedB || r.Change.EffectivePeriod != feb(2026) {
		t.Fatalf("schedule = %+v, want locked terms %+v effective 2026-02", r, lockedB)
	}

	// 安排成功后修改乙的当前定义：不影响已锁定的安排，只影响之后的开通。
	if err := s.UpdatePlan(planDef("b", 4000, 1, 500, 2000)); err != nil {
		t.Fatal(err)
	}

	// 1 月接收 12 单位用量（甲条件：额度 10、超额单价 100）。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "jan", At: utc(2026, 1, 10, 0, 0), Quantity: 12}); err != nil {
		t.Fatal(err)
	}
	// 2 月接收 25 单位用量（换套餐已生效，适用锁定的乙条件：额度 20、超额单价 200）。
	clk.t = utc(2026, 2, 10, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "feb", At: utc(2026, 2, 5, 0, 0), Quantity: 25}); err != nil {
		t.Fatal(err)
	}

	// 2 月中旬登记取消，3/1 终止；2 月（取消月份）仍按完整月费与额度计费。
	cr := mustCancel(t, s, "u")
	if !cr.Cancelled || !cr.Cancellation.EndAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("cancel = %+v, want end 2026-03-01", cr)
	}

	// 4/1 重新开通乙，采用修改后的条件；3 月整月没有订阅。
	clk.t = utc(2026, 4, 1, 0, 0)
	mustSubscribe(t, s, "u", "b", utc(2026, 4, 1, 0, 0))
	if st, _ := s.Status("u"); !st.Subscribed || st.CurrentTerms != newB {
		t.Fatalf("status after resubscribe: subscribed=%v terms=%+v", st.Subscribed, st.CurrentTerms)
	}

	// 5 月：先为重新开通后的 4 月出账，再补开 2 月、1 月账单。
	clk.t = utc(2026, 5, 10, 12, 0)

	// 4 月无用量：月费 4000、税 20% = 800，应付 4800，采用重新开通时的乙条件。
	aprBill := mustBill(t, s, "u", apr(2026))
	assertHistoricalBill(t, aprBill, newB, 0, 4000, 0, 0, 800, 4800)

	// 2 月保持安排换套餐时锁定的乙条件：月费 2000、超额 (25-20)*200 = 1000、
	// 税 10% = 300，应付 3300；不被修改后的乙定义或重新开通的订阅覆盖。
	febBill := mustBill(t, s, "u", feb(2026))
	assertHistoricalBill(t, febBill, lockedB, 25, 2000, 5, 1000, 300, 3300)

	// 1 月仍按甲条件计费：月费 1000、超额 (12-10)*100 = 200、税 10% = 120，应付 1320。
	janBill := mustBill(t, s, "u", jan(2026))
	assertHistoricalBill(t, janBill, termsA, 12, 1000, 2, 200, 120, 1320)

	// 重新开通不能把空档的 3 月变成可出账月份。
	if _, err := s.CreateBill("u", mar(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("bill gap month mar: %v", err)
	}
	if _, err := s.GetBill("u", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("get gap month mar bill: %v", err)
	}

	// 补开后账户当前套餐仍是重新开通时采用的乙条件。
	st, _ := s.Status("u")
	if !st.Subscribed || st.CurrentTerms != newB {
		t.Fatalf("current terms after backfill: subscribed=%v terms=%+v", st.Subscribed, st.CurrentTerms)
	}

	// 随后查询已生成的历史账单：计费条件、金额与用量保持一致，
	// 尚未登记付款时已付为零、余额等于应付。
	for _, want := range []Bill{janBill, febBill, aprBill} {
		got, err := s.GetBill("u", want.Period)
		if err != nil {
			t.Fatalf("get bill %s: %v", want.Period, err)
		}
		if got != want {
			t.Fatalf("bill %s changed after backfill:\n got %+v\nwant %+v", want.Period, got, want)
		}
	}
	// 重复出账得到同一张账单。
	if again := mustBill(t, s, "u", feb(2026)); again != febBill {
		t.Fatalf("feb bill not idempotent:\n got %+v\nwant %+v", again, febBill)
	}

	// 状态中的账单摘要同样反映各月应付与未付余额。
	if len(st.Bills) != 3 {
		t.Fatalf("status bills = %+v, want 3 entries", st.Bills)
	}
	for i, want := range []Bill{janBill, febBill, aprBill} {
		sum := st.Bills[i]
		if sum.Period != want.Period || sum.TotalDue != want.TotalDue ||
			sum.Paid != 0 || sum.Balance != want.TotalDue || sum.Settled {
			t.Fatalf("bill summary %d = %+v, want period %s due %d unpaid", i, sum, want.Period, want.TotalDue)
		}
	}
}
