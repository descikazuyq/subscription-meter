package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// switchSetup 创建一个已开通订阅的测试服务，返回服务与时钟。
func switchSetup(t *testing.T, start time.Time, plan Plan) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(start)
	mustPlan(t, s, plan)
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", plan.ID, start); err != nil {
		t.Fatal(err)
	}
	return s, clk
}

func apr(y int) Month { return Month{Year: y, Month: time.April} }
func jun(y int) Month { return Month{Year: y, Month: time.June} }

func TestSchedulePlanSwitchBasic(t *testing.T) {
	// 1 月 15 日安排从 2 月起切换到套餐 B。
	s, clk := switchSetup(t, utc(2026, 1, 15, 12, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	sw, err := s.SchedulePlanSwitch("a", "B")
	if err != nil {
		t.Fatal(err)
	}
	if sw.PlanID != "B" {
		t.Fatalf("plan id = %q", sw.PlanID)
	}
	if sw.Terms != (PlanTerms{PlanID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000}) {
		t.Fatalf("terms = %+v", sw.Terms)
	}
	if sw.Effective != feb(2026) {
		t.Fatalf("effective = %v, want 2026-02", sw.Effective)
	}

	// 状态查询应同时显示当前套餐与待生效安排。
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms == nil || st.CurrentTerms.PlanID != "A" {
		t.Fatalf("current terms = %+v", st.CurrentTerms)
	}
	if st.Pending == nil || st.Pending.PlanID != "B" || st.Pending.Effective != feb(2026) {
		t.Fatalf("pending = %+v", st.Pending)
	}

	// 1 月仍按 A 套餐计费。
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.Terms.PlanID != "A" || b.MonthlyFee != 100 {
		t.Fatalf("jan bill terms = %+v fee = %d", b.Terms, b.MonthlyFee)
	}
}

func TestSchedulePlanSwitchSameTargetReturnsOriginal(t *testing.T) {
	s, _ := switchSetup(t, utc(2026, 1, 15, 12, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	sw1, err := s.SchedulePlanSwitch("a", "B")
	if err != nil {
		t.Fatal(err)
	}
	// 再次选择同一目标套餐：返回原安排，不重新取价。
	sw2, err := s.SchedulePlanSwitch("a", "B")
	if err != nil {
		t.Fatal(err)
	}
	if sw1 != sw2 {
		t.Fatalf("same target returned different schedule: %+v vs %+v", sw1, sw2)
	}
}

func TestSchedulePlanSwitchReplaces(t *testing.T) {
	s, _ := switchSetup(t, utc(2026, 1, 15, 12, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})
	mustPlan(t, s, Plan{ID: "C", MonthlyFee: 300, IncludedUnits: 30, OveragePrice: 2, TaxRateBasisPoints: 3000})

	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatal(err)
	}
	// 选择另一个目标套餐：替换原安排，生效月份仍为下一月。
	sw, err := s.SchedulePlanSwitch("a", "C")
	if err != nil {
		t.Fatal(err)
	}
	if sw.PlanID != "C" || sw.Effective != feb(2026) {
		t.Fatalf("replaced schedule = %+v", sw)
	}
	if sw.Terms.MonthlyFee != 300 {
		t.Fatalf("replaced terms = %+v", sw.Terms)
	}

	// 状态中只有一个待生效安排，且为 C。
	st, _ := s.Status("a")
	if st.Pending == nil || st.Pending.PlanID != "C" {
		t.Fatalf("pending after replace = %+v", st.Pending)
	}
}

func TestSchedulePlanSwitchFailures(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	// 账户不存在。
	if _, err := s.SchedulePlanSwitch("ghost", "B"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}

	// 尚未开通订阅。
	mustAccount(t, s, "a")
	if _, err := s.SchedulePlanSwitch("a", "B"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("no sub: %v", err)
	}

	// 开通时刻尚未到达。
	if err := s.Subscribe("a", "A", utc(2026, 2, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulePlanSwitch("a", "B"); !errors.Is(err, ErrSubscriptionNotYetActive) {
		t.Fatalf("activation not arrived: %v", err)
	}

	// 开通时刻到达后可以安排。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatalf("schedule after activation: %v", err)
	}

	// 目标套餐不存在。
	if _, err := s.SchedulePlanSwitch("a", "ghost"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("missing plan: %v", err)
	}

	// 目标套餐与当前套餐相同。
	if _, err := s.SchedulePlanSwitch("a", "A"); !errors.Is(err, ErrPlanSameAsCurrent) {
		t.Fatalf("same as current: %v", err)
	}
}

func TestCancelSchedule(t *testing.T) {
	s, _ := switchSetup(t, utc(2026, 1, 15, 12, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatal(err)
	}
	// 取消待生效安排。
	if err := s.CancelSchedule("a"); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Status("a")
	if st.Pending != nil {
		t.Fatalf("pending after cancel = %+v", st.Pending)
	}
	// 取消后继续沿用当前套餐。
	if st.CurrentTerms == nil || st.CurrentTerms.PlanID != "A" {
		t.Fatalf("current terms after cancel = %+v", st.CurrentTerms)
	}

	// 没有待生效安排时取消也成功。
	if err := s.CancelSchedule("a"); err != nil {
		t.Fatalf("cancel with no pending: %v", err)
	}

	// 账户不存在时取消失败。
	if err := s.CancelSchedule("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cancel missing account: %v", err)
	}
}

func TestSwitchTakesEffectAtBoundary(t *testing.T) {
	s, clk := switchSetup(t, utc(2026, 1, 15, 12, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatal(err)
	}

	// 到达月初零点，新套餐立即生效，无需先上报用量或出账。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ := s.Status("a")
	if st.CurrentTerms == nil || st.CurrentTerms.PlanID != "B" {
		t.Fatalf("current terms at boundary = %+v", st.CurrentTerms)
	}
	if st.Pending != nil {
		t.Fatalf("pending at boundary = %+v", st.Pending)
	}

	// 1 月已结束，出账使用 A 套餐条件。
	bJan, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if bJan.Terms.PlanID != "A" || bJan.MonthlyFee != 100 {
		t.Fatalf("jan bill terms = %+v fee = %d", bJan.Terms, bJan.MonthlyFee)
	}

	// 2 月结束后出账使用 B 套餐条件。
	clk.t = utc(2026, 3, 1, 0, 0)
	b, err := s.CreateBill("a", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.Terms.PlanID != "B" || b.MonthlyFee != 200 {
		t.Fatalf("feb bill terms = %+v fee = %d", b.Terms, b.MonthlyFee)
	}
}

func TestScheduleAtBoundaryEffectiveMonth(t *testing.T) {
	// 恰在月初零点提交的新安排从再下一月生效。
	s, clk := switchSetup(t, utc(2026, 1, 15, 12, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	clk.t = utc(2026, 2, 1, 0, 0)
	sw, err := s.SchedulePlanSwitch("a", "B")
	if err != nil {
		t.Fatal(err)
	}
	// 2 月 1 日零点提交，从 4 月生效（再下一月）。
	if sw.Effective != apr(2026) {
		t.Fatalf("effective at boundary = %v, want 2026-04", sw.Effective)
	}
}

func TestHistoricalBillingUsesCorrectTerms(t *testing.T) {
	// 连续多次换套餐后，每张账单使用对应账期的完整条件。
	s, clk := switchSetup(t, utc(2026, 1, 1, 0, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})
	mustPlan(t, s, Plan{ID: "C", MonthlyFee: 300, IncludedUnits: 30, OveragePrice: 2, TaxRateBasisPoints: 3000})

	// 1 月用量 5。
	clk.t = utc(2026, 1, 10, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 5}); err != nil {
		t.Fatal(err)
	}

	// 1 月安排从 2 月起切换到 B。
	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatal(err)
	}

	// 2 月用量 15。
	clk.t = utc(2026, 2, 10, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e2", At: utc(2026, 2, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	// 2 月安排从 3 月起切换到 C。
	if _, err := s.SchedulePlanSwitch("a", "C"); err != nil {
		t.Fatal(err)
	}

	// 3 月用量 25。
	clk.t = utc(2026, 3, 10, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e3", At: utc(2026, 3, 10, 0, 0), Quantity: 25}); err != nil {
		t.Fatal(err)
	}

	// 先出 3 月账单，再补 1 月、2 月账单。
	clk.t = utc(2026, 4, 1, 0, 0)
	billMar, err := s.CreateBill("a", mar(2026))
	if err != nil {
		t.Fatal(err)
	}
	if billMar.Terms.PlanID != "C" || billMar.MonthlyFee != 300 {
		t.Fatalf("mar bill terms = %+v fee = %d", billMar.Terms, billMar.MonthlyFee)
	}
	if billMar.TotalUsage != 25 {
		t.Fatalf("mar usage = %d", billMar.TotalUsage)
	}

	billFeb, err := s.CreateBill("a", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	if billFeb.Terms.PlanID != "B" || billFeb.MonthlyFee != 200 {
		t.Fatalf("feb bill terms = %+v fee = %d", billFeb.Terms, billFeb.MonthlyFee)
	}
	if billFeb.TotalUsage != 15 {
		t.Fatalf("feb usage = %d", billFeb.TotalUsage)
	}

	billJan, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if billJan.Terms.PlanID != "A" || billJan.MonthlyFee != 100 {
		t.Fatalf("jan bill terms = %+v fee = %d", billJan.Terms, billJan.MonthlyFee)
	}
	if billJan.TotalUsage != 5 {
		t.Fatalf("jan usage = %d", billJan.TotalUsage)
	}
}

func TestBackdatedUsageAfterSwitch(t *testing.T) {
	// 切换后补报上月用量仍归上月，按该月当时适用的套餐计费。
	s, clk := switchSetup(t, utc(2026, 1, 1, 0, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatal(err)
	}
	// 2 月补报 1 月用量。
	clk.t = utc(2026, 2, 10, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "late", At: utc(2026, 1, 20, 0, 0), Quantity: 8}); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.Terms.PlanID != "A" || b.TotalUsage != 8 {
		t.Fatalf("backdated jan bill terms = %+v usage = %d", b.Terms, b.TotalUsage)
	}
}

func TestSwitchWhileSuspended(t *testing.T) {
	// 欠费停用期间允许安排或取消套餐切换，但操作不清除欠费。
	s, clk := switchSetup(t, utc(2026, 1, 1, 0, 0), Plan{
		ID: "A", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 2000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})

	// 1 月账单到期未付，停用。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("a", jan(2026)); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 8, 0, 0)
	st, _ := s.Status("a")
	if !st.Suspended {
		t.Fatal("should be suspended")
	}

	// 停用期间安排切换：成功。
	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatalf("schedule while suspended: %v", err)
	}
	// 停用期间取消切换：成功。
	if err := s.CancelSchedule("a"); err != nil {
		t.Fatalf("cancel while suspended: %v", err)
	}
	// 停用状态未被清除。
	st, _ = s.Status("a")
	if !st.Suspended {
		t.Fatal("suspension cleared by switch operation")
	}

	// 结清后恢复。
	if _, err := s.RecordPayment("a", "p1", jan(2026), 1000); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("a")
	if st.Suspended {
		t.Fatal("should resume after payment")
	}
}

func TestPlanUpdateDoesNotAffectPending(t *testing.T) {
	// 目标套餐的月费等以安排时刻的值为准，之后修改套餐定义不影响此安排。
	s, _ := switchSetup(t, utc(2026, 1, 15, 12, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	sw, err := s.SchedulePlanSwitch("a", "B")
	if err != nil {
		t.Fatal(err)
	}
	// 修改套餐 B 的定义。
	if err := s.UpdatePlan(Plan{ID: "B", MonthlyFee: 999, IncludedUnits: 99, OveragePrice: 9, TaxRateBasisPoints: 9999}); err != nil {
		t.Fatal(err)
	}
	// 待生效安排的条件不变。
	st, _ := s.Status("a")
	if st.Pending == nil {
		t.Fatal("no pending")
	}
	if st.Pending.Terms.MonthlyFee != 200 || st.Pending.Terms.IncludedUnits != 20 {
		t.Fatalf("pending terms changed after plan update: %+v", st.Pending.Terms)
	}
	_ = sw
}

func TestSwitchDoesNotProrateOrCarryUsage(t *testing.T) {
	// 升级和降级采用同一规则：当月收完整月费、提供完整额度，
	// 不按天补差，也不把本月用量移到下月。
	s, clk := switchSetup(t, utc(2026, 1, 1, 0, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 0,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 0})

	// 1 月用量 15（超出 A 额度 5）。
	clk.t = utc(2026, 1, 10, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatal(err)
	}

	// 2 月出账：1 月用量仍为 15，不结转到 2 月。
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.TotalUsage != 15 || b.OverageUnits != 5 {
		t.Fatalf("jan usage = %d over = %d", b.TotalUsage, b.OverageUnits)
	}
	// 2 月用量从 0 开始。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 0 {
		t.Fatalf("feb usage carried over = %d", u.Total)
	}
}

func TestConcurrentSwitchOperations(t *testing.T) {
	// 并发提交切换、取消、用量和出账时，各次成功操作表现为某个先后顺序，
	// 查询与账单不把两个套餐的价格、额度或税率混在一起。
	s, clk := switchSetup(t, utc(2026, 1, 1, 0, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})
	mustPlan(t, s, Plan{ID: "C", MonthlyFee: 300, IncludedUnits: 30, OveragePrice: 2, TaxRateBasisPoints: 3000})

	// 将时钟拨到 1 月 10 日，确保并发上报的用量事件不被拒绝。
	clk.t = utc(2026, 1, 10, 0, 0)

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n * 4)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = s.SchedulePlanSwitch("a", "B")
		}(i)
		go func(i int) {
			defer wg.Done()
			_, _ = s.SchedulePlanSwitch("a", "C")
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = s.CancelSchedule("a")
		}(i)
		go func(i int) {
			defer wg.Done()
			_, _ = s.RecordEvent(Event{
				AccountID: "a", EventID: fmt.Sprintf("e%d", i),
				At: utc(2026, 1, 10, 0, 0), Quantity: 1,
			})
		}(i)
	}
	wg.Wait()

	// 状态一致：当前套餐为 A，待生效安排至多一个。
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms == nil || st.CurrentTerms.PlanID != "A" {
		t.Fatalf("current terms = %+v", st.CurrentTerms)
	}
	if st.Pending != nil && st.Pending.PlanID != "B" && st.Pending.PlanID != "C" {
		t.Fatalf("pending = %+v", st.Pending)
	}
	// 用量累计正确。
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != int64(n) {
		t.Fatalf("jan usage = %d, want %d", u.Total, n)
	}

	// 出账使用 A 套餐条件，不混入 B/C。
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if b.Terms.PlanID != "A" || b.MonthlyFee != 100 {
		t.Fatalf("jan bill terms = %+v fee = %d", b.Terms, b.MonthlyFee)
	}
}

func TestNoCallsForMonthsStillCorrect(t *testing.T) {
	// 中间几个月没有调用服务，再次访问也应得到正确的当前套餐，
	// 并可继续安排后续切换。
	s, clk := switchSetup(t, utc(2026, 1, 1, 0, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})
	mustPlan(t, s, Plan{ID: "C", MonthlyFee: 300, IncludedUnits: 30, OveragePrice: 2, TaxRateBasisPoints: 3000})

	// 1 月安排从 2 月起切换到 B。
	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatal(err)
	}

	// 跳过 2 月、3 月、4 月不调用服务。
	clk.t = utc(2026, 5, 15, 0, 0)

	// 再次访问：当前套餐应为 B（2 月已生效）。
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms == nil || st.CurrentTerms.PlanID != "B" {
		t.Fatalf("current terms after gap = %+v", st.CurrentTerms)
	}
	if st.Pending != nil {
		t.Fatalf("pending after gap = %+v", st.Pending)
	}

	// 可继续安排从 6 月起切换到 C。
	sw, err := s.SchedulePlanSwitch("a", "C")
	if err != nil {
		t.Fatal(err)
	}
	if sw.PlanID != "C" || sw.Effective != jun(2026) {
		t.Fatalf("new schedule after gap = %+v", sw)
	}
}

func TestBillAmountsFixedAfterSwitch(t *testing.T) {
	// 已生成账单的用量、税额和应付金额不因切换或取消安排而变化。
	s, clk := switchSetup(t, utc(2026, 1, 1, 0, 0), Plan{
		ID: "A", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 5, TaxRateBasisPoints: 1000,
	})
	mustPlan(t, s, Plan{ID: "B", MonthlyFee: 200, IncludedUnits: 20, OveragePrice: 3, TaxRateBasisPoints: 2000})

	clk.t = utc(2026, 1, 10, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	wantDue := b.TotalDue

	// 切换到 B 后，1 月账单金额不变。
	if _, err := s.SchedulePlanSwitch("a", "B"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBill("a", jan(2026)); got.TotalDue != wantDue {
		t.Fatalf("bill due changed after switch: %d vs %d", got.TotalDue, wantDue)
	}
	// 取消安排后也不变。
	if err := s.CancelSchedule("a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBill("a", jan(2026)); got.TotalDue != wantDue {
		t.Fatalf("bill due changed after cancel: %d vs %d", got.TotalDue, wantDue)
	}
}
