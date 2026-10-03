package meter

import (
	"testing"
	"time"
)

// TestStatusPendingChangeMutationDoesNotAffectSubscription 验证 Status 返回的
// 待生效安排是独立副本：调用方修改返回内容（目标套餐、生效账期、月费、额度、
// 超额单价、税率）不改变账户已登记的安排，后续查询与实际出账仍按原安排执行。
func TestStatusPendingChangeMutationDoesNotAffectSubscription(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 旧套餐 a：月费 1000、额度 10、超额单价 100、税率 0；
	// 新套餐 b：月费 2000、额度 20、超额单价 200、税率 1000 万分点。
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	mustSchedule(t, s, "u", "b")

	wantPending := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 2000, 20, 200, 1000)),
		EffectivePeriod: feb(2026),
	}
	wantCurrent := termsOf(planDef("a", 1000, 10, 100, 0))

	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != wantPending {
		t.Fatalf("pending = %+v, want %+v", st.PendingChange, wantPending)
	}
	if st.CurrentTerms != wantCurrent {
		t.Fatalf("jan current = %+v, want %+v", st.CurrentTerms, wantCurrent)
	}

	// 调用方篡改返回的安排：目标套餐、生效账期以及月费、额度、超额单价、税率。
	st.PendingChange.TargetPlanID = "tampered"
	st.PendingChange.EffectivePeriod = Month{Year: 1999, Month: time.January}
	st.PendingChange.Terms = PlanTerms{
		PlanID:             "tampered",
		MonthlyFee:         1,
		IncludedUnits:      2,
		OveragePrice:       3,
		TaxRateBasisPoints: 4,
	}
	// 连当前条件也一并篡改。
	st.CurrentTerms = st.PendingChange.Terms

	// 再查询：仍应看到原登记的完整安排，一月当前条件仍是旧套餐。
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != wantPending {
		t.Fatalf("pending after caller mutation = %+v, want %+v", st.PendingChange, wantPending)
	}
	if st.CurrentTerms != wantCurrent {
		t.Fatalf("jan current after caller mutation = %+v, want %+v", st.CurrentTerms, wantCurrent)
	}

	// 进入二月：账户在原定月初切换到原定新套餐，待生效安排清空。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != wantPending.Terms {
		t.Fatalf("feb current = %+v, want %+v", st.CurrentTerms, wantPending.Terms)
	}
	if st.PendingChange != nil {
		t.Fatalf("feb pending = %+v, want nil", st.PendingChange)
	}

	// 二月累计 25 单位用量。
	clk.t = utc(2026, 2, 12, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "f", At: utc(2026, 2, 10, 0, 0), Quantity: 25}); err != nil {
		t.Fatal(err)
	}

	// 二月结束后出账：月费 2000、超额 5*200=1000、税 (2000+1000)*1000/10000=300、
	// 总额 3300；账单采用的套餐条件与原安排一致，不受此前篡改影响。
	clk.t = utc(2026, 3, 1, 0, 0)
	bill := mustBill(t, s, "u", feb(2026))
	if bill.Terms != wantPending.Terms {
		t.Fatalf("feb bill terms = %+v, want %+v", bill.Terms, wantPending.Terms)
	}
	if bill.MonthlyFee != 2000 || bill.OverageUnits != 5 || bill.OverageFee != 1000 ||
		bill.Tax != 300 || bill.TotalDue != 3300 {
		t.Fatalf("feb bill = %+v, want fee 2000 overage 1000 tax 300 total 3300", bill)
	}
}

// TestStatusSavedResultSurvivesScheduleReplacement 验证调用方保存的旧状态结果
// 不随后续替换安排而变化，修改旧结果也不能覆盖新登记的安排。
func TestStatusSavedResultSurvivesScheduleReplacement(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	mustPlan(t, s, planDef("c", 3000, 30, 300, 500))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	mustSchedule(t, s, "u", "b")

	wantB := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 2000, 20, 200, 1000)),
		EffectivePeriod: feb(2026),
	}
	wantC := PlanChange{
		TargetPlanID:    "c",
		Terms:           termsOf(planDef("c", 3000, 30, 300, 500)),
		EffectivePeriod: feb(2026),
	}

	// 调用方保留一份未改动的状态。
	saved, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if saved.PendingChange == nil || *saved.PendingChange != wantB {
		t.Fatalf("saved pending = %+v, want %+v", saved.PendingChange, wantB)
	}

	// 通过正常换套餐功能替换安排：b -> c。
	r := mustSchedule(t, s, "u", "c")
	if !r.Created || r.Change != wantC {
		t.Fatalf("replace schedule = %+v, want change %+v", r, wantC)
	}

	// 新查询显示替换后的目标和条件。
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != wantC {
		t.Fatalf("pending after replace = %+v, want %+v", st.PendingChange, wantC)
	}
	// 早先保存的结果仍展示取得它时的原目标、原生效账期及原条件。
	if saved.PendingChange == nil || *saved.PendingChange != wantB {
		t.Fatalf("saved result changed after replace = %+v, want %+v", saved.PendingChange, wantB)
	}

	// 调用方随后修改旧结果，也不能覆盖刚登记的新安排。
	saved.PendingChange.TargetPlanID = "tampered"
	saved.PendingChange.EffectivePeriod = Month{Year: 1999, Month: time.January}
	saved.PendingChange.Terms = PlanTerms{PlanID: "tampered", MonthlyFee: 1}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != wantC {
		t.Fatalf("pending after mutating saved result = %+v, want %+v", st.PendingChange, wantC)
	}
}

// TestStatusPendingChangeEmptyStates 验证没有待生效安排以及安排已生效时，
// 状态中的待生效安排为空；生效前保存的结果不受后续查询影响。
func TestStatusPendingChangeEmptyStates(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	termsA := termsOf(planDef("a", 1000, 10, 100, 0))
	termsB := termsOf(planDef("b", 2000, 20, 200, 1000))

	// 没有待生效安排：PendingChange 为空，当前条件按实际订阅显示。
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange != nil {
		t.Fatalf("pending without schedule = %+v, want nil", st.PendingChange)
	}
	if st.CurrentTerms != termsA {
		t.Fatalf("current = %+v, want %+v", st.CurrentTerms, termsA)
	}

	// 安排换套餐，在月初生效前保存一份状态。
	mustSchedule(t, s, "u", "b")
	wantPending := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsB,
		EffectivePeriod: feb(2026),
	}
	before, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if before.PendingChange == nil || *before.PendingChange != wantPending {
		t.Fatalf("pending before effect = %+v, want %+v", before.PendingChange, wantPending)
	}

	// 安排生效后：当前条件为新套餐，待生效安排为空。
	clk.t = utc(2026, 2, 1, 0, 0)
	after, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if after.CurrentTerms != termsB {
		t.Fatalf("current after effect = %+v, want %+v", after.CurrentTerms, termsB)
	}
	if after.PendingChange != nil {
		t.Fatalf("pending after effect = %+v, want nil", after.PendingChange)
	}

	// 生效前保存的结果仍保留当时的安排。
	if before.PendingChange == nil || *before.PendingChange != wantPending {
		t.Fatalf("saved pre-effect result changed = %+v, want %+v", before.PendingChange, wantPending)
	}

	// 后续查询不能反过来改变旧结果。
	if _, err := s.Status("u"); err != nil {
		t.Fatal(err)
	}
	if before.PendingChange == nil || *before.PendingChange != wantPending {
		t.Fatalf("saved pre-effect result changed by later query = %+v, want %+v", before.PendingChange, wantPending)
	}
	if before.CurrentTerms != termsA {
		t.Fatalf("saved pre-effect current changed = %+v, want %+v", before.CurrentTerms, termsA)
	}
}
