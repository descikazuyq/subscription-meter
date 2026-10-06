package meter

import (
	"testing"
)

// 本文件为“取消换套餐安排后重新选择同一套餐”提供端到端回归保障：取消必须真正
// 移除已接受的安排，之后重新选择同一目标套餐时要按当时的套餐定义重新锁定完整
// 条件与新的生效账期（Created=true），不能取回已经取消的旧安排；而新安排接受
// 之后再次修改套餐定义，重复选择仍返回该安排（Created=false），不重新取价。
// 全部只走公开入口（SchedulePlanChange / CancelPlanChange / Status /
// RecordEvent / CreateBill / GetBill / MonthlyUsage），计价沿用原有规则。
//
// 主时间线（全部按 UTC 自然月判断）：
//
//	2026-01-01 账户开通套餐甲：月费 1000、额度 10、超额单价 100、税率 0。
//	2026-01-15 安排 2 月起换套餐乙；乙当时（旧定义）月费 2000、额度 20、
//	           超额单价 200、税率 10%；安排锁定该完整快照。
//	之后       把乙改为月费 3000、额度 30、超额单价 300、税率 6%：
//	           旧安排仍保留首次接受时的条件。
//	1 月内     CancelPlanChange：待生效安排消失，甲的完整条件不变；
//	           2 月月初也不会自动切到乙。
//	2026-02-15 再次选择乙：必须是新安排（Created=true），四项计费条件全部取
//	           修改后的乙，生效账期为 3 月——套餐标识相同也不能沿用已取消的
//	           条件或 2 月生效月份。
//	新安排接受后再把乙改成第三套定义：重复选择乙返回原安排（Created=false），
//	           待生效条件保持 2 月中旬接受时的取值。
//	2026-03-01 直接查询：显示该安排保存的完整条件，待生效安排消失。
//	2 月用量 15（仍按甲：月费 1000、超额 5×100=500、应付 1500）；
//	3 月用量 35（按新安排的乙：月费 3000、超额 5×300=1500、税 270、应付 4770）。
//	两份账单都在各自账期结束后生成；2 月账单不因 3 月切换或后来修改套餐定义而变。
func TestCancelPlanChangeThenReselectSamePlanSnapshotsCurrentTerms(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))

	jia := planDef("plan-a", 1000, 10, 100, 0)
	yiOld := planDef("plan-b", 2000, 20, 200, 1000)  // 首次安排时的乙
	yiMid := planDef("plan-b", 3000, 30, 300, 600)   // 取消后重新选择时的乙
	yiLater := planDef("plan-b", 4000, 40, 400, 500) // 新安排接受后再次修改
	yiFinal := planDef("plan-b", 9999, 9, 999, 999)  // 出账后再改，不得影响账单
	jiaTerms := termsOf(jia)
	yiOldTerms := termsOf(yiOld)
	yiMidTerms := termsOf(yiMid)

	mustPlan(t, s, jia)
	mustPlan(t, s, yiOld)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "plan-a", utc(2026, 1, 1, 0, 0))

	// 一月中旬安排二月改用乙：锁定乙当时的完整条件与生效账期 2026-02。
	first := mustSchedule(t, s, "u", "plan-b")
	wantFirst := PlanChange{TargetPlanID: "plan-b", Terms: yiOldTerms, EffectivePeriod: feb(2026)}
	if !first.Created || first.Change != wantFirst {
		t.Fatalf("first schedule = %+v, want created %+v", first, wantFirst)
	}

	// 修改乙的四项计费条件：已接受的旧安排不被改写，仍持首次接受时的快照。
	if err := s.UpdatePlan(yiMid); err != nil {
		t.Fatalf("update plan-b: %v", err)
	}
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != wantFirst || st.PendingChange.Terms != yiOldTerms {
		t.Fatalf("pending change re-priced by update: %+v", st.PendingChange)
	}

	// 取消换套餐安排。
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatalf("cancel plan change: %v", err)
	}
	st, _ = s.Status("u")
	if !st.Subscribed || st.PendingChange != nil || st.ScheduledEnd != nil {
		t.Fatalf("status after cancel = %+v, want subscribed with no pending change", st)
	}
	// 甲的完整条件（月费、额度、超额单价、税率）一项都不能变。
	if st.CurrentTerms != jiaTerms {
		t.Fatalf("plan-a terms changed after cancel: %+v, want %+v", st.CurrentTerms, jiaTerms)
	}

	// 到达二月月初：安排已取消，不能自动切换到乙；直接查询仍是甲且无待生效安排。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms != jiaTerms || st.PendingChange != nil {
		t.Fatalf("feb status after cancel = %+v, want full plan-a with no pending change", st)
	}

	// 二月接收 15 单位用量（发生在二月的有效订阅期间、账户未欠费停用），
	// 此时仍按甲计入二月账期。
	clk.t = utc(2026, 2, 15, 12, 0)
	if st, _ = s.Status("u"); st.Suspended {
		t.Fatalf("account should not be suspended before billing: %+v", st)
	}
	ev, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-feb", At: utc(2026, 2, 10, 0, 0), Quantity: 15})
	if err != nil || !ev.Accepted || ev.Period != feb(2026) {
		t.Fatalf("feb event = %+v %v, want accepted in 2026-02", ev, err)
	}

	// 二月中旬再次选择同一套餐乙：必须是新安排，Created=true，四项计费条件全部
	// 取自修改后的乙，生效账期为三月。
	reselect := mustSchedule(t, s, "u", "plan-b")
	wantReselect := PlanChange{TargetPlanID: "plan-b", Terms: yiMidTerms, EffectivePeriod: mar(2026)}
	if !reselect.Created {
		t.Fatalf("reselect after cancel Created=false, change=%+v; want a brand-new arrangement", reselect.Change)
	}
	if reselect.Change != wantReselect {
		t.Fatalf("reselect change = %+v, want %+v", reselect.Change, wantReselect)
	}
	// 显式锁定回归点：相同套餐标识不能沿用已取消的旧条件，也不能沿用二月生效月份。
	if reselect.Change.Terms == yiOldTerms {
		t.Fatalf("reselect reused cancelled terms %+v", reselect.Change.Terms)
	}
	if reselect.Change.EffectivePeriod == feb(2026) {
		t.Fatalf("reselect kept cancelled effective month 2026-02, want 2026-03")
	}
	st, _ = s.Status("u")
	if st.CurrentTerms != jiaTerms || st.PendingChange == nil || *st.PendingChange != wantReselect {
		t.Fatalf("status after reselect: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 新安排接受后再次修改乙的四项条件：重复选择乙返回原安排、Created=false，
	// 不重新取价；状态中的待生效条件保持二月中旬接受时的取值。
	if err := s.UpdatePlan(yiLater); err != nil {
		t.Fatalf("update plan-b again: %v", err)
	}
	replay := mustSchedule(t, s, "u", "plan-b")
	if replay.Created || replay.Change != wantReselect {
		t.Fatalf("replay after new acceptance = created=%v %+v, want original %+v",
			replay.Created, replay.Change, wantReselect)
	}
	st, _ = s.Status("u")
	if st.PendingChange == nil || st.PendingChange.Terms != yiMidTerms ||
		st.PendingChange.EffectivePeriod != mar(2026) {
		t.Fatalf("pending terms changed after second update: %+v", st.PendingChange)
	}

	// 三月月初直接查询（先不上报用量、不出账）：显示安排保存的完整条件，
	// 待生效安排消失；条件是二月中旬锁定的乙，不是任何更晚的套餐定义。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms != yiMidTerms || st.PendingChange != nil {
		t.Fatalf("mar status = %+v, want effective locked plan-b terms and no pending", st)
	}

	// 三月接收 35 单位用量：账户仍未欠费停用（尚无账单），事件归三月账期。
	clk.t = utc(2026, 3, 10, 12, 0)
	if st, _ = s.Status("u"); st.Suspended {
		t.Fatalf("account should not be suspended before billing: %+v", st)
	}
	ev, err = s.RecordEvent(Event{AccountID: "u", EventID: "e-mar", At: utc(2026, 3, 10, 0, 0), Quantity: 35})
	if err != nil || !ev.Accepted || ev.Period != mar(2026) {
		t.Fatalf("mar event = %+v %v, want accepted in 2026-03", ev, err)
	}
	if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 15 {
		t.Fatalf("feb usage = %d, want 15", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 35 {
		t.Fatalf("mar usage = %d, want 35", u.Total)
	}

	// 两份账单都在各自账期结束后（四月月初）生成。
	clk.t = utc(2026, 4, 1, 0, 0)

	// 二月账单按甲：用量 15，超额 5×100=500，月费 1000，税率 0，应付 1500。
	febBill := mustBill(t, s, "u", feb(2026))
	if febBill.Terms != jiaTerms || febBill.TotalUsage != 15 ||
		febBill.IncludedUnits != 10 || febBill.OverageUnits != 5 ||
		febBill.MonthlyFee != 1000 || febBill.OverageFee != 500 ||
		febBill.Tax != 0 || febBill.TotalDue != 1500 {
		t.Fatalf("feb bill = %+v, want plan-a usage 15 fee 1000 overage 500 tax 0 due 1500", febBill)
	}
	if !febBill.DueAt.Equal(utc(2026, 3, 8, 0, 0)) {
		t.Fatalf("feb due at = %v, want 2026-03-08", febBill.DueAt)
	}

	// 三月账单按新安排锁定的乙：用量 35，超额 5×300=1500，月费 3000，
	// 税 (3000+1500)×6%=270，应付 4770；不能混入更晚的套餐定义。
	marBill := mustBill(t, s, "u", mar(2026))
	if marBill.Terms != yiMidTerms || marBill.TotalUsage != 35 ||
		marBill.IncludedUnits != 30 || marBill.OverageUnits != 5 ||
		marBill.MonthlyFee != 3000 || marBill.OverageFee != 1500 ||
		marBill.Tax != 270 || marBill.TotalDue != 4770 {
		t.Fatalf("mar bill = %+v, want locked plan-b usage 35 fee 3000 overage 1500 tax 270 due 4770", marBill)
	}
	if !marBill.DueAt.Equal(utc(2026, 4, 8, 0, 0)) {
		t.Fatalf("mar due at = %v, want 2026-04-08", marBill.DueAt)
	}

	// 出账后再改乙的定义：已生成账单的条件、用量与费用保持原样，
	// 二月账单尤其不因三月切换或之后的定义修改而变化。
	if err := s.UpdatePlan(yiFinal); err != nil {
		t.Fatalf("update plan-b after billing: %v", err)
	}
	gotFeb, _ := s.GetBill("u", feb(2026))
	if gotFeb.Terms != jiaTerms || gotFeb.TotalUsage != 15 || gotFeb.TotalDue != 1500 {
		t.Fatalf("feb bill changed after mar switch/later update: %+v", gotFeb)
	}
	gotMar, _ := s.GetBill("u", mar(2026))
	if gotMar.Terms != yiMidTerms || gotMar.TotalUsage != 35 || gotMar.TotalDue != 4770 {
		t.Fatalf("mar bill changed after later plan update: %+v", gotMar)
	}
	if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 15 {
		t.Fatalf("feb usage changed = %d, want 15", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 35 {
		t.Fatalf("mar usage changed = %d, want 35", u.Total)
	}
}

// TestCancelPlanChangeExactlyAtEffectiveMonthCannotRevertSwitch 锁定取消的边界：
// 安排在三月月初零点已经生效，恰在该瞬间调用 CancelPlanChange 也只能先承认
// 这次切换，不能把已经生效的乙改回甲（按 UTC 自然月判断，同一瞬间与时区无关）。
func TestCancelPlanChangeExactlyAtEffectiveMonthCannotRevertSwitch(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	jia := planDef("plan-a", 1000, 10, 100, 0)
	yi := planDef("plan-b", 3000, 30, 300, 600)
	jiaTerms := termsOf(jia)
	yiTerms := termsOf(yi)
	mustPlan(t, s, jia)
	mustPlan(t, s, yi)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "plan-a", utc(2026, 1, 1, 0, 0))

	// 二月中旬安排三月起换乙（锁定乙当时的完整条件，生效 2026-03）。
	clk.t = utc(2026, 2, 15, 12, 0)
	if st, _ := s.Status("u"); st.CurrentTerms != jiaTerms {
		t.Fatalf("feb current = %+v, want plan-a", st.CurrentTerms)
	}
	chg := mustSchedule(t, s, "u", "plan-b")
	wantChange := PlanChange{TargetPlanID: "plan-b", Terms: yiTerms, EffectivePeriod: mar(2026)}
	if !chg.Created || chg.Change != wantChange {
		t.Fatalf("schedule = %+v, want created %+v", chg, wantChange)
	}

	// 恰在三月生效月初零点取消：调用成功，但切换已先承认，不可撤回。
	clk.t = utc(2026, 3, 1, 0, 0)
	if err := s.CancelPlanChange("u"); err != nil {
		t.Fatalf("cancel at effective boundary: %v", err)
	}
	st, _ := s.Status("u")
	if !st.Subscribed || st.PendingChange != nil || st.CurrentTerms != yiTerms {
		t.Fatalf("status after boundary cancel = %+v, want effective plan-b, no revert to plan-a", st)
	}

	// 再往后查询仍停留在乙，取消没有把它改回甲。
	clk.t = utc(2026, 3, 10, 0, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms != yiTerms || st.PendingChange != nil {
		t.Fatalf("later status = %+v, want plan-b retained", st)
	}

	// 账期结束后出账：三月按乙、二月按甲，边界取消不改变历史归属与计价。
	clk.t = utc(2026, 4, 1, 0, 0)
	marBill := mustBill(t, s, "u", mar(2026))
	if marBill.Terms != yiTerms || marBill.MonthlyFee != 3000 {
		t.Fatalf("mar bill after boundary cancel = %+v, want plan-b terms", marBill)
	}
	febBill := mustBill(t, s, "u", feb(2026))
	if febBill.Terms != jiaTerms || febBill.MonthlyFee != 1000 {
		t.Fatalf("feb bill = %+v, want plan-a terms", febBill)
	}
}
