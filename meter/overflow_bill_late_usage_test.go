package meter

import (
	"errors"
	"testing"
)

// 出账金额溢出后的回归保障：一期账单因月费与超额费用之和超出 int64 无法
// 表示而出账失败时，失败不能留下任何“已出账”的痕迹——账期不被当成已关闭，
// 账户也不凭空背上欠费。此后到达正常付款截止时刻账户仍有效且未停用，该期
// 内迟到的用量仍可补报；补报后即使把套餐定义调低，原订阅仍按开通时锁定的
// 条件出账（继续溢出），而不是改用新定义生成较低金额的账单。
//
// 场景：账户 2026-01-01 00:00:00 UTC 开通，套餐月费 maxInt64 分、包含 0
// 单位、超额单价 1 分、税率 0。一月已接收 5 单位用量（远小于 int64 上限，
// 上报本身合法），月费与超额费用之和 maxInt64+5 超出金额上限。

// overflowTerms 是开通时锁定的套餐条件快照：合法但会让一月账单金额溢出。
var overflowTerms = PlanTerms{
	PlanID:             "overflow-plan",
	MonthlyFee:         maxInt64,
	IncludedUnits:      0,
	OveragePrice:       1,
	TaxRateBasisPoints: 0,
}

// newOverflowBillScenario 搭建共同初始状态：时钟停在 2026-02-01 00:00:00 UTC，
// 一月已结束；账户只有一份有效订阅，一月已接收 5 单位合法用量，无其他账单
// 与到期欠款。返回服务与时钟便于后续拨动。
func newOverflowBillScenario(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{
		ID:                 overflowTerms.PlanID,
		MonthlyFee:         overflowTerms.MonthlyFee,
		IncludedUnits:      overflowTerms.IncludedUnits,
		OveragePrice:       overflowTerms.OveragePrice,
		TaxRateBasisPoints: overflowTerms.TaxRateBasisPoints,
	})
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", overflowTerms.PlanID, utc(2026, 1, 1, 0, 0))
	// 合法用量累计：5 单位远小于 int64 上限，上报必须成功；
	// 无法表示的是应付金额，不能靠上报用量失败来代替出账失败。
	if r, err := s.RecordEvent(Event{
		AccountID: "a",
		EventID:   "jan-usage-5",
		At:        utc(2026, 1, 10, 0, 0),
		Quantity:  5,
	}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("seed january usage: %+v %v", r, err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
	return s, clk
}

// assertNoJanuaryBill 断言一月没有留下账单：查询不到，账户状态中也没有该期
// 账单摘要，账户未因这一期被停用。
func assertNoJanuaryBill(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.GetBill("a", jan(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("get january bill = %v, want ErrBillNotFound", err)
	}
	st, err := s.Status("a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Subscribed {
		t.Fatalf("subscription must stay active after failed billing")
	}
	if st.Suspended {
		t.Fatalf("failed billing must not suspend the account")
	}
	if len(st.Bills) != 0 {
		t.Fatalf("status bills = %+v, want none after overflow", st.Bills)
	}
	if st.CurrentTerms != overflowTerms {
		t.Fatalf("current terms = %+v, want subscribed snapshot %+v", st.CurrentTerms, overflowTerms)
	}
}

// assertJanuaryUsage 断言一月累计用量在单月查询与账户状态中一致。
func assertJanuaryUsage(t *testing.T, s *Service, want int64) {
	t.Helper()
	if u, err := s.MonthlyUsage("a", jan(2026)); err != nil || u.Total != want {
		t.Fatalf("january usage = %d (%v), want %d", u.Total, err, want)
	}
	st, err := s.Status("a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) ||
		st.MonthlyUsage[0].Total != want {
		t.Fatalf("status monthly usage = %+v, want january %d", st.MonthlyUsage, want)
	}
}

// TestOverflowBillKeepsPeriodOpenForLateUsage：出账溢出失败后账期不被关闭、
// 账户不欠费停用，该期迟到事件在原本应到付款截止的时刻仍可首次接收。
func TestOverflowBillKeepsPeriodOpenForLateUsage(t *testing.T) {
	s, clk := newOverflowBillScenario(t)

	// 一月结束后出账：月费 maxInt64 + 超额 5 分溢出，返回可识别的 ErrOverflow。
	if _, err := s.CreateBill("a", jan(2026)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("create january bill = %v, want ErrOverflow", err)
	}
	// 失败不留账单、不改用量与套餐条件。
	assertNoJanuaryBill(t, s)
	assertJanuaryUsage(t, s, 5)

	// 拨到账期结束后第七天：正常账单此刻应到付款截止。
	// 这一期没有账单，账户仍显示订阅有效且未因这一期停用。
	clk.t = utc(2026, 2, 8, 0, 0)
	assertNoJanuaryBill(t, s)

	// 补报一条此前未接收、发生在该期实际订阅期间的事件：发生时刻早于当前
	// 时刻，补报后累计 8 仍在合法范围内。不得返回已出账或欠费停用错误。
	r, err := s.RecordEvent(Event{
		AccountID: "a",
		EventID:   "late-jan-3",
		At:        utc(2026, 1, 20, 0, 0),
		Quantity:  3,
	})
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("late event after overflow = %+v %v, want accepted into january", r, err)
	}

	// 该月累计量只增加补报数量，单月查询与账户状态一致；补报不生成账单。
	assertJanuaryUsage(t, s, 8)
	assertNoJanuaryBill(t, s)
}

// TestOverflowBillKeepsSubscribedTermsAfterPlanRedefinition：出账失败不丢失
// 开通时保存的套餐条件。补报后把套餐定义调低到足以正常计价的水平，当前订阅
// 仍显示开通时的条件；再次为原账期出账仍按原条件溢出，刚补报的用量保留。
func TestOverflowBillKeepsSubscribedTermsAfterPlanRedefinition(t *testing.T) {
	s, clk := newOverflowBillScenario(t)

	if _, err := s.CreateBill("a", jan(2026)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("create january bill = %v, want ErrOverflow", err)
	}
	clk.t = utc(2026, 2, 8, 0, 0)
	if r, err := s.RecordEvent(Event{
		AccountID: "a",
		EventID:   "late-jan-3",
		At:        utc(2026, 1, 20, 0, 0),
		Quantity:  3,
	}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("late event after overflow = %+v %v, want accepted into january", r, err)
	}

	// 把套餐定义调低到足以正常计价的水平：只影响之后开通的订阅。
	if err := s.UpdatePlan(Plan{
		ID:                 overflowTerms.PlanID,
		MonthlyFee:         100,
		IncludedUnits:      1000,
		OveragePrice:       1,
		TaxRateBasisPoints: 0,
	}); err != nil {
		t.Fatalf("update plan: %v", err)
	}

	// 当前订阅仍显示开通时保存的条件，而不是调低后的新定义。
	assertNoJanuaryBill(t, s)

	// 再次为一月出账：仍按原条件计算（maxInt64 + 8 分）返回溢出，
	// 不能改用新定义生成较低金额的账单。
	if _, err := s.CreateBill("a", jan(2026)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("re-bill january after plan update = %v, want ErrOverflow", err)
	}
	assertNoJanuaryBill(t, s)
	// 刚补报的用量继续保留。
	assertJanuaryUsage(t, s, 8)
}
