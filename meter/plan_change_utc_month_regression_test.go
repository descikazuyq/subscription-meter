package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“换套餐安排的生效账期由接受请求时刻的下一个 UTC 自然月决定”提供
// 时区维度的回归保障：请求时刻的本地钟面可能与 UTC 月份不一致（本地已跨入
// 次月，或本地仍停在前月），生效账期、待生效信息与实际切换月份必须彼此一致，
// 且只认实际时刻的 UTC 月份。所有用例围绕已有生效订阅、无取消安排、无欠费的
// 账户，沿用 SchedulePlanChange、Status、RecordEvent、CreateBill 等公开入口与
// 现有计费规则；时钟全部由 fakeClock 显式给定，不依赖运行当天日期或机器默认时区。

// 甲：月费 1000 分、包含 10 单位、超额单价 100 分、税率零。
// 乙：月费 2000 分、包含 20 单位、超额单价 200 分、税率零。
var (
	utcMonthPlanA = planDef("a", 1000, 10, 100, 0)
	utcMonthPlanB = planDef("b", 2000, 20, 200, 0)
)

func utcMonthTermsA() PlanTerms { return termsOf(utcMonthPlanA) }
func utcMonthTermsB() PlanTerms { return termsOf(utcMonthPlanB) }

// TestScheduleEffectiveMonthUsesUTCMonthNotLocalWallClock 锁定：接受请求时刻
// 本地钟面已进入二月（UTC+8 的 2026-02-01 07:30）而 UTC 仍在一月末时，首次
// 安排改乙从二月生效，当前条件仍为甲、待生效条件为乙；同一瞬间换 UTC 或其他
// 时区表示，条件相同的账户得到完全相同的结果。反之，本地钟面仍在一月
// （UTC-5 的 2026-01-31 19:00）而 UTC 恰进入二月月初时，首次安排从三月生效，
// 二月继续使用甲。
func TestScheduleEffectiveMonthUsesUTCMonthNotLocalWallClock(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 10, 0, 0))
	mustPlan(t, s, utcMonthPlanA)
	mustPlan(t, s, utcMonthPlanB)
	// 条件相同的账户：均自 2026-01-01 起使用甲，无待生效安排、无欠费。
	for _, id := range []string{"wall-plus8", "same-utc", "same-minus5", "edge-minus5", "edge-utc"} {
		mustAccount(t, s, id)
		mustSubscribe(t, s, id, "a", utc(2026, 1, 1, 0, 0))
	}

	// 情形一：本地钟面 2026-02-01 07:30（UTC+8），本地已跨入二月，
	// 实际仍是 UTC 2026-01-31 23:30（一月末）。生效账期应取 UTC 一月的
	// 下一月：二月，而不是按本地二月再往后推一个月。
	plus8Now := time.Date(2026, time.February, 1, 7, 30, 0, 0, zonePlus8)
	plus8NowUTC := utc(2026, 1, 31, 23, 30)
	if !plus8Now.UTC().Equal(plus8NowUTC) {
		t.Fatalf("test setup: %v is not %v in UTC", plus8Now, plus8NowUTC)
	}
	if plus8Now.Month() != time.February || MonthOf(plus8Now) != jan(2026) {
		t.Fatalf("test setup: wall month %s, UTC period %s, want wall February in UTC January",
			plus8Now.Month(), MonthOf(plus8Now))
	}
	wantChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           utcMonthTermsB(),
		EffectivePeriod: feb(2026),
	}

	clk.t = plus8Now
	r1 := mustSchedule(t, s, "wall-plus8", "b")
	if !r1.Created || r1.Change != wantChange {
		t.Fatalf("plus8 schedule = %+v, want created with %+v", r1, wantChange)
	}
	// 同一瞬间用纯 UTC 表示，条件相同的账户得到相同结果。
	clk.t = plus8NowUTC
	r2 := mustSchedule(t, s, "same-utc", "b")
	// 同一瞬间用 -05:00 表示（本地 2026-01-31 18:30），结果仍然相同。
	clk.t = plus8Now.In(zoneMinus5)
	r3 := mustSchedule(t, s, "same-minus5", "b")
	if r2 != r1 || r3 != r1 {
		t.Fatalf("same instant across representations differ:\n%+v\n%+v\n%+v", r1, r2, r3)
	}

	// 即使本地钟面已经进入二月：当前条件仍为甲，待生效条件为乙、二月生效。
	for _, id := range []string{"wall-plus8", "same-utc", "same-minus5"} {
		st, err := s.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Subscribed || st.CurrentTerms != utcMonthTermsA() {
			t.Fatalf("%s current terms = %+v, want甲 %+v", id, st.CurrentTerms, utcMonthTermsA())
		}
		if st.PendingChange == nil || *st.PendingChange != wantChange {
			t.Fatalf("%s pending = %+v, want %+v", id, st.PendingChange, wantChange)
		}
	}

	// 情形二：本地钟面 2026-01-31 19:00（UTC-5），本地还停在一月，
	// 实际恰好是 UTC 2026-02-01 00:00（二月月初）。尚无待生效安排的账户
	// 首次安排改乙：生效账期应取 UTC 二月的下一月——三月，不能按本地
	// 一月少算一个月；二月继续使用甲。
	minus5Now := time.Date(2026, time.January, 31, 19, 0, 0, 0, zoneMinus5)
	minus5NowUTC := utc(2026, 2, 1, 0, 0)
	if !minus5Now.UTC().Equal(minus5NowUTC) {
		t.Fatalf("test setup: %v is not %v in UTC", minus5Now, minus5NowUTC)
	}
	if minus5Now.Month() != time.January || MonthOf(minus5Now) != feb(2026) {
		t.Fatalf("test setup: wall month %s, UTC period %s, want wall January in UTC February",
			minus5Now.Month(), MonthOf(minus5Now))
	}
	wantMarChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           utcMonthTermsB(),
		EffectivePeriod: mar(2026),
	}

	clk.t = minus5Now
	e1 := mustSchedule(t, s, "edge-minus5", "b")
	if !e1.Created || e1.Change != wantMarChange {
		t.Fatalf("minus5 schedule = %+v, want created with %+v", e1, wantMarChange)
	}
	// 同一瞬间用纯 UTC 表示，结果相同。
	clk.t = minus5NowUTC
	e2 := mustSchedule(t, s, "edge-utc", "b")
	if e2 != e1 {
		t.Fatalf("same instant across representations differ:\n%+v\n%+v", e1, e2)
	}
	st, err := s.Status("edge-minus5")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != utcMonthTermsA() || st.PendingChange == nil || *st.PendingChange != wantMarChange {
		t.Fatalf("edge status: cur=%+v pending=%+v, want 甲 + pending %+v",
			st.CurrentTerms, st.PendingChange, wantMarChange)
	}

	// 二月内查询：三月生效的安排尚未落地，当前条件仍是甲（二月继续用甲）。
	clk.t = utc(2026, 2, 10, 12, 0)
	st, err = s.Status("edge-minus5")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != utcMonthTermsA() || st.PendingChange == nil || *st.PendingChange != wantMarChange {
		t.Fatalf("edge status in feb: cur=%+v pending=%+v, want 甲 + pending %+v",
			st.CurrentTerms, st.PendingChange, wantMarChange)
	}
	// 二月账期结束后出账：整月按甲的完整条件收费（月费 1000、无用量），
	// 不能提前混入乙的条件。
	clk.t = utc(2026, 3, 1, 0, 0)
	febBill := mustBill(t, s, "edge-minus5", feb(2026))
	if febBill.Terms != utcMonthTermsA() || febBill.MonthlyFee != 1000 || febBill.TotalDue != 1000 {
		t.Fatalf("edge feb bill = %+v, want甲 terms with due 1000", febBill)
	}
	// 到达三月：安排生效，当前条件切换为乙，待生效安排消失。
	st, err = s.Status("edge-minus5")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != utcMonthTermsB() || st.PendingChange != nil {
		t.Fatalf("edge status in mar: cur=%+v pending=%+v, want 乙 no pending",
			st.CurrentTerms, st.PendingChange)
	}
}

// TestPlanChangeEffectiveInstantBoundaryAcrossZones 锁定生效瞬间的纳秒级边界：
// 对一月末已接受的二月安排，二月月初（2026-02-01 00:00 UTC）前最后一纳秒
// 查询仍显示甲和待生效安排；恰到月初零点，直接查询即显示乙的完整条件、
// 待生效安排消失，无需先接收用量或出账。生效瞬间换任何时区表达，切换不提前
// 也不延后；在该瞬间再次选择乙返回 ErrPlanChangeSamePlan，既不把已生效切换
// 当作尚未生效的重复安排，也不新建三月安排。
func TestPlanChangeEffectiveInstantBoundaryAcrossZones(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, utcMonthPlanA)
	mustPlan(t, s, utcMonthPlanB)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	// quiet 从不上报用量、从不出账：切换必须由状态查询直接体现。
	mustAccount(t, s, "quiet")
	mustSubscribe(t, s, "quiet", "a", utc(2026, 1, 1, 0, 0))

	wantChange := PlanChange{
		TargetPlanID:    "b",
		Terms:           utcMonthTermsB(),
		EffectivePeriod: feb(2026),
	}
	if r := mustSchedule(t, s, "u", "b"); !r.Created || r.Change != wantChange {
		t.Fatalf("test setup: schedule = %+v, want %+v", r, wantChange)
	}
	if r := mustSchedule(t, s, "quiet", "b"); !r.Created || r.Change != wantChange {
		t.Fatalf("test setup: quiet schedule = %+v, want %+v", r, wantChange)
	}

	// 生效瞬间：2026-02-01 00:00:00 UTC；前最后一纳秒仍属一月。
	effective := feb(2026).Start()
	lastNs := effective.Add(-time.Nanosecond)

	// 前最后一纳秒（UTC 表示）：仍显示甲和待生效安排。
	clk.t = lastNs
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != utcMonthTermsA() || st.PendingChange == nil || *st.PendingChange != wantChange {
		t.Fatalf("status one ns before effective = cur %+v pending %+v, want 甲 + %+v",
			st.CurrentTerms, st.PendingChange, wantChange)
	}
	// 同一纳秒换 +08:00 表达（本地钟面已是 2026-02-01 07:59:59.999999999，
	// 本地已进入二月）：不能提前切换。
	clk.t = lastNs.In(zonePlus8)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != utcMonthTermsA() || st.PendingChange == nil || *st.PendingChange != wantChange {
		t.Fatalf("status one ns before effective in +08:00 = cur %+v pending %+v, want 甲 + %+v",
			st.CurrentTerms, st.PendingChange, wantChange)
	}

	// 恰到 2026-02-01 00:00 UTC：直接查询即显示乙的完整条件，待生效安排
	// 消失；quiet 从未接收用量或出账，结果相同。
	clk.t = effective
	for _, id := range []string{"u", "quiet"} {
		st, err = s.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.CurrentTerms != utcMonthTermsB() || st.PendingChange != nil {
			t.Fatalf("%s status at effective instant = cur %+v pending %+v, want 乙 no pending",
				id, st.CurrentTerms, st.PendingChange)
		}
	}
	// 生效瞬间换 -05:00 表达（本地 2026-01-31 19:00，本地仍在一月）：
	// 不能延后切换。
	clk.t = effective.In(zoneMinus5)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != utcMonthTermsB() || st.PendingChange != nil {
		t.Fatalf("status at effective instant in -05:00 = cur %+v pending %+v, want 乙 no pending",
			st.CurrentTerms, st.PendingChange)
	}

	// 在生效瞬间再次选择乙：乙已是当前生效套餐，必须返回
	// ErrPlanChangeSamePlan——不得把已生效切换当作尚未生效的重复安排
	// （那样会返回 Created=false 的原安排），也不得新建三月安排。
	clk.t = effective
	if _, err := s.SchedulePlanChange("u", "b"); !errors.Is(err, ErrPlanChangeSamePlan) {
		t.Fatalf("re-select b at effective instant = %v, want ErrPlanChangeSamePlan", err)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != utcMonthTermsB() || st.PendingChange != nil {
		t.Fatalf("status after same-plan rejection = cur %+v pending %+v, want 乙 no pending",
			st.CurrentTerms, st.PendingChange)
	}
	// 同一瞬间换 +08:00 表达再选乙，结果相同。
	clk.t = effective.In(zonePlus8)
	if _, err := s.SchedulePlanChange("u", "b"); !errors.Is(err, ErrPlanChangeSamePlan) {
		t.Fatalf("re-select b at effective instant in +08:00 = %v, want ErrPlanChangeSamePlan", err)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentTerms != utcMonthTermsB() || st.PendingChange != nil {
		t.Fatalf("status after zoned same-plan rejection = cur %+v pending %+v, want 乙 no pending",
			st.CurrentTerms, st.PendingChange)
	}
}

// TestPlanChangeBillingUsesSwitchedMonthTermsAcrossZones 用账单锁定切换月份的
// 影响：安排二月生效后，一月账期整月按甲的完整条件（月费 1000、额度 10、
// 超额单价 100、税率零）出账，二月账期整月按乙的完整条件（月费 2000、
// 额度 20、超额单价 200、税率零）出账。账期结束后生成账单时，时钟使用其他
// 时区表示同一时刻，也不能使账单混入另一月的套餐。
func TestPlanChangeBillingUsesSwitchedMonthTermsAcrossZones(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, utcMonthPlanA)
	mustPlan(t, s, utcMonthPlanB)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 一月用量 15 单位（甲的订阅期间、账户无欠费未停用）。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-jan", At: utc(2026, 1, 20, 0, 0), Quantity: 15}); err != nil {
		t.Fatal(err)
	}

	// 本地钟面 2026-02-01 07:30（UTC+8）= UTC 2026-01-31 23:30 接受安排：
	// 仍是 UTC 一月末，乙从二月生效。
	clk.t = time.Date(2026, time.February, 1, 7, 30, 0, 0, zonePlus8)
	if r := mustSchedule(t, s, "u", "b"); r.Change.EffectivePeriod != feb(2026) {
		t.Fatalf("test setup: effective = %v, want 2026-02", r.Change.EffectivePeriod)
	}

	// 一月账期结束后生成一月账单：时钟用 +08:00 表示 2026-02-01 00:00 UTC
	// （本地 08:00）。此时当前套餐已切换为乙，但一月账单必须整月采用甲的
	// 完整条件：月费 1000、额度 10、超额 5×100=500、税零，应付 1500。
	clk.t = time.Date(2026, time.February, 1, 8, 0, 0, 0, zonePlus8)
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	wantJanBill := Bill{
		AccountID: "u", Period: jan(2026), Terms: utcMonthTermsA(),
		TotalUsage: 15, IncludedUnits: 10, OverageUnits: 5,
		MonthlyFee: 1000, OverageFee: 500, Tax: 0, TotalDue: 1500,
		Paid: 0, Balance: 1500, Settled: false,
		DueAt: utc(2026, 2, 8, 0, 0),
	}
	if janBill != wantJanBill {
		t.Fatalf("jan bill = %+v, want %+v", janBill, wantJanBill)
	}

	// 二月用量 25 单位（乙生效后的订阅期间；一月账单未到付款截止，未停用）。
	clk.t = utc(2026, 2, 2, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "e-feb", At: utc(2026, 2, 2, 0, 0), Quantity: 25}); err != nil {
		t.Fatal(err)
	}

	// 二月账期结束后生成二月账单：时钟用 -05:00 表示 2026-03-01 00:00 UTC
	// （本地 2026-02-28 19:00，钟面仍在二月）。二月账单必须整月采用乙的
	// 完整条件：月费 2000、额度 20、超额 5×200=1000、税零，应付 3000。
	clk.t = time.Date(2026, time.February, 28, 19, 0, 0, 0, zoneMinus5)
	febBill, err := s.CreateBill("u", feb(2026))
	if err != nil {
		t.Fatalf("create feb bill: %v", err)
	}
	wantFebBill := Bill{
		AccountID: "u", Period: feb(2026), Terms: utcMonthTermsB(),
		TotalUsage: 25, IncludedUnits: 20, OverageUnits: 5,
		MonthlyFee: 2000, OverageFee: 1000, Tax: 0, TotalDue: 3000,
		Paid: 0, Balance: 3000, Settled: false,
		DueAt: utc(2026, 3, 8, 0, 0),
	}
	if febBill != wantFebBill {
		t.Fatalf("feb bill = %+v, want %+v", febBill, wantFebBill)
	}

	// 出账后两张账单各自固定：一月不混入乙，二月不沿用甲。
	if got, _ := s.GetBill("u", jan(2026)); got != wantJanBill {
		t.Fatalf("jan bill changed after feb billing: %+v", got)
	}
	if got, _ := s.GetBill("u", feb(2026)); got != wantFebBill {
		t.Fatalf("feb bill changed: %+v", got)
	}
}
