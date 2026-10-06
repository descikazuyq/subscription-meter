package meter

import (
	"testing"
)

// 回归测试：取消换套餐安排后重新选择同一套餐，必须按当时的套餐定义
// 重新取价并新建安排，不能复活已取消的旧安排（旧条件、旧生效月份）。
func TestRescheduleSamePlanAfterCancelUsesCurrentTerms(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 甲：月费 1000、包含 10、超额单价 100、税率 0。
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	// 乙首次定义：月费 2000、包含 20、超额单价 200、税率 10%。
	mustPlan(t, s, planDef("b", 2000, 20, 200, 1000))
	mustAccount(t, s, "u")
	// 账户自 2026-01 起使用甲。
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 一月中旬安排二月改用乙：锁定乙当时的条件。
	r1 := mustSchedule(t, s, "u", "b")
	firstChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 2000, 20, 200, 1000)),
		EffectivePeriod: feb(2026),
	}
	if !r1.Created || r1.Change != firstChange {
		t.Fatalf("first schedule: %+v", r1)
	}

	// 修改乙的定义：四项条件全部改变。原安排保留首次接受时的条件。
	if err := s.UpdatePlan(planDef("b", 3000, 30, 300, 600)); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Status("u")
	if st.PendingChange == nil || *st.PendingChange != firstChange {
		t.Fatalf("pending rewritten by update: %+v", st.PendingChange)
	}

	// 取消安排：无待生效安排，甲的完整条件不变。
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, _ = s.Status("u")
	if st.PendingChange != nil || st.CurrentTerms != termsOf(planDef("a", 1000, 10, 100, 0)) {
		t.Fatalf("after cancel: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 到达二月月初：不能自动切换到乙，仍是甲。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms.PlanID != "a" || st.PendingChange != nil {
		t.Fatalf("feb after cancel: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 二月用量 15 单位，发生在甲的有效订阅期间、账户未停用。
	clk.t = utc(2026, 2, 15, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-feb", At: utc(2026, 2, 10, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}

	// 二月中旬再次选择乙：必须返回新安排，Created=true，四项条件全部
	// 取自修改后的乙（3000/30/300/6%），生效账期为三月。套餐标识相同
	// 也不能沿用已取消安排的条件或二月生效月份。
	r2 := mustSchedule(t, s, "u", "b")
	secondChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planDef("b", 3000, 30, 300, 600)),
		EffectivePeriod: mar(2026),
	}
	if !r2.Created {
		t.Fatalf("reschedule after cancel must create a new change")
	}
	if r2.Change != secondChange {
		t.Fatalf("reschedule reused cancelled change: %+v, want %+v", r2.Change, secondChange)
	}

	// 新安排接受后再次修改乙的四项条件，重复选择乙：返回原安排、
	// Created=false，待生效条件保持二月中旬接受时的取值。
	if err := s.UpdatePlan(planDef("b", 4000, 40, 400, 800)); err != nil {
		t.Fatal(err)
	}
	r3 := mustSchedule(t, s, "u", "b")
	if r3.Created || r3.Change != secondChange {
		t.Fatalf("replay after re-schedule: created=%v change=%+v", r3.Created, r3.Change)
	}
	st, _ = s.Status("u")
	if st.PendingChange == nil || *st.PendingChange != secondChange {
		t.Fatalf("pending after replay: %+v", st.PendingChange)
	}

	// 三月月初零点：直接查询即显示安排保存的完整条件，待生效安排消失。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms != termsOf(planDef("b", 3000, 30, 300, 600)) || st.PendingChange != nil {
		t.Fatalf("mar current=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 边界：恰在三月生效月初取消换套餐安排，不能把已生效的乙改回甲。
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatalf("cancel at effective boundary: %v", err)
	}
	st, _ = s.Status("u")
	if st.CurrentTerms != termsOf(planDef("b", 3000, 30, 300, 600)) {
		t.Fatalf("effective switch revoked by cancel: %+v", st.CurrentTerms)
	}

	// 二月账期已结束，出账：按甲收 1000 月费、超额 5×100=500，税 0，
	// 应付 1500。
	febBill := mustBill(t, s, "u", feb(2026))
	if febBill.Terms != termsOf(planDef("a", 1000, 10, 100, 0)) ||
		febBill.TotalUsage != 15 || febBill.MonthlyFee != 1000 ||
		febBill.OverageUnits != 5 || febBill.OverageFee != 500 ||
		febBill.Tax != 0 || febBill.TotalDue != 1500 {
		t.Fatalf("feb bill: %+v", febBill)
	}

	// 截止前结清二月账单，避免欠费停用阻止三月用量。
	if _, err := s.RecordPayment("u", "pay-feb", feb(2026), febBill.TotalDue); err != nil {
		t.Fatal(err)
	}

	// 三月用量 35 单位，发生在新安排生效后的订阅期间、账户未停用。
	clk.t = utc(2026, 3, 5, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-mar", At: utc(2026, 3, 5, 0, 0), Quantity: 35}); err != nil {
		t.Fatal(err)
	}

	// 三月账期结束后出账：按新安排锁定的条件收 3000 月费、
	// 超额 5×300=1500、税 (3000+1500)×6%=270，应付 4770。
	clk.t = utc(2026, 4, 1, 0, 0)
	marBill := mustBill(t, s, "u", mar(2026))
	if marBill.Terms != termsOf(planDef("b", 3000, 30, 300, 600)) ||
		marBill.TotalUsage != 35 || marBill.MonthlyFee != 3000 ||
		marBill.OverageUnits != 5 || marBill.OverageFee != 1500 ||
		marBill.Tax != 270 || marBill.TotalDue != 4770 {
		t.Fatalf("mar bill: %+v", marBill)
	}

	// 二月账单的条件、用量和费用不因三月切换或后来的套餐定义修改而变化。
	gotFeb, err := s.GetBill("u", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	if gotFeb.Terms != termsOf(planDef("a", 1000, 10, 100, 0)) ||
		gotFeb.TotalUsage != 15 || gotFeb.OverageFee != 500 ||
		gotFeb.Tax != 0 || gotFeb.TotalDue != 1500 {
		t.Fatalf("feb bill changed: %+v", gotFeb)
	}
}
