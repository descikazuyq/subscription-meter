package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func planDef(id string, fee, included, overage, tax int64) Plan {
	return Plan{ID: id, MonthlyFee: fee, IncludedUnits: included, OveragePrice: overage, TaxRateBasisPoints: tax}
}

func mustSubscribe(t *testing.T, s *Service, acct, plan string, at time.Time) {
	t.Helper()
	if err := s.Subscribe(acct, plan, at); err != nil {
		t.Fatalf("subscribe %s->%s: %v", acct, plan, err)
	}
}

func mustSchedule(t *testing.T, s *Service, acct, plan string) PlanChangeResult {
	t.Helper()
	r, err := s.SchedulePlanChange(acct, plan)
	if err != nil {
		t.Fatalf("schedule %s->%s: %v", acct, plan, err)
	}
	return r
}

func TestSchedulePlanChangeSnapshotAndIdempotency(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 1000))
	mustPlan(t, s, planDef("b", 2000, 20, 8, 600))
	mustPlan(t, s, planDef("c", 3000, 30, 9, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 成功返回目标套餐完整条件与生效账期（下一个 UTC 自然月）。
	r := mustSchedule(t, s, "u", "b")
	if !r.Created {
		t.Fatalf("first schedule should be created")
	}
	wantChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 2000, 20, 8, 600)),
		EffectivePeriod: feb(2026),
	}
	if r.Change != wantChange {
		t.Fatalf("change = %+v, want %+v", r.Change, wantChange)
	}

	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms.PlanID != "a" || st.PendingChange == nil || *st.PendingChange != wantChange {
		t.Fatalf("status after schedule: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 修改套餐定义后，同一目标的重复安排返回原安排，不重新取价。
	if err := s.UpdatePlan(planDef("b", 9999, 1, 99, 2000)); err != nil {
		t.Fatal(err)
	}
	r2 := mustSchedule(t, s, "u", "b")
	if r2.Created || r2.Change != wantChange {
		t.Fatalf("same target replay: created=%v change=%+v", r2.Created, r2.Change)
	}

	// 换一个目标：替换原安排，生效月份仍是本次接受时的下一月（仍为 2 月）。
	r3 := mustSchedule(t, s, "u", "c")
	if !r3.Created || r3.Change.TargetPlanID != "c" || r3.Change.EffectivePeriod != feb(2026) {
		t.Fatalf("replace schedule: %+v", r3)
	}
	if r3.Change.Terms != termsOf(planDef("c", 3000, 30, 9, 0)) {
		t.Fatalf("replacement terms: %+v", r3.Change.Terms)
	}
	st, _ = s.Status("u")
	if st.PendingChange == nil || st.PendingChange.TargetPlanID != "c" {
		t.Fatalf("pending after replace: %+v", st.PendingChange)
	}

	// 月末深夜安排，生效月仍是次月；恰在月初零点，新安排从再下一月生效。
	clk.t = utc(2026, 1, 31, 23, 59)
	mustAccount(t, s, "v")
	mustSubscribe(t, s, "v", "a", utc(2026, 1, 1, 0, 0))
	rv := mustSchedule(t, s, "v", "b")
	if rv.Change.EffectivePeriod != feb(2026) {
		t.Fatalf("late Jan schedule effective = %v", rv.Change.EffectivePeriod)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
	// 此刻 v 的 b 安排已生效；为 c 安排应从 3 月生效。
	rc := mustSchedule(t, s, "v", "c")
	if rc.Change.EffectivePeriod != mar(2026) {
		t.Fatalf("schedule exactly at boundary effective = %v, want 2026-03", rc.Change.EffectivePeriod)
	}
}

func TestSchedulePlanChangeErrors(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 1000))
	mustPlan(t, s, planDef("b", 2000, 20, 8, 600))
	mustAccount(t, s, "u")
	mustAccount(t, s, "late")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	// 开通时刻尚未到达。
	mustSubscribe(t, s, "late", "a", utc(2026, 1, 20, 0, 0))

	if _, err := s.SchedulePlanChange("ghost", "b"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	if _, err := s.SchedulePlanChange("", "b"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty account: %v", err)
	}
	if _, err := s.SchedulePlanChange("u", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty plan: %v", err)
	}
	mustAccount(t, s, "nosub")
	if _, err := s.SchedulePlanChange("nosub", "b"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("no subscription: %v", err)
	}
	if _, err := s.SchedulePlanChange("late", "b"); !errors.Is(err, ErrSubscriptionNotActivated) {
		t.Fatalf("not activated: %v", err)
	}
	if _, err := s.SchedulePlanChange("u", "ghost"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("missing plan: %v", err)
	}
	if _, err := s.SchedulePlanChange("u", "a"); !errors.Is(err, ErrPlanChangeSamePlan) {
		t.Fatalf("same as current: %v", err)
	}

	// 已有 b 安排时：安排当前套餐 a 失败，原安排保持不变。
	mustSchedule(t, s, "u", "b")
	if _, err := s.SchedulePlanChange("u", "a"); !errors.Is(err, ErrPlanChangeSamePlan) {
		t.Fatalf("schedule current plan while pending: %v", err)
	}
	if _, err := s.SchedulePlanChange("u", "ghost"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("missing plan while pending: %v", err)
	}
	st, _ := s.Status("u")
	if st.PendingChange == nil || st.PendingChange.TargetPlanID != "b" {
		t.Fatalf("pending changed after failures: %+v", st.PendingChange)
	}
	_ = clk
}

func TestPlanChangeAppliesAtMonthBoundaryWithoutAction(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 1000))
	mustPlan(t, s, planDef("b", 2000, 20, 8, 600))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	mustSchedule(t, s, "u", "b")

	// 1 月仍按旧套餐。
	st, _ := s.Status("u")
	if st.CurrentTerms.PlanID != "a" {
		t.Fatalf("jan current = %s", st.CurrentTerms.PlanID)
	}

	// 到达月初零点：无需上报用量或出账，查询立即反映新条件。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms.PlanID != "b" || st.PendingChange != nil {
		t.Fatalf("feb current = %+v pending = %+v", st.CurrentTerms, st.PendingChange)
	}

	// 中间几个月不调用服务，4 月再访问仍得到正确当前套餐。
	clk.t = utc(2026, 4, 10, 9, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms.PlanID != "b" {
		t.Fatalf("apr current = %s", st.CurrentTerms.PlanID)
	}
	// 仍可继续安排后续切换。
	r := mustSchedule(t, s, "u", "a")
	if r.Change.EffectivePeriod != (Month{Year: 2026, Month: time.May}) {
		t.Fatalf("next switch effective = %v", r.Change.EffectivePeriod)
	}
}

func TestPlanChangeKeepsFullMonthlyTerms(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// a：月费 1000、额度 10、超额单价 100、税 0；b：月费 2000、额度 20、单价 200。
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	mustSchedule(t, s, "u", "b")

	// 当前月（1 月）用量按旧套餐完整额度与单价计费，即使安排已存在。
	clk.t = utc(2026, 1, 31, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "j", At: utc(2026, 1, 20, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	// 进入 2 月（切换已生效）后补报上月用量：仍归 1 月、按旧套餐累计；
	// 只要 1 月尚未出账就接收。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "jlate", At: utc(2026, 1, 25, 0, 0), Quantity: 5}); err != nil {
		t.Fatalf("backfill jan after switch: %v", err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 20 {
		t.Fatalf("jan usage = %d, want 20", u.Total)
	}
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	// 旧套餐 a：用量 20 -> 超额 10 * 100 = 1000，月费 1000，合计 2000。
	if janBill.Terms.PlanID != "a" || janBill.MonthlyFee != 1000 || janBill.OverageUnits != 10 || janBill.OverageFee != 1000 || janBill.TotalDue != 2000 {
		t.Fatalf("jan bill mixed terms: %+v", janBill)
	}

	// 2 月用量按新套餐：25 单位 -> 超额 5 * 200 = 1000，月费 2000。
	// 截止前付清 1 月账单，避免欠费停用。
	if _, err := s.RecordPayment("u", "p", jan(2026), janBill.TotalDue); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 12, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "f", At: utc(2026, 2, 10, 0, 0), Quantity: 25}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 3, 1, 0, 0)
	febBill, err := s.CreateBill("u", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	if febBill.Terms.PlanID != "b" || febBill.MonthlyFee != 2000 || febBill.OverageUnits != 5 || febBill.OverageFee != 1000 || febBill.TotalDue != 3000 {
		t.Fatalf("feb bill mixed terms: %+v", febBill)
	}
	// 1 月账单不受 2 月出账影响。
	gotJan, _ := s.GetBill("u", jan(2026))
	if gotJan.TotalUsage != 20 || gotJan.TotalDue != 2000 {
		t.Fatalf("jan bill changed: %+v", gotJan)
	}

	// 已出账月份继续拒绝新事件。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "jnew", At: utc(2026, 1, 26, 0, 0), Quantity: 1}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new event on billed jan: %v", err)
	}
}

func TestCancelPlanChange(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 5, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 8, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 没有待生效安排时取消也成功。
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatalf("cancel without pending: %v", err)
	}

	mustSchedule(t, s, "u", "b")
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, _ := s.Status("u")
	if st.CurrentTerms.PlanID != "a" || st.PendingChange != nil {
		t.Fatalf("after cancel: cur=%s pending=%+v", st.CurrentTerms.PlanID, st.PendingChange)
	}
	// 取消后 2 月仍沿用旧套餐。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms.PlanID != "a" {
		t.Fatalf("plan changed after cancel: %s", st.CurrentTerms.PlanID)
	}
	// 已生效的切换不被撤回。
	mustSchedule(t, s, "u", "b")
	clk.t = utc(2026, 3, 1, 0, 0)
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatalf("cancel after effective: %v", err)
	}
	st, _ = s.Status("u")
	if st.CurrentTerms.PlanID != "b" {
		t.Fatalf("effective switch revoked: %s", st.CurrentTerms.PlanID)
	}

	// 引用错误。
	if err := s.CancelPlanChange("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("cancel ghost: %v", err)
	}
	mustAccount(t, s, "nosub")
	if err := s.CancelPlanChange("nosub"); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("cancel without subscription: %v", err)
	}
}

func TestBillsAfterMultipleChangesUsePeriodTerms(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 每月费不同，便于核对账单采用了哪个套餐：a=1000 b=2000 c=3000，额度均为 0、单价 0。
	mustPlan(t, s, planDef("a", 1000, 0, 0, 0))
	mustPlan(t, s, planDef("b", 2000, 0, 0, 0))
	mustPlan(t, s, planDef("c", 3000, 0, 0, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// a -> b（2 月生效）。
	mustSchedule(t, s, "u", "b")
	clk.t = utc(2026, 2, 5, 0, 0)
	// b -> c（3 月生效）。
	mustSchedule(t, s, "u", "c")
	clk.t = utc(2026, 4, 2, 0, 0)

	// 先出 3 月账单（当前套餐为 c），再补 1、2 月账单：每张都必须用当时条件。
	marBill := mustBill(t, s, "u", mar(2026))
	if marBill.Terms.PlanID != "c" || marBill.MonthlyFee != 3000 {
		t.Fatalf("mar bill: %+v", marBill)
	}
	janBill := mustBill(t, s, "u", jan(2026))
	if janBill.Terms.PlanID != "a" || janBill.MonthlyFee != 1000 {
		t.Fatalf("jan bill: %+v", janBill)
	}
	febBill := mustBill(t, s, "u", feb(2026))
	if febBill.Terms.PlanID != "b" || febBill.MonthlyFee != 2000 {
		t.Fatalf("feb bill: %+v", febBill)
	}

	// 取消/切换不改变已生成账单。
	mustSchedule(t, s, "u", "a")
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		p    Month
		plan string
		fee  int64
	}{
		{jan(2026), "a", 1000},
		{feb(2026), "b", 2000},
		{mar(2026), "c", 3000},
	} {
		b, err := s.GetBill("u", want.p)
		if err != nil {
			t.Fatalf("get bill %s: %v", want.p, err)
		}
		if b.Terms.PlanID != want.plan || b.TotalDue != want.fee {
			t.Fatalf("bill %s changed: %+v", want.p, b)
		}
	}
}

func mustBill(t *testing.T, s *Service, acct string, p Month) Bill {
	t.Helper()
	b, err := s.CreateBill(acct, p)
	if err != nil {
		t.Fatalf("create bill %s: %v", p, err)
	}
	return b
}

func TestPlanChangeWhileSuspended(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 0, 0, 0))
	mustPlan(t, s, planDef("b", 2000, 0, 0, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 1, 10, 0, 0), Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("u", jan(2026)); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 8, 0, 0)
	st, _ := s.Status("u")
	if !st.Suspended {
		t.Fatal("should be suspended")
	}

	// 停用期间允许安排与取消，但不清除欠费、不解除停用。
	mustSchedule(t, s, "u", "b")
	st, _ = s.Status("u")
	if !st.Suspended || st.PendingChange == nil || st.PendingChange.TargetPlanID != "b" {
		t.Fatalf("schedule while suspended: %+v", st)
	}
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatalf("cancel while suspended: %v", err)
	}
	st, _ = s.Status("u")
	if !st.Suspended || st.PendingChange != nil {
		t.Fatalf("cancel while suspended changed state: %+v", st)
	}

	// 停用期间安排的切换在月初生效，但停用状态按原规则仍由欠费决定。
	mustSchedule(t, s, "u", "b")
	clk.t = utc(2026, 3, 1, 0, 0)
	st, _ = s.Status("u")
	if !st.Suspended || st.CurrentTerms.PlanID != "b" {
		t.Fatalf("suspended switch: %+v", st)
	}
	// 结清后恢复，规则不变。
	if _, err := s.RecordPayment("u", "p1", jan(2026), 1000); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status("u")
	if st.Suspended {
		t.Fatalf("still suspended after payment: %+v", st)
	}
}

func TestConcurrentPlanChangesAndBilling(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 5, 10, 0))
	mustPlan(t, s, planDef("b", 2000, 5, 20, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e", At: utc(2026, 1, 10, 0, 0), Quantity: 8}); err != nil {
		t.Fatal(err)
	}

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n * 3)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = s.SchedulePlanChange("u", "b")
		}(i)
		go func() {
			defer wg.Done()
			_ = s.CancelPlanChange("u")
		}()
		go func(i int) {
			defer wg.Done()
			// 1 月未出账时并发的不同事件：要么计入 1 月、要么因出账被拒，结果自洽。
			_, _ = s.RecordEvent(Event{
				AccountID: "u",
				EventID:   fmt.Sprintf("c%d", i),
				At:        utc(2026, 1, 12, 0, 0),
				Quantity:  1,
			})
		}(i)
	}
	wg.Wait()

	// 无论上述操作如何交错，最终状态必须内部一致。
	st, _ := s.Status("u")
	if st.CurrentTerms.PlanID != "a" {
		t.Fatalf("current plan = %s, want a", st.CurrentTerms.PlanID)
	}

	// 推进到 2 月并并发出账：1 月账单必须恰好使用一个套餐的条件。
	clk.t = utc(2026, 2, 1, 0, 0)
	bills := make([]Bill, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			b, err := s.CreateBill("u", jan(2026))
			if err != nil {
				t.Errorf("concurrent bill: %v", err)
				return
			}
			bills[i] = b
		}(i)
	}
	wg.Wait()
	var first Bill
	for i, b := range bills {
		if i == 0 {
			first = b
			continue
		}
		if b != first {
			t.Fatalf("bills differ:\n%+v\n%+v", first, b)
		}
	}
	if first.Terms.PlanID != "a" {
		t.Fatalf("jan bill terms = %s", first.Terms.PlanID)
	}
	// 内部金额自洽：月费 1000、额度 5、单价 10。
	wantOverage := first.TotalUsage - 5
	if wantOverage < 0 {
		wantOverage = 0
	}
	if first.OverageUnits != wantOverage || first.MonthlyFee != 1000 ||
		first.OverageFee != wantOverage*10 || first.TotalDue != 1000+wantOverage*10 {
		t.Fatalf("jan bill inconsistent: %+v", first)
	}
}
