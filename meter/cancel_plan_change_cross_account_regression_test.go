package meter

import (
	"testing"
	"time"
)

// 本文件为“取消换套餐安排只作用于请求指定的账户”补充跨账户回归保障。
//
// 多个账户可以使用同一个当前套餐，也可以各自安排在下一个 UTC 自然月换到
// 同一个目标套餐；安排、生效与取消的归属域都是单个账户：只为其中一个账户
// 取消换套餐安排时，其他账户已接受的安排必须独立保留——不能因为当前套餐
// 相同或目标套餐相同而被一并清除。取消换套餐安排沿用 CancelPlanChange
// 公开入口，且只是撤掉待生效安排，绝不能被当成取消订阅（ScheduledEnd
// 不出现、Subscribed 保持 true）。
//
// 场景固定为两个没有欠费、订阅均已生效的账户 u-cancel 与 u-keep：
//   - 旧套餐 plan-old：月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%；
//   - 新套餐 plan-new：月费 2000 分、包含 20 单位、超额单价 200 分、税率 6%；
//   - 两账户都自 2026-01-01 00:00 UTC 起使用旧套餐，一月中旬分别安排
//     二月改用新套餐（生效账期 2026-02，安排接受时锁定完整计费条件）。
var (
	crossCancelOldPlan = planDef("plan-old", 1000, 10, 100, 1000)
	crossCancelNewPlan = planDef("plan-new", 2000, 20, 200, 600)
)

const (
	crossCancelAccount = "u-cancel"
	crossKeepAccount   = "u-keep"
)

// newCrossCancelFixture 建立上述两个账户与两份套餐：两账户订阅均已生效，
// 且各自接受了一条二月生效、目标为 plan-new 的换套餐安排。时钟停在
// 2026-01-15（一月中旬，安排均尚未生效），由调用方随后推进。
func newCrossCancelFixture(t *testing.T) (*Service, *fakeClock, PlanChange) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, crossCancelOldPlan)
	mustPlan(t, s, crossCancelNewPlan)
	for _, id := range []string{crossCancelAccount, crossKeepAccount} {
		mustAccount(t, s, id)
		mustSubscribe(t, s, id, crossCancelOldPlan.ID, utc(2026, 1, 1, 0, 0))
	}

	wantChange := PlanChange{
		TargetPlanID:    crossCancelNewPlan.ID,
		Terms:           termsOf(crossCancelNewPlan),
		EffectivePeriod: feb(2026),
	}
	for _, id := range []string{crossCancelAccount, crossKeepAccount} {
		r := mustSchedule(t, s, id, crossCancelNewPlan.ID)
		if !r.Created || r.Change != wantChange {
			t.Fatalf("schedule for %s = %+v, want created %+v", id, r, wantChange)
		}
	}
	return s, clk, wantChange
}

// assertKeepAccountPending 断言保留安排账户的待生效安排原样存在：
// 目标套餐、安排接受时锁定的完整计费条件与二月生效账期都不被其他账户的
// 取消操作改写。
func assertKeepAccountPending(t *testing.T, st AccountStatus, wantChange PlanChange) {
	t.Helper()
	if !st.Subscribed {
		t.Fatalf("%s Subscribed = false, cancel plan change must not cancel subscription: %+v",
			crossKeepAccount, st)
	}
	if st.ScheduledEnd != nil {
		t.Fatalf("%s ScheduledEnd = %v, want nil: cancel plan change is not cancel subscription",
			crossKeepAccount, *st.ScheduledEnd)
	}
	if st.CurrentTerms != termsOf(crossCancelOldPlan) {
		t.Fatalf("%s current terms = %+v, want old plan %+v",
			crossKeepAccount, st.CurrentTerms, termsOf(crossCancelOldPlan))
	}
	if st.PendingChange == nil || *st.PendingChange != wantChange {
		got := "<nil>"
		if st.PendingChange != nil {
			got = formatChange(*st.PendingChange)
		}
		t.Fatalf("%s pending = %s, want %s", crossKeepAccount, got, formatChange(wantChange))
	}
}

// formatChange 以稳定格式输出一条换套餐安排的完整内容，供失败信息展示。
func formatChange(c PlanChange) string {
	return "{target=" + c.TargetPlanID +
		" fee=" + itoa(c.Terms.MonthlyFee) +
		" included=" + itoa(c.Terms.IncludedUnits) +
		" overagePrice=" + itoa(c.Terms.OveragePrice) +
		" taxBps=" + itoa(c.Terms.TaxRateBasisPoints) +
		" effective=" + c.EffectivePeriod.String() + "}"
}

// itoa 避免为失败信息再引入 strconv。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestCancelPlanChangeOnlyClearsRequestedAccount 锁定核心行为：两个当前套餐
// 与目标套餐都相同的账户各自安排二月换新套餐后，只取消其中一个账户的安排，
// 取消账户继续旧套餐、无待生效安排、订阅仍有效；另一账户的安排（目标套餐、
// 二月生效账期、接受时锁定的完整计费条件）原样保留。生效前对已无安排的账户
// 重复取消仍成功，且不改变另一账户。到达二月月初零点后无需上报用量或出账，
// 直接查询状态即可看出两账户差异；二月各 25 单位用量的账单必须分别按各自
// 二月状态采用旧/新套餐条件，账户归属与用量都不能串到另一账户。
func TestCancelPlanChangeOnlyClearsRequestedAccount(t *testing.T) {
	s, clk, wantChange := newCrossCancelFixture(t)
	oldTerms := termsOf(crossCancelOldPlan)
	newTerms := termsOf(crossCancelNewPlan)

	// 只为 u-cancel 取消换套餐安排：成功且无返回错误。
	if err := s.CancelPlanChange(crossCancelAccount); err != nil {
		t.Fatalf("cancel plan change for %s: %v", crossCancelAccount, err)
	}

	// 取消账户：订阅继续有效，继续旧套餐，没有待生效安排，也没有被登记
	// 订阅终止时刻——取消换套餐安排不是取消订阅。
	stCancel, err := s.Status(crossCancelAccount)
	if err != nil {
		t.Fatal(err)
	}
	if !stCancel.Subscribed {
		t.Fatalf("%s Subscribed = false after cancel plan change: %+v", crossCancelAccount, stCancel)
	}
	if stCancel.ScheduledEnd != nil {
		t.Fatalf("%s ScheduledEnd = %v, want nil", crossCancelAccount, *stCancel.ScheduledEnd)
	}
	if stCancel.CurrentTerms != oldTerms {
		t.Fatalf("%s current terms = %+v, want old plan %+v",
			crossCancelAccount, stCancel.CurrentTerms, oldTerms)
	}
	if stCancel.PendingChange != nil {
		t.Fatalf("%s pending = %+v, want nil", crossCancelAccount, *stCancel.PendingChange)
	}

	// 保留账户：安排（目标套餐、完整锁定条件、二月账期）原样保留，
	// 不能因为当前套餐或目标套餐相同而被一并清除。
	stKeep, err := s.Status(crossKeepAccount)
	if err != nil {
		t.Fatal(err)
	}
	assertKeepAccountPending(t, stKeep, wantChange)

	// 生效前对已经没有安排的账户重复取消：仍然成功，且另一账户的安排不变。
	clk.t = utc(2026, 1, 20, 9, 0)
	if err := s.CancelPlanChange(crossCancelAccount); err != nil {
		t.Fatalf("repeat cancel plan change without pending: %v", err)
	}
	stKeep, err = s.Status(crossKeepAccount)
	if err != nil {
		t.Fatal(err)
	}
	assertKeepAccountPending(t, stKeep, wantChange)
	stCancel, _ = s.Status(crossCancelAccount)
	if !stCancel.Subscribed || stCancel.CurrentTerms != oldTerms || stCancel.PendingChange != nil {
		t.Fatalf("%s changed after repeat cancel: %+v", crossCancelAccount, stCancel)
	}

	// 恰好到达二月月初零点：不先上报用量、不先出账，直接查询状态即见区别。
	clk.t = utc(2026, 2, 1, 0, 0)
	stCancel, _ = s.Status(crossCancelAccount)
	if !stCancel.Subscribed || stCancel.CurrentTerms != oldTerms ||
		stCancel.PendingChange != nil || stCancel.ScheduledEnd != nil {
		t.Fatalf("%s feb status = %+v, want subscribed on old plan with no pending/end",
			crossCancelAccount, stCancel)
	}
	stKeep, _ = s.Status(crossKeepAccount)
	if !stKeep.Subscribed || stKeep.CurrentTerms != newTerms ||
		stKeep.PendingChange != nil || stKeep.ScheduledEnd != nil {
		t.Fatalf("%s feb status = %+v, want subscribed on new plan with pending cleared and no end",
			crossKeepAccount, stKeep)
	}

	// 二月两账户各累计 25 单位。事件标识在账户内去重，两账户复用同一标识
	// 也必须各自独立累计。
	clk.t = utc(2026, 2, 10, 12, 0)
	for _, id := range []string{crossCancelAccount, crossKeepAccount} {
		if r, err := s.RecordEvent(Event{
			AccountID: id, EventID: "e-feb-25",
			At: utc(2026, 2, 10, 0, 0), Quantity: 25,
		}); err != nil || !r.Accepted || r.Period != feb(2026) {
			t.Fatalf("feb event for %s: %+v %v", id, r, err)
		}
	}
	if u, _ := s.MonthlyUsage(crossCancelAccount, feb(2026)); u.Total != 25 {
		t.Fatalf("%s feb usage = %d, want 25", crossCancelAccount, u.Total)
	}
	if u, _ := s.MonthlyUsage(crossKeepAccount, feb(2026)); u.Total != 25 {
		t.Fatalf("%s feb usage = %d, want 25", crossKeepAccount, u.Total)
	}

	// 账期结束（三月月初）后各自出账：账户归属、套餐条件、用量与金额必须
	// 与各自二月状态相符，不能串到另一账户。
	clk.t = utc(2026, 3, 1, 0, 0)
	cancelBill := mustBill(t, s, crossCancelAccount, feb(2026))
	if cancelBill.AccountID != crossCancelAccount {
		t.Fatalf("bill account = %q, want %q", cancelBill.AccountID, crossCancelAccount)
	}
	// 取消安排账户二月仍按旧套餐：超额 15、超额费 1500、税 250、应付 2750。
	if cancelBill.Terms != oldTerms ||
		cancelBill.TotalUsage != 25 || cancelBill.IncludedUnits != 10 ||
		cancelBill.OverageUnits != 15 || cancelBill.MonthlyFee != 1000 ||
		cancelBill.OverageFee != 1500 || cancelBill.Tax != 250 ||
		cancelBill.TotalDue != 2750 {
		t.Fatalf("%s feb bill = %+v, want old terms: usage 25 overage 15 fee 1000 overFee 1500 tax 250 due 2750",
			crossCancelAccount, cancelBill)
	}

	keepBill := mustBill(t, s, crossKeepAccount, feb(2026))
	if keepBill.AccountID != crossKeepAccount {
		t.Fatalf("bill account = %q, want %q", keepBill.AccountID, crossKeepAccount)
	}
	// 保留安排账户二月按已生效新套餐：超额 5、超额费 1000、税 180、应付 3180。
	if keepBill.Terms != newTerms ||
		keepBill.TotalUsage != 25 || keepBill.IncludedUnits != 20 ||
		keepBill.OverageUnits != 5 || keepBill.MonthlyFee != 2000 ||
		keepBill.OverageFee != 1000 || keepBill.Tax != 180 ||
		keepBill.TotalDue != 3180 {
		t.Fatalf("%s feb bill = %+v, want new terms: usage 25 overage 5 fee 2000 overFee 1000 tax 180 due 3180",
			crossKeepAccount, keepBill)
	}

	// 两张账单独立保存，互不改写。
	gotCancel, _ := s.GetBill(crossCancelAccount, feb(2026))
	gotKeep, _ := s.GetBill(crossKeepAccount, feb(2026))
	if gotCancel.AccountID != crossCancelAccount || gotCancel.TotalDue != 2750 ||
		gotCancel.Terms.PlanID != crossCancelOldPlan.ID {
		t.Fatalf("stored cancel bill changed: %+v", gotCancel)
	}
	if gotKeep.AccountID != crossKeepAccount || gotKeep.TotalDue != 3180 ||
		gotKeep.Terms.PlanID != crossCancelNewPlan.ID {
		t.Fatalf("stored keep bill changed: %+v", gotKeep)
	}

	// 两个账户的订阅都继续有效：取消换套餐安排始终不是取消订阅。
	stCancel, _ = s.Status(crossCancelAccount)
	stKeep, _ = s.Status(crossKeepAccount)
	if !stCancel.Subscribed || stCancel.ScheduledEnd != nil {
		t.Fatalf("%s not still subscribed after billing: %+v", crossCancelAccount, stCancel)
	}
	if !stKeep.Subscribed || stKeep.ScheduledEnd != nil {
		t.Fatalf("%s not still subscribed after billing: %+v", crossKeepAccount, stKeep)
	}
}

// TestCancelPlanChangeAtEffectiveInstantDoesNotRevert 锁定取消操作的时间边界：
// 在二月生效瞬间的前一纳秒取消，安排确实被撤掉、二月沿用旧套餐；恰好到达
// 二月月初零点（安排已生效）后再取消也成功，但不能把已经生效的新套餐退回
// 旧套餐。后一种情况下，状态与二月账单都按新套餐显示和计费。
func TestCancelPlanChangeAtEffectiveInstantDoesNotRevert(t *testing.T) {
	s, clk, _ := newCrossCancelFixture(t)
	oldTerms := termsOf(crossCancelOldPlan)
	newTerms := termsOf(crossCancelNewPlan)

	effective := utc(2026, 2, 1, 0, 0)
	// 生效前最后一纳秒取消 u-cancel 的安排：此刻安排仍属待生效，取消生效，
	// u-keep 的安排不受影响。
	clk.t = effective.Add(-time.Nanosecond)
	if err := s.CancelPlanChange(crossCancelAccount); err != nil {
		t.Fatalf("cancel one ns before effective: %v", err)
	}
	stCancel, _ := s.Status(crossCancelAccount)
	if stCancel.CurrentTerms != oldTerms || stCancel.PendingChange != nil {
		t.Fatalf("%s one ns before effective = %+v, want old plan no pending",
			crossCancelAccount, stCancel)
	}
	stKeep, _ := s.Status(crossKeepAccount)
	if stKeep.PendingChange == nil || stKeep.PendingChange.TargetPlanID != crossCancelNewPlan.ID {
		t.Fatalf("%s pending lost by other account's pre-boundary cancel: %+v",
			crossKeepAccount, stKeep)
	}

	// 恰好到达二月生效瞬间：u-cancel 已是旧套餐、u-keep 的安排已生效为新套餐。
	clk.t = effective
	stCancel, _ = s.Status(crossCancelAccount)
	if !stCancel.Subscribed || stCancel.CurrentTerms != oldTerms || stCancel.PendingChange != nil {
		t.Fatalf("%s at effective instant = %+v, want old plan no pending",
			crossCancelAccount, stCancel)
	}
	stKeep, _ = s.Status(crossKeepAccount)
	if !stKeep.Subscribed || stKeep.CurrentTerms != newTerms || stKeep.PendingChange != nil {
		t.Fatalf("%s at effective instant = %+v, want new plan with pending cleared",
			crossKeepAccount, stKeep)
	}

	// 生效后再取消保留账户的换套餐安排：请求成功，但已生效的新套餐不能被
	// 退回旧套餐；对取消账户的重复取消同样成功、仍停留旧套餐。
	if err := s.CancelPlanChange(crossKeepAccount); err != nil {
		t.Fatalf("cancel plan change after effective instant: %v", err)
	}
	if err := s.CancelPlanChange(crossCancelAccount); err != nil {
		t.Fatalf("repeat cancel at effective instant: %v", err)
	}
	stKeep, _ = s.Status(crossKeepAccount)
	if !stKeep.Subscribed || stKeep.CurrentTerms != newTerms ||
		stKeep.PendingChange != nil || stKeep.ScheduledEnd != nil {
		t.Fatalf("%s reverted after post-effective cancel: %+v, want new plan still active",
			crossKeepAccount, stKeep)
	}
	stCancel, _ = s.Status(crossCancelAccount)
	if !stCancel.Subscribed || stCancel.CurrentTerms != oldTerms || stCancel.ScheduledEnd != nil {
		t.Fatalf("%s changed after post-effective cancel: %+v, want old plan still active",
			crossCancelAccount, stCancel)
	}

	// 二月各累计 25 单位；账期结束后出账：边界两侧的账户分别按旧、新套餐计费，
	// 生效后取消不影响已生效条件。
	clk.t = utc(2026, 2, 10, 12, 0)
	for _, id := range []string{crossCancelAccount, crossKeepAccount} {
		if _, err := s.RecordEvent(Event{
			AccountID: id, EventID: "e-feb-25",
			At: utc(2026, 2, 10, 0, 0), Quantity: 25,
		}); err != nil {
			t.Fatalf("feb event for %s: %v", id, err)
		}
	}
	clk.t = utc(2026, 3, 1, 0, 0)
	cancelBill := mustBill(t, s, crossCancelAccount, feb(2026))
	if cancelBill.Terms != oldTerms || cancelBill.OverageUnits != 15 ||
		cancelBill.OverageFee != 1500 || cancelBill.Tax != 250 || cancelBill.TotalDue != 2750 {
		t.Fatalf("%s feb bill = %+v, want old terms due 2750", crossCancelAccount, cancelBill)
	}
	keepBill := mustBill(t, s, crossKeepAccount, feb(2026))
	if keepBill.Terms != newTerms || keepBill.OverageUnits != 5 ||
		keepBill.OverageFee != 1000 || keepBill.Tax != 180 || keepBill.TotalDue != 3180 {
		t.Fatalf("%s feb bill = %+v, want new terms due 3180", crossKeepAccount, keepBill)
	}
}
