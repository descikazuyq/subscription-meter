package meter

import (
	"errors"
	"testing"
)

// 回归测试：待生效目标从乙换成丙、再改回乙时，最后一次选择乙是
// 新接受的安排，必须按当时的乙定义重新保存条件快照，不能复用最早
// 那次已被替换的乙安排，也不能混入中途丙的条件。这与连续选择同一
// 待生效目标的重报（返回原安排、不重新取价）不同。
func TestRescheduleBackToPreviousTargetTakesFreshTerms(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 甲：月费 1000、包含 10、超额单价 100、无税。
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	// 乙首次定义：月费 2000、包含 20、超额单价 200、税率 10%。
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	// 丙：月费 5000、包含 50、超额单价 500、税率 25%。
	mustPlan(t, s, planDef("c", 5000, 50, 500, 2500))
	mustAccount(t, s, "u")
	// 账户无欠费，自 2026-01 起使用甲。
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 一月用量 15 单位，发生在甲的有效订阅期间、账户未停用。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-jan", At: utc(2026, 1, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}

	// 一月中旬安排二月改用乙：锁定乙当时的条件（2000/20/200/10%）。
	r1 := mustSchedule(t, s, "u", "b")
	firstB := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 2000, 20, 200, 1000)),
		EffectivePeriod: feb(2026),
	}
	if !r1.Created || r1.Change != firstB {
		t.Fatalf("first schedule b: %+v", r1)
	}

	// 修改乙的定义：四项条件全部改变（3000/30/300/6%）。
	if err := s.UpdatePlan(planDef("b", 3000, 30, 300, 600)); err != nil {
		t.Fatal(err)
	}

	// 把待生效目标改为丙：替换原乙安排，锁定丙当时的条件。
	r2 := mustSchedule(t, s, "u", "c")
	changeC := PlanChange{
		TargetPlanID:    "c",
		Terms:           termsOf(planDef("c", 5000, 50, 500, 2500)),
		EffectivePeriod: feb(2026),
	}
	if !r2.Created || r2.Change != changeC {
		t.Fatalf("schedule c: %+v", r2)
	}

	// 同一月内再改回乙：这是新接受的安排，Created=true，生效账期
	// 仍为二月，四项条件全部取自修改后的乙（3000/30/300/6%）——
	// 不能复用最早那次已被替换的乙安排（2000/20/200/10%），也不能
	// 混入中途丙的条件。
	r3 := mustSchedule(t, s, "u", "b")
	finalB := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 3000, 30, 300, 600)),
		EffectivePeriod: feb(2026),
	}
	if !r3.Created {
		t.Fatalf("switching back to b must create a new change")
	}
	if r3.Change != finalB {
		t.Fatalf("switch back reused replaced change: %+v, want %+v", r3.Change, finalB)
	}

	// 账户只保留这一条待生效安排；一月当前使用的甲条件不变。
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != finalB {
		t.Fatalf("pending after switch back: %+v", st.PendingChange)
	}
	if st.CurrentTerms != termsOf(planDef("a", 1000, 10, 100, 0)) {
		t.Fatalf("current terms changed while waiting: %+v", st.CurrentTerms)
	}

	// 等待安排生效期间，选择当前甲套餐返回同套餐错误，选择不存在的
	// 目标返回套餐不存在错误；两次失败都不能清空或改写那条安排。
	if _, err := s.SchedulePlanChange("u", "a"); !errors.Is(err, ErrPlanChangeSamePlan) {
		t.Fatalf("schedule current plan: %v", err)
	}
	if _, err := s.SchedulePlanChange("u", "missing"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("schedule missing plan: %v", err)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != finalB {
		t.Fatalf("pending rewritten by failed schedules: %+v", st.PendingChange)
	}
	if st.CurrentTerms != termsOf(planDef("a", 1000, 10, 100, 0)) {
		t.Fatalf("current terms changed by failed schedules: %+v", st.CurrentTerms)
	}

	// 再次修改乙的定义（4000/40/400/8%），并在二月到来前连续选择乙：
	// 这是对同一待生效目标的重报，返回最后一次已接受的安排，
	// Created=false，不重新取价，生效月份与已保存条件均不变。
	if err := s.UpdatePlan(planDef("b", 4000, 40, 400, 800)); err != nil {
		t.Fatal(err)
	}
	r4 := mustSchedule(t, s, "u", "b")
	if r4.Created || r4.Change != finalB {
		t.Fatalf("replay same pending target: created=%v change=%+v", r4.Created, r4.Change)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != finalB {
		t.Fatalf("pending after replay: %+v", st.PendingChange)
	}

	// 到达二月月初零点：直接查询即显示最后安排保存的完整乙条件
	// （3000/30/300/6%），待生效安排消失，丙不曾成为当前套餐。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != termsOf(planDef("b", 3000, 30, 300, 600)) || st.PendingChange != nil {
		t.Fatalf("feb current=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 一月账期已结束，出账：仍按甲收 1000 月费、超额 5×100=500、
	// 无税，应付 1500。
	janBill := mustBill(t, s, "u", jan(2026))
	if janBill.Terms != termsOf(planDef("a", 1000, 10, 100, 0)) ||
		janBill.TotalUsage != 15 || janBill.MonthlyFee != 1000 ||
		janBill.OverageUnits != 5 || janBill.OverageFee != 500 ||
		janBill.Tax != 0 || janBill.TotalDue != 1500 {
		t.Fatalf("jan bill: %+v", janBill)
	}

	// 一月账单截止 2026-02-08。当前时刻推进到 2 月 10 日：先结清
	// 一月账单，避免欠费停用阻止二月上报新用量。
	clk.t = utc(2026, 2, 10, 0, 0)
	if _, err := s.RecordPayment("u", "pay-jan", jan(2026), janBill.TotalDue); err != nil {
		t.Fatal(err)
	}

	// 二月用量 35 单位，发生在乙生效后的订阅期间、账户未停用。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-feb", At: utc(2026, 2, 10, 0, 0), Quantity: 35}); err != nil {
		t.Fatal(err)
	}

	// 二月账期结束后出账：按最后安排保存的乙条件收 3000 月费、
	// 超额 5×300=1500、税 (3000+1500)×6%=270，应付 4770——不能
	// 沿用最早的乙价格（2000/20/200/10%）或再次修改后的定义
	// （4000/40/400/8%）。
	clk.t = utc(2026, 3, 1, 0, 0)
	febBill := mustBill(t, s, "u", feb(2026))
	if febBill.Terms != termsOf(planDef("b", 3000, 30, 300, 600)) ||
		febBill.TotalUsage != 35 || febBill.MonthlyFee != 3000 ||
		febBill.OverageUnits != 5 || febBill.OverageFee != 1500 ||
		febBill.Tax != 270 || febBill.TotalDue != 4770 {
		t.Fatalf("feb bill: %+v", febBill)
	}

	// 一月账单的条件、用量和费用不因二月切换或后来的定义修改而变化。
	gotJan, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if gotJan.Terms != termsOf(planDef("a", 1000, 10, 100, 0)) ||
		gotJan.TotalUsage != 15 || gotJan.OverageFee != 500 ||
		gotJan.Tax != 0 || gotJan.TotalDue != 1500 {
		t.Fatalf("jan bill changed: %+v", gotJan)
	}
}
