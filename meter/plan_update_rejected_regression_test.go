package meter

import (
	"errors"
	"testing"
)

// 本文件回归“修改套餐定义被拒绝（ErrInvalidArgument）后”的定价保护：
// 已经开通的订阅、已经接受的换套餐安排各自保存的条件快照，以及之后新开通
// 订阅所读取的套餐当前定义，都必须保持修改前的原样——不能只凭请求返回了
// 错误就认为已有价格没有受到影响。
//
// 场景（全部 UTC）：
//
//	套餐 p 原定义：月费 2000、包含 20、超额单价 200、税率 600（6%）。
//	账户 active 自 2026-01-01 起生效使用 p；账户 changer 使用另一套餐
//	other，并在一月中旬登记自 2026-02 起换到 p（安排被接受时锁定 p 的
//	原定义）。随后两次非法修改 p：一次把月费改成负数，一次把税率改成
//	10001；两次请求的其余计费字段都给出与原定义不同的合法值。每次请求
//	都必须返回可识别的 ErrInvalidArgument，且拒绝之后：
//	  - p 的当前定义保持原样；
//	  - active 的当前条件仍是原来的完整四项；
//	  - changer 的待换套餐安排仍保留原目标、原生效月份与原先锁定的
//	    四项条件，失败请求中的任何数值都不能混入。
//
//	在失败修改之后才开通 p 的账户 newcomer，保存的条件必须与修改前的
//	定义完全一致：这是对“套餐仍可按原价格供新订阅使用”的保障，不能用
//	老订阅快照没变来代替。newcomer 在一月二十日（月中）开通，当月接收
//	25 单位用量，预计费用为超额 5 单位、超额费 1000、税额 180、总额
//	3180；该月结束后没有新增用量的正式账单保持相同的用量和金额（月中
//	开通仍收完整月费、给完整额度）。
//
//	changer 的安排到达 2026-02-01 00:00 后，直接查询即显示其锁定的
//	原条件，待生效安排消失。合法修改仍然只影响之后新保存的条件：已开通
//	订阅与已接受安排的快照不被改写，之后新开通的订阅才按新定义取价——
//	套餐定义与账户快照之间的这一区别不因为这些回归场景而改变。

// invalidPlanUpdates 返回两次对 p 的非法修改：一次月费为负、一次税率
// 越界（10001）；两次请求的其余计费字段都是与原定义不同的合法值，
// 用于检验失败请求中的任何数值都不会混入已保存的条件。
func invalidPlanUpdates() []Plan {
	return []Plan{
		{ID: "p", MonthlyFee: -1, IncludedUnits: 99, OveragePrice: 999, TaxRateBasisPoints: 900},
		{ID: "p", MonthlyFee: 5000, IncludedUnits: 50, OveragePrice: 500, TaxRateBasisPoints: 10001},
	}
}

// newRejectedUpdateService 在 2026-01-15 12:00 UTC 构造服务：套餐 p 按
// 原定义（2000/20/200/600）存在，另一套餐 other 存在；账户 active 自
// 2026-01-01 起生效使用 p，账户 changer 自同日起使用 other 并已登记
// 自 2026-02 起换到 p。返回服务、时钟与 changer 被接受时锁定的安排。
func newRejectedUpdateService(t *testing.T) (*Service, *fakeClock, PlanChange) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("p", 2000, 20, 200, 600))
	mustPlan(t, s, planDef("other", 1000, 10, 100, 0))
	mustAccount(t, s, "active")
	mustSubscribe(t, s, "active", "p", utc(2026, 1, 1, 0, 0))
	mustAccount(t, s, "changer")
	mustSubscribe(t, s, "changer", "other", utc(2026, 1, 1, 0, 0))
	r := mustSchedule(t, s, "changer", "p")
	wantLocked := PlanChange{
		TargetPlanID:    "p",
		Terms:           termsOf(planDef("p", 2000, 20, 200, 600)),
		EffectivePeriod: feb(2026),
	}
	if !r.Created || r.Change != wantLocked {
		t.Fatalf("schedule changer->p: created=%v change=%+v, want %+v", r.Created, r.Change, wantLocked)
	}
	return s, clk, r.Change
}

// assertRejectedUpdateKeepsOriginalTerms 提交一次非法修改并断言：请求返回
// 可识别的 ErrInvalidArgument；拒绝之后套餐当前定义、active 的当前条件与
// changer 的待生效安排（原目标、原生效月份、原先锁定的四项条件）都保持
// 修改前的原样。
func assertRejectedUpdateKeepsOriginalTerms(t *testing.T, s *Service, bad Plan, locked PlanChange) {
	t.Helper()
	origTerms := termsOf(planDef("p", 2000, 20, 200, 600))

	if err := s.UpdatePlan(bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("update plan %+v: want ErrInvalidArgument, got %v", bad, err)
	}
	// 套餐当前定义不被失败请求改写。
	if got := s.Status2("p"); got != planDef("p", 2000, 20, 200, 600) {
		t.Fatalf("plan definition after rejected update %+v = %+v, want original", bad, got)
	}
	// 已开通订阅仍显示原来的完整条件。
	st, err := s.Status("active")
	if err != nil {
		t.Fatalf("status active: %v", err)
	}
	if !st.Subscribed || st.CurrentTerms != origTerms {
		t.Fatalf("active terms after rejected update %+v = %+v (subscribed=%v), want %+v",
			bad, st.CurrentTerms, st.Subscribed, origTerms)
	}
	// 待换套餐安排保留原目标、原生效月份与原先锁定的四项条件，
	// 失败请求中的任何数值都不能混入。
	st, err = s.Status("changer")
	if err != nil {
		t.Fatalf("status changer: %v", err)
	}
	if st.PendingChange == nil {
		t.Fatalf("pending change lost after rejected update %+v", bad)
	}
	if *st.PendingChange != locked {
		t.Fatalf("pending change after rejected update %+v = %+v, want %+v", bad, *st.PendingChange, locked)
	}
	if st.PendingChange.TargetPlanID != "p" || st.PendingChange.EffectivePeriod != feb(2026) {
		t.Fatalf("pending target/effective changed: %+v", st.PendingChange)
	}
	if st.PendingChange.Terms != origTerms {
		t.Fatalf("pending locked terms = %+v, want %+v", st.PendingChange.Terms, origTerms)
	}
}

// TestRejectedPlanUpdateKeepsDefinitionAndSavedSnapshots 覆盖两类非法修改
// （月费为负、税率 10001）：每次都被拒绝为 ErrInvalidArgument，且拒绝之后
// 正在使用 p 的账户仍显示原来的完整条件，已接受换套餐安排的账户仍保留
// 原目标、原生效月份与原先锁定的四项条件；安排到达约定的 UTC 月初后，
// 直接查询即显示锁定的原条件，待生效安排消失。
func TestRejectedPlanUpdateKeepsDefinitionAndSavedSnapshots(t *testing.T) {
	s, clk, locked := newRejectedUpdateService(t)
	origTerms := termsOf(planDef("p", 2000, 20, 200, 600))

	for _, bad := range invalidPlanUpdates() {
		assertRejectedUpdateKeepsOriginalTerms(t, s, bad, locked)
	}

	// 到达约定的 UTC 月初（2026-02-01 00:00）：changer 直接查询即显示
	// 锁定的原条件，待生效安排消失；active 的当前条件也仍是原定义。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, err := s.Status("changer")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != origTerms || st.PendingChange != nil {
		t.Fatalf("changer at 2026-02-01: current=%+v pending=%+v, want original terms and no pending",
			st.CurrentTerms, st.PendingChange)
	}
	st, err = s.Status("active")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.CurrentTerms != origTerms {
		t.Fatalf("active at 2026-02-01: current=%+v (subscribed=%v), want %+v",
			st.CurrentTerms, st.Subscribed, origTerms)
	}
}

// TestSubscribeAfterRejectedPlanUpdateGetsOriginalTerms 覆盖失败修改之后才
// 开通 p 的账户：它保存的条件与修改前的定义完全一致（套餐仍可按原价格供
// 新订阅使用，不能用老订阅快照没变来代替）。开通当月接收 25 单位用量时，
// 预计费用为超额 5 单位、超额费 1000、税额 180、总额 3180；该月结束后
// 没有新增用量的正式账单保持相同的用量和金额，月中开通也沿用完整月费与
// 完整额度规则。
func TestSubscribeAfterRejectedPlanUpdateGetsOriginalTerms(t *testing.T) {
	s, clk, locked := newRejectedUpdateService(t)
	origTerms := termsOf(planDef("p", 2000, 20, 200, 600))

	for _, bad := range invalidPlanUpdates() {
		assertRejectedUpdateKeepsOriginalTerms(t, s, bad, locked)
	}

	// 失败修改之后才开通 p 的账户（一月二十日，月中开通）。
	clk.t = utc(2026, 1, 20, 12, 0)
	mustAccount(t, s, "newcomer")
	mustSubscribe(t, s, "newcomer", "p", utc(2026, 1, 20, 12, 0))
	st, err := s.Status("newcomer")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.CurrentTerms != origTerms {
		t.Fatalf("newcomer terms = %+v (subscribed=%v), want %+v", st.CurrentTerms, st.Subscribed, origTerms)
	}

	// 开通当月接收 25 单位用量：超额 5、超额费 1000、税 180、总额 3180；
	// 月中开通仍收完整月费 2000、给完整额度 20。
	if _, err := s.RecordEvent(Event{
		AccountID: "newcomer", EventID: "e-jan",
		At: utc(2026, 1, 20, 12, 0), Quantity: 25,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	est, err := s.EstimateCurrentBill("newcomer")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !est.Estimated || est.Period != jan(2026) || est.Terms != origTerms {
		t.Fatalf("estimate header: %+v", est)
	}
	if est.TotalUsage != 25 || est.IncludedUnits != 20 || est.OverageUnits != 5 ||
		est.MonthlyFee != 2000 || est.OverageFee != 1000 || est.Tax != 180 || est.EstimatedTotalDue != 3180 {
		t.Fatalf("estimate amounts = usage %d included %d overage %d fee %d overageFee %d tax %d total %d, "+
			"want 25/20/5/2000/1000/180/3180",
			est.TotalUsage, est.IncludedUnits, est.OverageUnits,
			est.MonthlyFee, est.OverageFee, est.Tax, est.EstimatedTotalDue)
	}

	// 该月结束后没有新增用量：正式账单保持相同的用量和金额。
	clk.t = utc(2026, 2, 1, 0, 0)
	bill := mustBill(t, s, "newcomer", jan(2026))
	if bill.Terms != origTerms || bill.TotalUsage != 25 || bill.IncludedUnits != 20 ||
		bill.OverageUnits != 5 || bill.MonthlyFee != 2000 || bill.OverageFee != 1000 ||
		bill.Tax != 180 || bill.TotalDue != 3180 || bill.Balance != 3180 {
		t.Fatalf("jan bill: %+v", bill)
	}
	if !bill.DueAt.Equal(jan(2026).End().AddDate(0, 0, 7)) {
		t.Fatalf("bill due at = %v, want 2026-02-08T00:00:00Z", bill.DueAt)
	}
}

// TestLegalPlanUpdateAfterRejectionsAffectsOnlyNewSnapshots 固定套餐定义与
// 账户快照之间的区别：非法修改不能影响任何一方，而合法修改只影响之后新
// 保存的条件——已开通订阅与已接受安排（含其生效后）的快照不被改写，
// 之后新开通的订阅才按修改后的定义取得条件。
func TestLegalPlanUpdateAfterRejectionsAffectsOnlyNewSnapshots(t *testing.T) {
	s, clk, locked := newRejectedUpdateService(t)
	origTerms := termsOf(planDef("p", 2000, 20, 200, 600))

	for _, bad := range invalidPlanUpdates() {
		assertRejectedUpdateKeepsOriginalTerms(t, s, bad, locked)
	}

	// 合法修改被接受：套餐当前定义更新，但只影响之后新保存的条件。
	updated := planDef("p", 3000, 30, 300, 800)
	if err := s.UpdatePlan(updated); err != nil {
		t.Fatalf("legal update: %v", err)
	}
	if got := s.Status2("p"); got != updated {
		t.Fatalf("plan definition = %+v, want %+v", got, updated)
	}
	// 已开通订阅与已接受安排的快照不被改写。
	st, err := s.Status("active")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != origTerms {
		t.Fatalf("active terms rewritten by legal update: %+v, want %+v", st.CurrentTerms, origTerms)
	}
	st, err = s.Status("changer")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingChange == nil || *st.PendingChange != locked {
		t.Fatalf("pending change rewritten by legal update: %+v, want %+v", st.PendingChange, locked)
	}

	// 之后新开通的订阅按修改后的定义取得条件。
	mustAccount(t, s, "late")
	mustSubscribe(t, s, "late", "p", utc(2026, 1, 15, 12, 0))
	st, err = s.Status("late")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.CurrentTerms != termsOf(updated) {
		t.Fatalf("late terms = %+v (subscribed=%v), want updated %+v", st.CurrentTerms, st.Subscribed, termsOf(updated))
	}

	// changer 的安排到达 2026-02-01 00:00 后仍按锁定的原条件生效，
	// 合法修改不影响已接受安排；待生效安排消失。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, err = s.Status("changer")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != origTerms || st.PendingChange != nil {
		t.Fatalf("changer at 2026-02-01: current=%+v pending=%+v, want locked original terms and no pending",
			st.CurrentTerms, st.PendingChange)
	}
}
