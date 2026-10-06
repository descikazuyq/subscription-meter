package meter

import (
	"errors"
	"testing"
)

// 回归测试：待生效目标从乙换成丙、再改回乙时，最后一次选择乙必须按当时的
// 套餐定义重新取价并新建安排，不能复用最早那次已被丙替换掉的乙安排；而在
// 生效前连续选择同一个目标乙，仍返回最后一次已接受的安排，不重新取价。
// 两者的区别只在于“当前待生效目标是否就是乙”：改回乙时当前待生效是丙，
// 属于替换；连续选乙时当前待生效已是乙，属于重报。
func TestSwitchBackToPlanSavesCurrentTerms(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 甲：月费 1000、包含 10、超额单价 100、无税。
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	// 乙首次定义：月费 2000、包含 20、超额单价 200、税率 10%。
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	// 丙：与乙的任一定义都明显不同，便于发现丙条件混入。
	mustPlan(t, s, planDef("c", 7000, 70, 700, 700))
	mustAccount(t, s, "u")
	// 无欠费、已生效且正在使用甲的账户。
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 一月用量 15 单位。
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-jan",
		At: utc(2026, 1, 10, 0, 0), Quantity: 15,
	}); err != nil {
		t.Fatal(err)
	}

	// 一月中旬先安排二月改用乙：锁定乙最早的条件，生效账期为二月。
	firstB := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 2000, 20, 200, 1000)),
		EffectivePeriod: feb(2026),
	}
	r1 := mustSchedule(t, s, "u", "b")
	if !r1.Created || r1.Change != firstB {
		t.Fatalf("first schedule to b: %+v, want %+v", r1.Change, firstB)
	}

	// 修改乙定义：月费 3000、包含 30、超额单价 300、税率 6%。
	if err := s.UpdatePlan(planDef("b", 3000, 30, 300, 600)); err != nil {
		t.Fatal(err)
	}

	// 把待生效目标改为丙：替换原乙安排，仍在一月内接受故生效账期仍是二月。
	rC := mustSchedule(t, s, "u", "c")
	cChange := PlanChange{
		TargetPlanID:    "c",
		Terms:           termsOf(planDef("c", 7000, 70, 700, 700)),
		EffectivePeriod: feb(2026),
	}
	if !rC.Created || rC.Change != cChange {
		t.Fatalf("switch to c: %+v, want %+v", rC.Change, cChange)
	}
	st, _ := s.Status("u")
	if st.PendingChange == nil || *st.PendingChange != cChange {
		t.Fatalf("pending after switching to c: %+v", st.PendingChange)
	}

	// 同一月内再改回乙：当前待生效目标是丙而不是乙，这次选择乙必须被视为
	// 新接受的安排（Created=true），按修改后的乙重新取价，生效账期仍为二月。
	r2 := mustSchedule(t, s, "u", "b")
	newB := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 3000, 30, 300, 600)),
		EffectivePeriod: feb(2026),
	}
	if !r2.Created {
		t.Fatalf("switch back to b must be a newly accepted change")
	}
	if r2.Change != newB {
		t.Fatalf("switch back to b reused stale terms: %+v, want %+v", r2.Change, newB)
	}

	// 账户只保留这一条待生效安排：四项条件全部来自修改后的乙，
	// 最早的乙条件与中途选择的丙条件都不能混入。
	st, _ = s.Status("u")
	if st.PendingChange == nil {
		t.Fatalf("pending change missing after switch back")
	}
	if *st.PendingChange != newB {
		t.Fatalf("pending after switch back: %+v, want %+v", *st.PendingChange, newB)
	}
	if st.PendingChange.Terms == firstB.Terms {
		t.Fatalf("pending kept the original b terms that should have been replaced: %+v", st.PendingChange.Terms)
	}
	// 一月当前使用的甲条件始终不变。
	if st.CurrentTerms != termsOf(planDef("a", 1000, 10, 100, 0)) {
		t.Fatalf("current jan terms changed: %+v", st.CurrentTerms)
	}

	// 再次修改乙定义（4000/40/400/8%），并在二月到来前连续选择乙：
	// 当前待生效目标已是乙，两次都应返回最后一次已接受的安排，
	// Created=false，生效月份与已保存条件均不改变，也不按新定义重新取价。
	if err := s.UpdatePlan(planDef("b", 4000, 40, 400, 800)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r := mustSchedule(t, s, "u", "b")
		if r.Created || r.Change != newB {
			t.Fatalf("consecutive select b #%d created=%v change=%+v, want saved %+v",
				i, r.Created, r.Change, newB)
		}
	}
	st, _ = s.Status("u")
	if st.PendingChange == nil || *st.PendingChange != newB {
		t.Fatalf("pending after consecutive selects: %+v, want %+v", st.PendingChange, newB)
	}

	// 等待安排生效期间选择当前甲套餐：返回同套餐错误，且不清空、不改写那条安排。
	if _, err := s.SchedulePlanChange("u", "a"); !errors.Is(err, ErrPlanChangeSamePlan) {
		t.Fatalf("schedule current plan a: err=%v, want ErrPlanChangeSamePlan", err)
	}
	st, _ = s.Status("u")
	if st.PendingChange == nil || *st.PendingChange != newB {
		t.Fatalf("same-plan error altered pending: %+v", st.PendingChange)
	}
	// 选择不存在的目标套餐：返回套餐不存在错误，安排同样保持不变。
	if _, err := s.SchedulePlanChange("u", "missing"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("schedule missing plan: err=%v, want ErrPlanNotFound", err)
	}
	st, _ = s.Status("u")
	if st.PendingChange == nil || *st.PendingChange != newB {
		t.Fatalf("missing-plan error altered pending: %+v", st.PendingChange)
	}

	// 二月月初零点：直接查询即显示最后安排保存的完整乙条件，待生效安排消失，
	// 丙从不曾成为当前套餐。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms != termsOf(planDef("b", 3000, 30, 300, 600)) {
		t.Fatalf("current feb terms: %+v, want saved b terms", st.CurrentTerms)
	}
	if st.PendingChange != nil {
		t.Fatalf("pending change should be gone in feb: %+v", st.PendingChange)
	}

	// 一月账期已结束，出账：甲收月费 1000、超额 5×100=500、无税，应付 1500。
	janBill := mustBill(t, s, "u", jan(2026))
	if janBill.Terms != termsOf(planDef("a", 1000, 10, 100, 0)) ||
		janBill.TotalUsage != 15 || janBill.MonthlyFee != 1000 ||
		janBill.IncludedUnits != 10 || janBill.OverageUnits != 5 ||
		janBill.OverageFee != 500 || janBill.Tax != 0 || janBill.TotalDue != 1500 {
		t.Fatalf("jan bill: %+v", janBill)
	}

	// 推进到 2 月 10 日（已过一月账单截止 2026-02-08）：先在未欠费停用的
	// 状态下结清一月账单，再上报二月用量，避免停用拦截。
	clk.t = utc(2026, 2, 10, 0, 0)
	if _, err := s.RecordPayment("u", "pay-jan", jan(2026), janBill.TotalDue); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-feb",
		At: utc(2026, 2, 10, 0, 0), Quantity: 35,
	}); err != nil {
		t.Fatal(err)
	}

	// 二月账期结束后出账：按最后安排保存的乙条件（3000/30/300/6%）计费，
	// 既不沿用最早的乙价格（2000/20/200/10%），也不采用后来再次修改后的
	// 定义（4000/40/400/8%）。用量 35，超额 5×300=1500，
	// 税 (3000+1500)×6%=270，应付 4770。
	clk.t = utc(2026, 3, 1, 0, 0)
	febBill := mustBill(t, s, "u", feb(2026))
	if febBill.Terms != termsOf(planDef("b", 3000, 30, 300, 600)) ||
		febBill.TotalUsage != 35 || febBill.MonthlyFee != 3000 ||
		febBill.IncludedUnits != 30 || febBill.OverageUnits != 5 ||
		febBill.OverageFee != 1500 || febBill.Tax != 270 || febBill.TotalDue != 4770 {
		t.Fatalf("feb bill: %+v", febBill)
	}
	if febBill.Terms.PlanID == "c" {
		t.Fatalf("c must never have become the current plan: %+v", febBill.Terms)
	}

	// 一月账单结果不被二月切换或后续定义修改影响。
	janAgain, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if janAgain.Terms != termsOf(planDef("a", 1000, 10, 100, 0)) ||
		janAgain.OverageFee != 500 || janAgain.Tax != 0 || janAgain.TotalDue != 1500 {
		t.Fatalf("jan bill changed afterward: %+v", janAgain)
	}
}
