package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“换套餐安排的生效账期由接受请求时刻的下一个 UTC 自然月决定”
// 提供时区维度的回归保障：请求时刻的本地钟面月份与 UTC 月份不一致时，
// 生效账期、当前条件、待生效安排与实际切换月份必须全部按 UTC 自然月判定，
// 同一瞬间换任何时区表示都得到相同结果，且不依赖运行当天日期与机器默认时区。
//
// 甲：月费 1000 分、包含 10 单位、超额单价 100 分、税率 0。
// 乙：月费 2000 分、包含 20 单位、超额单价 200 分、税率 0。
var (
	planJia = planDef("a", 1000, 10, 100, 0)
	planYi  = planDef("b", 2000, 20, 200, 0)
)

// newJiaYiService 创建账户 u：自 2026-01-01 起使用甲套餐，无欠费、无取消安排。
// 时钟由调用方随后设定到需要的请求时刻。
func newJiaYiService(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, planJia)
	mustPlan(t, s, planYi)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	return s, clk
}

// TestSchedulePlanChangeWhenLocalDateCrossesUTCMonth 锁定：2026-02-01 07:30（UTC+8）
// 首次安排改用乙时，该瞬间仍是 UTC 一月末（2026-01-31 23:30 UTC），生效账期
// 必须是二月而非三月；即使本地钟面已经进入二月，当前条件仍为甲、待生效条件为乙。
// 同一瞬间以 UTC 或其他时区表示，条件相同的账户必须得到完全相同的结果。
func TestSchedulePlanChangeWhenLocalDateCrossesUTCMonth(t *testing.T) {
	// 同一实际瞬间 2026-01-31 23:30:00 UTC 的四种时区表示。
	// +08:00 与 +05:30 的本地钟面已经进入 2 月 1 日，UTC 与 -05:00 仍在 1 月 31 日。
	instant := utc(2026, 1, 31, 23, 30)
	representations := map[string]time.Time{
		"UTC":      instant,
		"UTC+8":    time.Date(2026, time.February, 1, 7, 30, 0, 0, zonePlus8),
		"UTC-5":    time.Date(2026, time.January, 31, 18, 30, 0, 0, zoneMinus5),
		"UTC+5:30": time.Date(2026, time.February, 1, 5, 0, 0, 0, zonePlus530),
	}
	for name, at := range representations {
		if !at.Equal(instant) {
			t.Fatalf("%s representation %v is not instant %v", name, at, instant)
		}
	}
	// 锁定测试前提：+08 表示的本地日期已是 2 月 1 日，而 UTC 仍是 1 月 31 日——
	// 任何按本地月份决定生效账期的实现都会在此暴露。
	if representations["UTC+8"].Day() != 1 || representations["UTC+8"].Month() != time.February {
		t.Fatalf("plus8 wall clock = %v, want 2026-02-01", representations["UTC+8"])
	}
	if instant.Day() != 31 || instant.Month() != time.January {
		t.Fatalf("utc wall clock = %v, want 2026-01-31", instant)
	}

	wantChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planYi),
		EffectivePeriod: feb(2026),
	}
	wantCurrent := termsOf(planJia)

	var wantResult *PlanChangeResult
	for name, at := range representations {
		s, clk := newJiaYiService(t)
		// 时钟以该时区表示返回同一瞬间；服务内部必须按 UTC 判定月份。
		clk.t = at

		r, err := s.SchedulePlanChange("u", "b")
		if err != nil {
			t.Fatalf("%s: schedule: %v", name, err)
		}
		if !r.Created || r.Change != wantChange {
			t.Fatalf("%s: schedule = %+v, want created change %+v", name, r, wantChange)
		}
		if wantResult == nil {
			got := r
			wantResult = &got
		} else if r != *wantResult {
			t.Fatalf("%s: result %+v differs from UTC result %+v", name, r, *wantResult)
		}

		// 即使本地钟面已经进入二月，当前条件仍为甲、待生效条件为乙。
		st, err := s.Status("u")
		if err != nil {
			t.Fatalf("%s: status: %v", name, err)
		}
		if st.CurrentTerms != wantCurrent {
			t.Fatalf("%s: current = %+v, want %+v", name, st.CurrentTerms, wantCurrent)
		}
		if st.PendingChange == nil || *st.PendingChange != wantChange {
			t.Fatalf("%s: pending = %+v, want %+v", name, st.PendingChange, wantChange)
		}
	}
}

// TestSchedulePlanChangeExactlyAtUTCMonthStart 锁定：2026-01-31 19:00（UTC-5）
// 恰好是 UTC 二月月初零点（2026-02-01 00:00 UTC）。尚无待生效安排的账户
// 此刻首次安排改用乙，新安排必须从三月生效，二月继续使用甲套餐——
// 不能按本地钟面的一月把生效账期定成二月。
func TestSchedulePlanChangeExactlyAtUTCMonthStart(t *testing.T) {
	s, clk := newJiaYiService(t)
	// 本地钟面 2026-01-31 19:00（UTC-5）== 2026-02-01 00:00:00 UTC。
	at := time.Date(2026, time.January, 31, 19, 0, 0, 0, zoneMinus5)
	if !at.UTC().Equal(utc(2026, 2, 1, 0, 0)) {
		t.Fatalf("request instant = %v, want 2026-02-01 00:00 UTC", at.UTC())
	}
	if at.Month() != time.January {
		t.Fatalf("request wall month = %s, want January", at.Month())
	}
	clk.t = at

	r, err := s.SchedulePlanChange("u", "b")
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	wantChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planYi),
		EffectivePeriod: mar(2026),
	}
	if !r.Created || r.Change != wantChange {
		t.Fatalf("schedule = %+v, want created change %+v", r, wantChange)
	}

	// 当前（二月）条件仍为甲，待生效安排为三月生效的乙。
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != termsOf(planJia) {
		t.Fatalf("feb current = %+v, want %+v", st.CurrentTerms, termsOf(planJia))
	}
	if st.PendingChange == nil || *st.PendingChange != wantChange {
		t.Fatalf("pending = %+v, want %+v", st.PendingChange, wantChange)
	}

	// 二月中旬再查：仍是甲，安排仍等待三月生效。
	clk.t = utc(2026, 2, 15, 12, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms != termsOf(planJia) || st.PendingChange == nil || *st.PendingChange != wantChange {
		t.Fatalf("mid-feb: current=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}

	// 三月月初零点：乙生效，待生效安排消失。
	clk.t = utc(2026, 3, 1, 0, 0)
	st, _ = s.Status("u")
	if st.CurrentTerms != termsOf(planYi) || st.PendingChange != nil {
		t.Fatalf("mar: current=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}
}

// TestPlanChangeEffectiveInstantUTCBoundary 锁定生效瞬间的纳秒级边界：
// 对一月末已接受的二月安排，二月月初前最后一纳秒查询仍显示甲和待生效安排；
// 恰到 2026-02-01 00:00 UTC，直接查询即显示乙的完整条件、待生效安排消失，
// 无需先接收用量或出账。同一瞬间用带时区的时间表达也不能提前或延后切换；
// 在该瞬间再次选择乙必须返回 ErrPlanChangeSamePlan，不得新建三月安排。
func TestPlanChangeEffectiveInstantUTCBoundary(t *testing.T) {
	s, clk := newJiaYiService(t)
	// 一月末接受二月生效的安排。
	clk.t = utc(2026, 1, 31, 12, 0)
	r := mustSchedule(t, s, "u", "b")
	wantChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           termsOf(planYi),
		EffectivePeriod: feb(2026),
	}
	if !r.Created || r.Change != wantChange {
		t.Fatalf("schedule = %+v, want %+v", r, wantChange)
	}

	// 二月月初零点的同一瞬间与前一纳秒，各取两种时区表示。
	exact := utc(2026, 2, 1, 0, 0)
	before := exact.Add(-time.Nanosecond)
	exactPlus8 := time.Date(2026, time.February, 1, 8, 0, 0, 0, zonePlus8)
	beforePlus8 := time.Date(2026, time.February, 1, 7, 59, 59, 999999999, zonePlus8)
	if !exactPlus8.Equal(exact) || !beforePlus8.Equal(before) {
		t.Fatal("test setup: zoned representations are not the expected instants")
	}

	// 月初前最后一纳秒：无论 UTC 还是 +08:00 表示，都仍显示甲和待生效安排。
	for _, at := range []time.Time{before, beforePlus8} {
		clk.t = at
		st, err := s.Status("u")
		if err != nil {
			t.Fatal(err)
		}
		if st.CurrentTerms != termsOf(planJia) {
			t.Fatalf("at %v: current = %+v, want %+v", at, st.CurrentTerms, termsOf(planJia))
		}
		if st.PendingChange == nil || *st.PendingChange != wantChange {
			t.Fatalf("at %v: pending = %+v, want %+v", at, st.PendingChange, wantChange)
		}
	}

	// 恰到 2026-02-01 00:00 UTC：直接查询（未接收用量、未出账）即显示
	// 乙的完整条件，待生效安排消失；换 +08:00 表示的同一瞬间结果相同。
	for _, at := range []time.Time{exact, exactPlus8} {
		clk.t = at
		st, err := s.Status("u")
		if err != nil {
			t.Fatal(err)
		}
		if st.CurrentTerms != termsOf(planYi) {
			t.Fatalf("at %v: current = %+v, want %+v", at, st.CurrentTerms, termsOf(planYi))
		}
		if st.PendingChange != nil {
			t.Fatalf("at %v: pending = %+v, want nil", at, st.PendingChange)
		}
	}

	// 在生效瞬间再次选择乙：已生效的切换就是当前套餐，必须返回
	// ErrPlanChangeSamePlan——不得当作尚未生效的重复安排，也不得新建三月安排。
	clk.t = exact
	if _, err := s.SchedulePlanChange("u", "b"); !errors.Is(err, ErrPlanChangeSamePlan) {
		t.Fatalf("re-select b at effective instant = %v, want ErrPlanChangeSamePlan", err)
	}
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != termsOf(planYi) || st.PendingChange != nil {
		t.Fatalf("after same-plan rejection: current=%+v pending=%+v", st.CurrentTerms, st.PendingChange)
	}
}

// TestPlanChangeBillingUsesEffectiveMonthTerms 用账单锁定切换月份的影响：
// 二月生效的安排下，一月用量 15 单位的账单按甲应付 1500 分，
// 二月用量 25 单位的账单按乙应付 3000 分；账单采用对应月份的完整套餐条件，
// 按整月收费和提供额度。账期结束后生成账单时，时钟使用其他时区也不能
// 使账单混入另一月的套餐。
func TestPlanChangeBillingUsesEffectiveMonthTerms(t *testing.T) {
	s, clk := newJiaYiService(t)

	// 一月用量 15 单位，发生在甲的有效订阅期间、账户未停用。
	clk.t = utc(2026, 1, 20, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-jan", At: utc(2026, 1, 20, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}

	// 本地钟面已进入二月的时刻（2026-02-01 07:30 +08 == 2026-01-31 23:30 UTC）
	// 安排改用乙：生效账期为二月。
	clk.t = time.Date(2026, time.February, 1, 7, 30, 0, 0, zonePlus8)
	r := mustSchedule(t, s, "u", "b")
	if !r.Created || r.Change.EffectivePeriod != feb(2026) {
		t.Fatalf("schedule = %+v, want effective 2026-02", r)
	}

	// 一月账期结束后出账；时钟用 +08:00 表示（本地 2026-02-01 08:00）。
	// 账单必须采用一月的甲条件：月费 1000、超额 5×100=500、税 0，应付 1500。
	clk.t = time.Date(2026, time.February, 1, 8, 0, 0, 0, zonePlus8)
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	if janBill.Terms != termsOf(planJia) ||
		janBill.TotalUsage != 15 || janBill.IncludedUnits != 10 ||
		janBill.OverageUnits != 5 || janBill.MonthlyFee != 1000 ||
		janBill.OverageFee != 500 || janBill.Tax != 0 || janBill.TotalDue != 1500 {
		t.Fatalf("jan bill mixed terms: %+v", janBill)
	}
	// 截止前结清一月账单，避免欠费停用阻止二月用量。
	if _, err := s.RecordPayment("u", "pay-jan", jan(2026), janBill.TotalDue); err != nil {
		t.Fatal(err)
	}

	// 二月用量 25 单位，发生在乙生效后的订阅期间、账户未停用。
	clk.t = utc(2026, 2, 10, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-feb", At: utc(2026, 2, 10, 0, 0), Quantity: 25}); err != nil {
		t.Fatal(err)
	}

	// 二月账期结束后出账；时钟用 -05:00 表示（本地 2026-02-28 19:00，
	// 即 2026-03-01 00:00 UTC）。账单必须采用二月的乙条件：
	// 月费 2000、超额 5×200=1000、税 0，应付 3000。
	clk.t = time.Date(2026, time.February, 28, 19, 0, 0, 0, zoneMinus5)
	if !clk.t.UTC().Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("billing clock = %v, want 2026-03-01 00:00 UTC", clk.t.UTC())
	}
	febBill, err := s.CreateBill("u", feb(2026))
	if err != nil {
		t.Fatalf("create feb bill: %v", err)
	}
	if febBill.Terms != termsOf(planYi) ||
		febBill.TotalUsage != 25 || febBill.IncludedUnits != 20 ||
		febBill.OverageUnits != 5 || febBill.MonthlyFee != 2000 ||
		febBill.OverageFee != 1000 || febBill.Tax != 0 || febBill.TotalDue != 3000 {
		t.Fatalf("feb bill mixed terms: %+v", febBill)
	}

	// 一月账单的条件、用量与金额不因二月出账或切换生效而变化。
	gotJan, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if gotJan.Terms != termsOf(planJia) || gotJan.TotalUsage != 15 || gotJan.TotalDue != 1500 {
		t.Fatalf("jan bill changed: %+v", gotJan)
	}
}
