package meter

import (
	"errors"
	"testing"
)

// 本文件回归“套餐定义的非法修改被拒绝后的定价保护”：一次非法 UpdatePlan
// 返回 ErrInvalidArgument 之后，三类使用者都必须仍按各自原有的计费条件计费，
// 不能只凭请求返回了错误就认为已有价格没有受到影响。
//
// 受保护的套餐 t（原定义）：
//
//	月费 2000 分、包含 20 单位、超额单价 200 分、税率 600（6%）。
//
// 场景（全部 UTC）：
//
//   - 账户 active 已生效并使用 t（2026-01-01 00:00 开通），持有开通快照；
//   - 账户 switcher 使用另一套餐 o（月费 1000、包含 10、超额 100、税率 1000），
//     于 2026-01-15 12:00 登记二月换到 t，安排锁定 t 当时的完整四项条件；
//   - 两类非法修改分别覆盖：把月费改成负数、把税率改成 10001；其余计费字段
//     同时给出与原定义不同的合法值，防止“非法字段被拒、合法字段却已落库”的
//     部分写入；
//   - 拒绝之后才开通 t 的新账户保存的也必须是修改前的原定义，开通当月接收
//     25 单位用量的预计费用与正式账单一致：超额 5 单位、超额费 1000 分、
//     税额 180 分、总额 3180 分；
//   - 已接受的换套餐安排到达约定的 UTC 月初（2026-02-01 00:00）后，直接查询
//     即显示其锁定的原条件，待生效安排消失。
//
// 实现上 UpdatePlan 在同一把互斥锁的临界区内先 validatePlan、通过后才整体
// 替换 s.plans 中的定义（见 service.go 的 UpdatePlan）：校验失败时根本不触碰
// 套餐当前定义，已保存的开通快照与安排锁定快照更是各自独立的副本。本文件把
// “拒绝即完全无副作用”这一保证固定下来。合法修改只影响之后新保存的条件这一
// 既有区别也在末尾顺带锁定，防止为这些场景改动公开行为。

// planProtected 是被保护套餐 t 的原定义。
var planProtected = planDef("t", 2000, 20, 200, 600)

// planOther 是换套餐账户当前使用的另一套餐。
var planOther = planDef("o", 1000, 10, 100, 1000)

// newRejectedUpdateService 在 2026-01-15 12:00 UTC 构造：
//   - 套餐 t（原定义）与 o；
//   - 账户 active：当月一日起已生效使用 t；
//   - 账户 switcher：当月一日起使用 o，并已登记二月换到 t。
//
// 返回服务、时钟以及 switcher 已接受安排应有的完整内容（锁定 t 原定义、
// 二月生效）。
func newRejectedUpdateService(t *testing.T) (*Service, *fakeClock, PlanChange) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planProtected)
	mustPlan(t, s, planOther)
	mustAccount(t, s, "active")
	mustAccount(t, s, "switcher")
	mustSubscribe(t, s, "active", "t", utc(2026, 1, 1, 0, 0))
	mustSubscribe(t, s, "switcher", "o", utc(2026, 1, 1, 0, 0))

	r := mustSchedule(t, s, "switcher", "t")
	locked := PlanChange{
		TargetPlanID:    "t",
		Terms:           termsOf(planProtected),
		EffectivePeriod: feb(2026),
	}
	if !r.Created || r.Change != locked {
		t.Fatalf("setup: schedule = %+v, want created %+v", r, locked)
	}
	return s, clk, locked
}

// assertPlanUntouched 确认套餐 t 的当前定义仍是原定义，失败请求中的任何
// 数值都没有落库。
func assertPlanUntouched(t *testing.T, s *Service) {
	t.Helper()
	if got := s.Status2("t"); got != planProtected {
		t.Fatalf("plan current definition = %+v, want untouched %+v", got, planProtected)
	}
}

// assertActiveSubscriberKeepsOriginalTerms 确认正在使用 t 的账户仍显示原来的
// 完整条件，且订阅持续生效。
func assertActiveSubscriberKeepsOriginalTerms(t *testing.T, s *Service) {
	t.Helper()
	st, err := s.Status("active")
	if err != nil {
		t.Fatalf("status active: %v", err)
	}
	want := termsOf(planProtected)
	if !st.Subscribed {
		t.Fatalf("active subscription no longer shown as subscribed: %+v", st)
	}
	if st.CurrentTerms != want {
		t.Fatalf("active current terms = %+v, want original %+v", st.CurrentTerms, want)
	}
}

// assertPendingChangeUntouched 确认 switcher 的待换套餐安排仍保留原目标、
// 原生效月份与安排时锁定的四项条件，失败请求中的任何数值都没有混入。
func assertPendingChangeUntouched(t *testing.T, s *Service, locked PlanChange, rejected Plan) {
	t.Helper()
	st, err := s.Status("switcher")
	if err != nil {
		t.Fatalf("status switcher: %v", err)
	}
	if !st.Subscribed || st.CurrentTerms != termsOf(planOther) {
		t.Fatalf("switcher current terms = %+v, want still on other plan %+v",
			st.CurrentTerms, termsOf(planOther))
	}
	if st.PendingChange == nil {
		t.Fatalf("pending change disappeared after rejected update")
	}
	got := *st.PendingChange
	if got != locked {
		t.Fatalf("pending change = %+v, want locked %+v", got, locked)
	}
	// 逐项对照被拒请求，使任何字段级的部分写入在失败信息中直接暴露。
	if got.TargetPlanID != "t" {
		t.Fatalf("pending target = %q, want t", got.TargetPlanID)
	}
	if got.EffectivePeriod != feb(2026) {
		t.Fatalf("pending effective period = %s, want 2026-02", got.EffectivePeriod)
	}
	if got.Terms.MonthlyFee == rejected.MonthlyFee && rejected.MonthlyFee != planProtected.MonthlyFee {
		t.Fatalf("pending monthly fee %d leaked from rejected request", got.Terms.MonthlyFee)
	}
	if got.Terms.IncludedUnits == rejected.IncludedUnits {
		t.Fatalf("pending included units %d leaked from rejected request", got.Terms.IncludedUnits)
	}
	if got.Terms.OveragePrice == rejected.OveragePrice {
		t.Fatalf("pending overage price %d leaked from rejected request", got.Terms.OveragePrice)
	}
	if got.Terms.TaxRateBasisPoints == rejected.TaxRateBasisPoints {
		t.Fatalf("pending tax rate %d leaked from rejected request", got.Terms.TaxRateBasisPoints)
	}
}

// TestRejectedPlanUpdateKeepsPlanDefinitionAndSnapshots 分别对两类非法修改
// 锁定：请求返回可识别的 ErrInvalidArgument；套餐当前定义、正在使用该套餐
// 的账户快照、另一账户待生效安排锁定的目标/月份/四项条件全部原样保留，
// 失败请求中的任何数值都不得混入；再次安排同一目标仍返回原安排，不重新取价。
func TestRejectedPlanUpdateKeepsPlanDefinitionAndSnapshots(t *testing.T) {
	cases := []struct {
		name string
		bad  Plan
	}{
		{
			name: "negative-monthly-fee",
			// 月费非法（负数），其余三项同时给出与原定义不同的合法值。
			bad: planDef("t", -1, 25, 300, 700),
		},
		{
			name: "tax-rate-above-10000",
			// 税率非法（10001 越出 [0,10000]），其余三项同时给出不同的合法值。
			bad: planDef("t", 3000, 25, 300, 10001),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, locked := newRejectedUpdateService(t)

			err := s.UpdatePlan(tc.bad)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("update with %+v: err = %v, want ErrInvalidArgument", tc.bad, err)
			}

			assertPlanUntouched(t, s)
			assertActiveSubscriberKeepsOriginalTerms(t, s)
			assertPendingChangeUntouched(t, s, locked, tc.bad)

			// 再次安排同一目标：必须原样返回此前锁定的安排（Created 为 false），
			// 不按失败请求中的数值重新取价。
			r, err := s.SchedulePlanChange("switcher", "t")
			if err != nil {
				t.Fatalf("re-schedule same target: %v", err)
			}
			if r.Created || r.Change != locked {
				t.Fatalf("re-schedule = %+v, want original locked change %+v", r.Change, locked)
			}
			assertPendingChangeUntouched(t, s, locked, tc.bad)
		})
	}
}

// TestRejectedPlanUpdateDoesNotChangeFutureSubscriptionPricing 锁定“套餐仍可
// 按原价格供新订阅使用”：两类非法修改先后被拒后才开通 t 的新账户，保存的
// 条件必须与修改前的定义完全一致——不能仅用老订阅快照没变来代替这一保障。
// 该账户月中开通（2026-01-20），开通当月接收 25 单位用量：
//
//	超额 5 单位、超额费 5×200=1000 分、税 (2000+1000)×6%=180 分、
//	总额 3180 分；月中开通也沿用完整月费 2000 与完整额度 20。
//
// 该月结束后没有新增用量，正式账单必须保持相同的用量与金额。
func TestRejectedPlanUpdateDoesNotChangeFutureSubscriptionPricing(t *testing.T) {
	s, clk, _ := newRejectedUpdateService(t)

	badAttempts := []Plan{
		planDef("t", -1, 25, 300, 700),
		planDef("t", 3000, 25, 300, 10001),
	}
	for i, bad := range badAttempts {
		if err := s.UpdatePlan(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("bad update #%d %+v: %v", i+1, bad, err)
		}
	}
	assertPlanUntouched(t, s)

	// 失败修改之后才开通 t 的新账户，月中开通。
	mustAccount(t, s, "late")
	clk.t = utc(2026, 1, 20, 9, 0)
	mustSubscribe(t, s, "late", "t", utc(2026, 1, 20, 0, 0))

	wantTerms := termsOf(planProtected)
	st, err := s.Status("late")
	if err != nil {
		t.Fatalf("status late: %v", err)
	}
	if !st.Subscribed || st.CurrentTerms != wantTerms {
		t.Fatalf("late subscriber terms = %+v, want pre-update definition %+v",
			st.CurrentTerms, wantTerms)
	}

	// 开通当月接收 25 单位用量。
	clk.t = utc(2026, 1, 21, 12, 0)
	if _, err := s.RecordEvent(Event{
		AccountID: "late", EventID: "usage-jan",
		At: utc(2026, 1, 21, 0, 0), Quantity: 25,
	}); err != nil {
		t.Fatalf("record jan usage: %v", err)
	}

	// 当月预计费用：超额 5、超额费 1000、税 180、总额 3180；
	// 完整月费 2000、完整额度 20，不按月中开通折算。
	est, err := s.EstimateCurrentBill("late")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !est.Estimated || est.Period != jan(2026) {
		t.Fatalf("estimate identity = %+v, want estimated 2026-01", est)
	}
	if est.Terms != wantTerms {
		t.Fatalf("estimate terms = %+v, want %+v", est.Terms, wantTerms)
	}
	if est.TotalUsage != 25 || est.IncludedUnits != 20 || est.OverageUnits != 5 ||
		est.MonthlyFee != 2000 || est.OverageFee != 1000 || est.Tax != 180 ||
		est.EstimatedTotalDue != 3180 {
		t.Fatalf("estimate amounts = usage %d included %d overageUnits %d fee %d overageFee %d tax %d total %d",
			est.TotalUsage, est.IncludedUnits, est.OverageUnits,
			est.MonthlyFee, est.OverageFee, est.Tax, est.EstimatedTotalDue)
	}

	// 一月结束（2026-02-01 00:00 UTC），没有新增用量，正式出账：
	// 用量与金额与预估完全一致，月中开通仍按完整月费与额度。
	clk.t = utc(2026, 2, 1, 0, 0)
	bill := mustBill(t, s, "late", jan(2026))
	if bill.Terms != wantTerms {
		t.Fatalf("bill terms = %+v, want %+v", bill.Terms, wantTerms)
	}
	if bill.TotalUsage != 25 || bill.IncludedUnits != 20 || bill.OverageUnits != 5 ||
		bill.MonthlyFee != 2000 || bill.OverageFee != 1000 || bill.Tax != 180 ||
		bill.TotalDue != 3180 {
		t.Fatalf("bill amounts = usage %d included %d overageUnits %d fee %d overageFee %d tax %d total %d",
			bill.TotalUsage, bill.IncludedUnits, bill.OverageUnits,
			bill.MonthlyFee, bill.OverageFee, bill.Tax, bill.TotalDue)
	}
	// 出账固定：再次读取得到同一张账单。
	got, err := s.GetBill("late", jan(2026))
	if err != nil {
		t.Fatalf("get bill: %v", err)
	}
	if got != bill {
		t.Fatalf("bill changed between create and get:\ncreate=%+v\nget   =%+v", bill, got)
	}
}

// TestRejectedPlanUpdateThenPlanChangeActivatesWithLockedTerms 锁定：非法修改
// 被拒后，已接受的换套餐安排到达约定的 UTC 月初（2026-02-01 00:00 UTC）时，
// 直接查询（无须先上报用量或出账）即显示安排锁定的原条件，待生效安排消失。
// 随后一次合法修改只影响之后新保存的条件：已生效使用 t 的两个账户继续按原
// 条件，二月之后才开通的新账户按新定义取价，公开行为不被这些回归场景改变。
func TestRejectedPlanUpdateThenPlanChangeActivatesWithLockedTerms(t *testing.T) {
	s, clk, locked := newRejectedUpdateService(t)

	for _, bad := range []Plan{
		planDef("t", -1, 25, 300, 700),
		planDef("t", 3000, 25, 300, 10001),
	} {
		if err := s.UpdatePlan(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("bad update %+v: %v", bad, err)
		}
	}

	// 到达约定的 UTC 月初：直接查询即显示锁定的原条件，待生效安排消失。
	clk.t = locked.EffectivePeriod.Start() // 2026-02-01 00:00 UTC
	if !clk.t.Equal(utc(2026, 2, 1, 0, 0)) {
		t.Fatalf("effective instant = %v, want 2026-02-01T00:00:00Z", clk.t)
	}
	st, err := s.Status("switcher")
	if err != nil {
		t.Fatalf("status switcher at effective month: %v", err)
	}
	wantTerms := termsOf(planProtected)
	if !st.Subscribed {
		t.Fatalf("switcher not subscribed after change activated: %+v", st)
	}
	if st.CurrentTerms != wantTerms {
		t.Fatalf("switcher current terms at activation = %+v, want locked original %+v",
			st.CurrentTerms, wantTerms)
	}
	if st.PendingChange != nil {
		t.Fatalf("pending change still present after effective month: %+v", st.PendingChange)
	}

	// 一次合法修改：套餐当前定义确实更新。
	legal := planDef("t", 5000, 5, 900, 2500)
	if err := s.UpdatePlan(legal); err != nil {
		t.Fatalf("legal update: %v", err)
	}
	if got := s.Status2("t"); got != legal {
		t.Fatalf("plan definition = %+v, want legal %+v", got, legal)
	}

	// 非法修改不能影响任何一方；合法修改也不改写已保存的条件：
	// 切换刚生效的 switcher 与早已开通的 active 都继续按 t 原条件。
	st, _ = s.Status("switcher")
	if st.CurrentTerms != wantTerms || st.PendingChange != nil {
		t.Fatalf("switcher terms rewritten by later legal update: %+v", st.CurrentTerms)
	}
	assertActiveSubscriberKeepsOriginalTerms(t, s)

	// 合法修改只影响之后新保存的条件：二月之后才开通 t 的新账户按新定义取价。
	mustAccount(t, s, "newbie")
	clk.t = utc(2026, 2, 2, 10, 0)
	mustSubscribe(t, s, "newbie", "t", utc(2026, 2, 2, 0, 0))
	nst, err := s.Status("newbie")
	if err != nil {
		t.Fatalf("status newbie: %v", err)
	}
	if !nst.Subscribed || nst.CurrentTerms != termsOf(legal) {
		t.Fatalf("new subscriber terms = %+v, want post-update definition %+v",
			nst.CurrentTerms, termsOf(legal))
	}
}
