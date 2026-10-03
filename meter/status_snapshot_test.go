package meter

import "testing"

// 调用方可能保存 Status 结果或修改其中内容用于展示；查询结果必须独立于
// 账户已登记的安排：修改返回内容不能改变订阅，且要体现在后续查询与实际出账中。
func TestStatusPendingChangeIsolatedFromCaller(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 旧套餐：月费 1000、额度 10、超额单价 100、税率 0；
	// 新套餐：月费 2000、额度 20、超额单价 200、税率 1000 万分点。
	mustPlan(t, s, planDef("old", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("new", 2000, 20, 200, 1000))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "old", utc(2026, 1, 1, 0, 0))
	mustSchedule(t, s, "u", "new")

	wantChange := PlanChange{
		TargetPlanID:    "new",
		Terms:           termsOf(planDef("new", 2000, 20, 200, 1000)),
		EffectivePeriod: feb(2026),
	}
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != wantChange {
		t.Fatalf("pending = %+v, want %+v", st.PendingChange, wantChange)
	}

	// 调用方把返回安排中的目标套餐、生效月份以及月费、额度、超额单价和税率
	// 全部改成其他值。
	st.PendingChange.TargetPlanID = "ghost"
	st.PendingChange.EffectivePeriod = mar(2026)
	st.PendingChange.Terms = PlanTerms{
		PlanID:             "ghost",
		MonthlyFee:         9,
		IncludedUnits:      9,
		OveragePrice:       9,
		TaxRateBasisPoints: 9,
	}
	st.CurrentTerms.PlanID = "ghost"
	st.CurrentTerms.MonthlyFee = 9

	// 再查询仍应看到原来登记的完整安排，一月当前条件仍是旧套餐。
	st2, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st2.PendingChange == nil || *st2.PendingChange != wantChange {
		t.Fatalf("pending after caller mutation = %+v, want %+v", st2.PendingChange, wantChange)
	}
	if st2.CurrentTerms != termsOf(planDef("old", 1000, 10, 100, 0)) {
		t.Fatalf("jan current terms = %+v, want old plan", st2.CurrentTerms)
	}

	// 进入二月：账户在原定月初切换到原定新套餐，而非调用方改出的内容。
	clk.t = utc(2026, 2, 1, 0, 0)
	st3, _ := s.Status("u")
	if st3.CurrentTerms != wantChange.Terms || st3.PendingChange != nil {
		t.Fatalf("feb status: cur=%+v pending=%+v", st3.CurrentTerms, st3.PendingChange)
	}

	// 二月累计 25 单位用量，结束后出账：
	// 月费 2000、超额 5*200=1000、税 (2000+1000)*1000/10000=300、总额 3300。
	clk.t = utc(2026, 2, 21, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "f1", At: utc(2026, 2, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "f2", At: utc(2026, 2, 20, 0, 0), Quantity: 10}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 3, 1, 0, 0)
	bill, err := s.CreateBill("u", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	if bill.Terms != wantChange.Terms {
		t.Fatalf("feb bill terms = %+v, want %+v", bill.Terms, wantChange.Terms)
	}
	if bill.MonthlyFee != 2000 || bill.OverageUnits != 5 || bill.OverageFee != 1000 ||
		bill.Tax != 300 || bill.TotalDue != 3300 {
		t.Fatalf("feb bill = %+v, want fee 2000 overage 1000 tax 300 total 3300", bill)
	}
}

// 调用方保留一份未改动的状态，随后通过正常换套餐功能替换安排：
// 新查询显示替换后的安排，早先保存的结果仍展示取得它时的原内容；
// 修改旧结果也不能覆盖刚登记的新安排。
func TestSavedStatusSurvivesPlanChangeReplacement(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	mustPlan(t, s, planDef("c", 3000, 30, 300, 500))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	mustSchedule(t, s, "u", "b")

	saved, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	wantSaved := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 2000, 20, 200, 1000)),
		EffectivePeriod: feb(2026),
	}
	if saved.PendingChange == nil || *saved.PendingChange != wantSaved {
		t.Fatalf("saved pending = %+v, want %+v", saved.PendingChange, wantSaved)
	}

	// 通过正常换套餐功能把安排替换为 c。
	r := mustSchedule(t, s, "u", "c")
	wantNew := PlanChange{
		TargetPlanID:    "c",
		Terms:           termsOf(planDef("c", 3000, 30, 300, 500)),
		EffectivePeriod: feb(2026),
	}
	if !r.Created || r.Change != wantNew {
		t.Fatalf("replace result = %+v, want %+v", r, wantNew)
	}

	// 新查询显示替换后的目标和条件。
	st, _ := s.Status("u")
	if st.PendingChange == nil || *st.PendingChange != wantNew {
		t.Fatalf("pending after replace = %+v, want %+v", st.PendingChange, wantNew)
	}
	// 早先保存的结果仍展示取得它时的原目标、原生效账期及原条件。
	if saved.PendingChange == nil || *saved.PendingChange != wantSaved {
		t.Fatalf("saved result changed after replace: %+v", saved.PendingChange)
	}

	// 调用方随后修改旧结果，也不能覆盖刚登记的新安排。
	saved.PendingChange.TargetPlanID = "a"
	saved.PendingChange.EffectivePeriod = mar(2026)
	saved.PendingChange.Terms = termsOf(planDef("a", 1000, 10, 100, 0))
	st, _ = s.Status("u")
	if st.PendingChange == nil || *st.PendingChange != wantNew {
		t.Fatalf("pending overwritten via saved result: %+v", st.PendingChange)
	}

	// 生效后实际切换的仍是替换后的套餐 c。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms != wantNew.Terms || st.PendingChange != nil {
		t.Fatalf("feb status: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}
}

// 没有待生效安排以及安排已经生效时，状态中的待生效安排都应为空；
// 生效前保存的结果保留当时的安排，生效后的查询不能反过来改变旧结果。
func TestStatusPendingChangeNilAndSavedResultAcrossEffective(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 从未安排换套餐：待生效安排为空，当前条件按实际订阅显示。
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange != nil {
		t.Fatalf("pending without schedule = %+v", st.PendingChange)
	}
	if st.CurrentTerms != termsOf(planDef("a", 1000, 10, 100, 0)) {
		t.Fatalf("current terms = %+v, want plan a", st.CurrentTerms)
	}

	// 安排后、月初生效前保存一份结果。
	mustSchedule(t, s, "u", "b")
	before, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	wantPending := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 2000, 20, 200, 1000)),
		EffectivePeriod: feb(2026),
	}
	if before.PendingChange == nil || *before.PendingChange != wantPending {
		t.Fatalf("saved pre-effective pending = %+v, want %+v", before.PendingChange, wantPending)
	}

	// 生效后取得的结果显示新套餐和空安排。
	clk.t = utc(2026, 2, 1, 0, 0)
	after, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if after.PendingChange != nil {
		t.Fatalf("pending after effective = %+v", after.PendingChange)
	}
	if after.CurrentTerms != wantPending.Terms {
		t.Fatalf("current after effective = %+v, want plan b terms", after.CurrentTerms)
	}

	// 生效前保存的结果仍保留当时的安排，后续查询不能反过来改变它。
	if before.PendingChange == nil || *before.PendingChange != wantPending {
		t.Fatalf("saved result changed after effective: %+v", before.PendingChange)
	}
	// 再次查询（含旧结果被修改之后）也不影响已生效的订阅状态。
	before.PendingChange.TargetPlanID = "a"
	before.PendingChange.EffectivePeriod = jan(2026)
	st, _ = s.Status("u")
	if st.PendingChange != nil || st.CurrentTerms != wantPending.Terms {
		t.Fatalf("status after mutating saved result: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}
}
