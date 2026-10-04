package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归“出账金额溢出”与“合法用量累计”的区分：
// 应付金额无法用 int64 表示时，失败的只是出账这一步——账期不得被当成
// 已关闭，也不得凭空成为欠费来源；合法用量继续保留，之后仍可补报。
//
// 时间线（全部 UTC）：
//
//	2026-01-01 开通订阅并实际生效，套餐月费 maxInt64-100、额度 10、
//	           超额单价 200、税率 0；账户无其他账单与到期欠款。
//	1 月接收 11 单位：数量本身远小于 int64 上限，合法。
//	2026-02-01 一月结束后出账：月费 + 1*200 超出金额上限 -> ErrOverflow，
//	           但不留账单、不关闭账期、不产生欠费，用量与套餐快照保持原值。
//	2026-02-08 即正常账单应到付款截止的时刻：账户仍订阅有效、未停用；
//	           补报一条 1 月 15 日发生、此前未接收的 5 单位事件，应被首次
//	           接收并归入一月，累计只增加 5（=16），补报不生成账单。
//	之后       把套餐定义调低到足以正常计价的水平：当前订阅仍显示开通时
//	           保存的条件，再次为一月出账仍按原条件返回 ErrOverflow，
//	           不能改用新定义生成较低金额账单，刚补报的用量继续保留。

// overflowPlanTerms 是开通时锁定的套餐条件：月费接近上限，仅 1 单位超额
// 就会使月费与超额费用之和溢出；税率为零，排除计税路径的干扰。
var overflowPlanTerms = PlanTerms{
	PlanID:             "overflow-plan",
	MonthlyFee:         maxInt64 - 100,
	IncludedUnits:      10,
	OveragePrice:       200,
	TaxRateBasisPoints: 0,
}

const (
	overflowSeedUsage      = int64(11) // 一月当时接收的合法用量
	overflowBackfillUsage  = int64(5)  // 截止日当天补报的数量
	overflowUsageAfterFill = overflowSeedUsage + overflowBackfillUsage
)

// overflowBillingFixture 构造上述账户：一月初已生效的订阅、一月已接收 11
// 单位用量、时钟停在 2026-02-01 零点（一月已结束、尚未出账）。
func overflowBillingFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{
		ID:                 overflowPlanTerms.PlanID,
		MonthlyFee:         overflowPlanTerms.MonthlyFee,
		IncludedUnits:      overflowPlanTerms.IncludedUnits,
		OveragePrice:       overflowPlanTerms.OveragePrice,
		TaxRateBasisPoints: overflowPlanTerms.TaxRateBasisPoints,
	})
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", overflowPlanTerms.PlanID, utc(2026, 1, 1, 0, 0))

	clk.t = utc(2026, 1, 20, 12, 0)
	r, err := s.RecordEvent(Event{
		AccountID: "u",
		EventID:   "jan-seed-11",
		At:        utc(2026, 1, 20, 0, 0),
		Quantity:  overflowSeedUsage,
	})
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("seed january usage: %+v %v", r, err)
	}

	clk.t = utc(2026, 2, 1, 0, 0)
	return s, clk
}

// assertOverflowPeriodOpen 断言出账失败后的一月仍是“开放账期”：
// 查不到账单、状态中没有这一期的账单摘要（即没有欠费来源）、订阅有效且
// 未停用、当前条件仍是开通时锁定的快照、用量保持 wantUsage 且单月查询与
// 账户状态一致。
func assertOverflowPeriodOpen(t *testing.T, s *Service, wantUsage int64) {
	t.Helper()

	if _, err := s.GetBill("u", jan(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("january bill after overflow = %v, want ErrBillNotFound", err)
	}

	u, err := s.MonthlyUsage("u", jan(2026))
	if err != nil || u.Total != wantUsage {
		t.Fatalf("january usage = %d (%v), want %d", u.Total, err, wantUsage)
	}

	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Subscribed {
		t.Fatalf("subscription must stay active after failed billing: %+v", st)
	}
	if st.Suspended {
		t.Fatalf("failed billing must not become overdue debt: status = %+v", st)
	}
	if st.CurrentTerms != overflowPlanTerms {
		t.Fatalf("current terms = %+v, want activation snapshot %+v", st.CurrentTerms, overflowPlanTerms)
	}
	if st.PendingChange != nil || st.ScheduledEnd != nil {
		t.Fatalf("unexpected pending change/scheduled end: %+v", st)
	}
	// 没有任何账单摘要：溢出失败不能凭空造出欠费账单。
	if len(st.Bills) != 0 {
		t.Fatalf("status bills = %+v, want none", st.Bills)
	}
	if len(st.MonthlyUsage) != 1 ||
		st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: wantUsage}) {
		t.Fatalf("status monthly usage = %+v, want january %d", st.MonthlyUsage, wantUsage)
	}
}

func TestOverflowBillKeepsPeriodOpenForLateUsage(t *testing.T) {
	s, clk := overflowBillingFixture(t)

	// 一月结束后出账：月费 maxInt64-100 与超额费用 200 之和超出上限，
	// 返回可识别的 ErrOverflow，且不返回半成品账单。
	b, err := s.CreateBill("u", jan(2026))
	if !errors.Is(err, ErrOverflow) || b != (Bill{}) {
		t.Fatalf("create bill = %+v %v, want ErrOverflow with zero bill", b, err)
	}
	// 查询不到账单、状态无该期摘要、用量（11）与套餐条件保持原值。
	assertOverflowPeriodOpen(t, s, overflowSeedUsage)

	// 重复出账仍是溢出：账期没有被第一次失败悄悄关闭。
	if again, err := s.CreateBill("u", jan(2026)); !errors.Is(err, ErrOverflow) || again != (Bill{}) {
		t.Fatalf("retry create bill = %+v %v, want ErrOverflow with zero bill", again, err)
	}
	assertOverflowPeriodOpen(t, s, overflowSeedUsage)

	// 到账期结束后第七天（正常账单的付款截止时刻 2026-02-08 零点）：
	// 失败的出账没有留下到期账单，订阅仍有效、账户未停用。
	clk.t = utc(2026, 2, 8, 0, 0)
	assertOverflowPeriodOpen(t, s, overflowSeedUsage)

	// 补报一条发生在一月实际订阅期间、此前未接收的事件（时刻早于当前）：
	// 必须被首次接收并归入原账期，不能返回已出账或欠费停用错误。
	late := Event{
		AccountID: "u",
		EventID:   "late-jan-15-5",
		At:        utc(2026, 1, 15, 10, 0),
		Quantity:  overflowBackfillUsage,
	}
	r, err := s.RecordEvent(late)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("late event into overflowed period = %+v %v, want accepted into january", r, err)
	}

	// 月累计只增加补报数量；单月查询与账户状态中的用量一致；补报不生成账单。
	assertOverflowPeriodOpen(t, s, overflowUsageAfterFill)

	// 完全相同的重报去重成功但不再次累计。
	replay, err := s.RecordEvent(late)
	if err != nil || replay.Accepted || replay.Period != jan(2026) {
		t.Fatalf("identical replay = %+v %v, want dedup no-op in january", replay, err)
	}
	assertOverflowPeriodOpen(t, s, overflowUsageAfterFill)

	// 出账失败不能让订阅丢失开通时保存的套餐条件：把套餐定义调低到足以
	// 正常计价的水平（按新定义一月只需 100 分），当前订阅仍显示原快照。
	cheap := Plan{ID: overflowPlanTerms.PlanID, MonthlyFee: 100, IncludedUnits: 100, OveragePrice: 1, TaxRateBasisPoints: 0}
	if err := s.UpdatePlan(cheap); err != nil {
		t.Fatalf("update plan: %v", err)
	}
	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Subscribed || st.Suspended || st.CurrentTerms != overflowPlanTerms {
		t.Fatalf("terms after plan definition lowered = %+v, want activation snapshot", st)
	}

	// 再次为原账期出账仍按开通时锁定的原条件返回溢出：不能改用新定义
	// 生成一张较低金额的账单；刚补报的 16 单位用量继续保留。
	if b, err := s.CreateBill("u", jan(2026)); !errors.Is(err, ErrOverflow) || b != (Bill{}) {
		t.Fatalf("re-bill after plan lowered = %+v %v, want ErrOverflow with zero bill", b, err)
	}
	assertOverflowPeriodOpen(t, s, overflowUsageAfterFill)
}

// TestSuccessfullyBilledPeriodStaysClosed 对照保障：金额合法、成功出账后的
// 既有规则不因溢出保障而改变——同样是“账期结束后补报一月事件”，已关闭
// 账期拒收新事件（ErrMonthBilled），用量固定在出账时的值，重复出账得到
// 同一张金额固定的账单，状态中恰有这一期的账单摘要。
func TestSuccessfullyBilledPeriodStaysClosed(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, Plan{ID: "closed-plan", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 7, TaxRateBasisPoints: 500})
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "closed-plan", utc(2026, 1, 1, 0, 0))

	if r, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "jan-seed-8", At: utc(2026, 1, 20, 0, 0), Quantity: 8,
	}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("seed usage: %+v %v", r, err)
	}

	// 进入二月零点：一月已结束，可出账；账期结束后补报的新事件也在此之后。
	clk.t = utc(2026, 2, 1, 0, 0)

	// 月费 1000、用量 8 无超额，税 50，应付 1050。
	wantTerms := PlanTerms{PlanID: "closed-plan", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 7, TaxRateBasisPoints: 500}
	b, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create bill: %v", err)
	}
	if b.Terms != wantTerms || b.TotalUsage != 8 || b.OverageUnits != 0 ||
		b.MonthlyFee != 1000 || b.OverageFee != 0 || b.Tax != 50 || b.TotalDue != 1050 ||
		b.Paid != 0 || b.Balance != 1050 || b.Settled {
		t.Fatalf("bill = %+v", b)
	}

	// 账期结束后补报的新事件（在溢出场景下会被开放账期接收）这里必须被拒。
	late := Event{
		AccountID: "u",
		EventID:   "late-jan-31-5",
		At:        time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC),
		Quantity:  5,
	}
	if _, err := s.RecordEvent(late); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("late event after successful bill = %v, want ErrMonthBilled", err)
	}
	// 被拒事件不占用标识也不累计：一月用量固定为 8。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 8 {
		t.Fatalf("january usage = %d, want 8", u.Total)
	}

	// 重复出账得到同一张账单，金额与用量固定；查询结果完全一致。
	again, err := s.CreateBill("u", jan(2026))
	if err != nil || again != b {
		t.Fatalf("re-bill not identical:\nfirst=%+v\nagain=%+v err=%v", b, again, err)
	}
	got, err := s.GetBill("u", jan(2026))
	if err != nil || got != b {
		t.Fatalf("get bill changed: %+v %v", got, err)
	}

	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("status subscription flags = %+v", st)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 8}) {
		t.Fatalf("status usage = %+v, want january 8", st.MonthlyUsage)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) ||
		st.Bills[0].TotalDue != 1050 || st.Bills[0].Paid != 0 ||
		st.Bills[0].Balance != 1050 || st.Bills[0].Settled {
		t.Fatalf("status bills = %+v, want one january bill of 1050 unpaid", st.Bills)
	}
}
