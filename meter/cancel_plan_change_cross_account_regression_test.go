package meter

import "testing"

// 回归测试：取消换套餐安排只作用于请求指定的账户。多个账户可以使用
// 同一个当前套餐，也可以各自安排下月换到同一个目标套餐；其中一个账户
// 取消安排后，其他账户已接受的安排必须独立保留，不能因为当前套餐或
// 目标套餐相同而被一并清除。取消换套餐安排也不能被当成取消订阅：
// 两个账户的订阅都继续有效。
func TestCancelPlanChangeAffectsOnlyRequestedAccount(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 旧套餐：月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%。
	mustPlan(t, s, planDef("plan-old", 1000, 10, 100, 1000))
	// 新套餐：月费 2000 分、包含 20 单位、超额单价 200 分、税率 6%。
	mustPlan(t, s, planDef("plan-new", 2000, 20, 200, 600))
	mustAccount(t, s, "acct-cancel")
	mustAccount(t, s, "acct-keep")
	// 两个账户都没有欠费，订阅均自 2026-01-01 00:00 UTC 起生效，
	// 从一月起使用同一个旧套餐。
	mustSubscribe(t, s, "acct-cancel", "plan-old", utc(2026, 1, 1, 0, 0))
	mustSubscribe(t, s, "acct-keep", "plan-old", utc(2026, 1, 1, 0, 0))

	oldTerms := termsOf(planDef("plan-old", 1000, 10, 100, 1000))
	newTerms := termsOf(planDef("plan-new", 2000, 20, 200, 600))
	wantChange := PlanChange{
		TargetPlanID:    "plan-new",
		Terms:           newTerms,
		EffectivePeriod: feb(2026),
	}

	// 一月中旬两个账户分别安排二月改用同一个新套餐：各自接受安排，
	// 锁定相同的目标套餐完整条件与生效账期。
	for _, acct := range []string{"acct-cancel", "acct-keep"} {
		r := mustSchedule(t, s, acct, "plan-new")
		if !r.Created || r.Change != wantChange {
			t.Fatalf("schedule %s: %+v", acct, r)
		}
	}

	// 只为 acct-cancel 取消换套餐安排。
	if err := s.CancelPlanChange("acct-cancel"); err != nil {
		t.Fatalf("cancel plan change: %v", err)
	}

	// 取消成功后：acct-cancel 继续显示旧套餐且没有待生效安排；
	// acct-keep 仍显示旧套餐，并保留原目标套餐、二月生效账期以及
	// 安排接受时保存的完整计费条件。两个账户的订阅都继续有效——
	// 取消换套餐安排不是取消订阅，ScheduledEnd 必须为空。
	st, err := s.Status("acct-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd != nil {
		t.Fatalf("acct-cancel subscription broken by cancel: %+v", st)
	}
	if st.CurrentTerms != oldTerms || st.PendingChange != nil {
		t.Fatalf("acct-cancel after cancel: cur=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}
	st, err = s.Status("acct-keep")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd != nil {
		t.Fatalf("acct-keep subscription broken by other's cancel: %+v", st)
	}
	if st.CurrentTerms != oldTerms {
		t.Fatalf("acct-keep current changed: %+v", st.CurrentTerms)
	}
	if st.PendingChange == nil || *st.PendingChange != wantChange {
		t.Fatalf("acct-keep pending cleared by other's cancel: %+v", st.PendingChange)
	}

	// 时间边界一：安排生效前，重复取消已经没有安排的 acct-cancel
	// 仍成功，且不改变 acct-keep 保留的安排。
	if err := s.CancelPlanChange("acct-cancel"); err != nil {
		t.Fatalf("re-cancel without pending: %v", err)
	}
	st, err = s.Status("acct-keep")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != wantChange {
		t.Fatalf("acct-keep pending changed by re-cancel: %+v", st.PendingChange)
	}

	// 到达 2026-02-01 00:00 UTC（二月月初零点）：直接查询即应看出
	// 区别，不依赖先上报用量或出账——acct-cancel 仍使用旧套餐，
	// acct-keep 已使用安排锁定的新套餐条件且不再显示待生效安排。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, err = s.Status("acct-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.CurrentTerms != oldTerms || st.PendingChange != nil {
		t.Fatalf("acct-cancel at feb: %+v", st)
	}
	st, err = s.Status("acct-keep")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.CurrentTerms != newTerms || st.PendingChange != nil {
		t.Fatalf("acct-keep at feb: %+v", st)
	}

	// 时间边界二：恰好到达二月生效时刻后，再取消 acct-keep 的换套餐
	// 安排也应成功，但不能把已经生效的新套餐退回旧套餐。
	if err := s.CancelPlanChange("acct-keep"); err != nil {
		t.Fatalf("cancel after effective: %v", err)
	}
	st, err = s.Status("acct-keep")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.CurrentTerms != newTerms || st.PendingChange != nil {
		t.Fatalf("acct-keep reverted by late cancel: %+v", st)
	}

	// 两个账户的一月账期已结束：各自出账并结清（均无用量，应付
	// 1000+税100=1100，截止 2026-02-08），保持无欠费状态，避免
	// 欠费停用阻止二月上报用量。出账与付款都按账户各自进行。
	for _, acct := range []string{"acct-cancel", "acct-keep"} {
		janBill := mustBill(t, s, acct, jan(2026))
		if janBill.AccountID != acct || janBill.Terms != oldTerms ||
			janBill.TotalUsage != 0 || janBill.TotalDue != 1100 {
			t.Fatalf("jan bill %s: %+v", acct, janBill)
		}
		if _, err := s.RecordPayment(acct, "pay-jan", jan(2026), janBill.TotalDue); err != nil {
			t.Fatalf("pay jan %s: %v", acct, err)
		}
	}

	// 二月两个账户各自累计 25 单位用量：事件按账户分别上报，
	// 用量不能混到另一个账户。
	clk.t = utc(2026, 2, 10, 0, 0)
	for _, acct := range []string{"acct-cancel", "acct-keep"} {
		if _, err := s.RecordEvent(Event{
			AccountID: acct, EventID: "e-feb",
			At: utc(2026, 2, 10, 0, 0), Quantity: 25,
		}); err != nil {
			t.Fatalf("record feb event %s: %v", acct, err)
		}
	}
	for _, acct := range []string{"acct-cancel", "acct-keep"} {
		if u, _ := s.MonthlyUsage(acct, feb(2026)); u.Total != 25 {
			t.Fatalf("feb usage %s = %d, want 25", acct, u.Total)
		}
	}

	// 二月账期结束后各自出账：账单采用的套餐条件和所属账户应与各自
	// 二月的状态相符——acct-cancel 仍按旧套餐（超额 15×100=1500、
	// 税 (1000+1500)×10%=250、应付 2750），acct-keep 按新套餐
	// （超额 5×200=1000、税 (2000+1000)×6%=180、应付 3180），
	// 生效后的取消没有把 acct-keep 的计费退回旧套餐。
	clk.t = utc(2026, 3, 1, 0, 0)
	cancelBill := mustBill(t, s, "acct-cancel", feb(2026))
	if cancelBill.AccountID != "acct-cancel" || cancelBill.Terms != oldTerms ||
		cancelBill.TotalUsage != 25 || cancelBill.OverageUnits != 15 ||
		cancelBill.OverageFee != 1500 || cancelBill.Tax != 250 ||
		cancelBill.TotalDue != 2750 {
		t.Fatalf("feb bill acct-cancel: %+v", cancelBill)
	}
	keepBill := mustBill(t, s, "acct-keep", feb(2026))
	if keepBill.AccountID != "acct-keep" || keepBill.Terms != newTerms ||
		keepBill.TotalUsage != 25 || keepBill.OverageUnits != 5 ||
		keepBill.OverageFee != 1000 || keepBill.Tax != 180 ||
		keepBill.TotalDue != 3180 {
		t.Fatalf("feb bill acct-keep: %+v", keepBill)
	}

	// 查询各自账单：与出账结果一致，互不串户。
	gotCancel, err := s.GetBill("acct-cancel", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	if gotCancel != cancelBill {
		t.Fatalf("feb bill acct-cancel changed: %+v", gotCancel)
	}
	gotKeep, err := s.GetBill("acct-keep", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	if gotKeep != keepBill {
		t.Fatalf("feb bill acct-keep changed: %+v", gotKeep)
	}

	// 全部操作之后两个账户的订阅仍然有效：取消换套餐安排（无论生效前
	// 还是生效后）都没有被当成取消订阅。
	for _, acct := range []string{"acct-cancel", "acct-keep"} {
		st, err = s.Status(acct)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Subscribed || st.ScheduledEnd != nil {
			t.Fatalf("%s subscription cancelled by plan-change cancel: %+v", acct, st)
		}
	}
}
