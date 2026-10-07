package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_estimateCurrentBill 演示账期尚未结束时查询当月预计费用。
// 示例只使用 meter 的公开入口（账户、套餐、订阅、用量、换套餐、预估），
// 并通过 NewServiceWithClock 推进当前时刻，复制后在任何真实日期运行结果
// 都相同。
//
// 账户 acct-estimate 在 2026-01-10 12:00 UTC（一月中旬）开通 plan-est
// （月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%）：月中开通
// 也按完整月费与完整额度预估。无用量时预计应付为月费 1000 分加税 100 分；
// 接收 15 单位后超额 5 单位、超额费 500 分、税 150 分、预计应付 1650 分。
// 预估不保存账单、不关闭当月。一月中旬安排二月换到 plan-b 并把两个套餐的
// 定义都改价：一月预估继续使用订阅快照；推进到二月月初后直接预估即使用
// 安排接受时锁定的 plan-b 条件（2000/20/200/600），无须先上报用量。
// 最后演示账户不存在、从未开通、提前登记但开通时刻未到三种不可预估的
// 情况各自返回已有错误。任一步意外失败即 panic 中断；预期内的错误显式
// 判断并打印。
func ExampleService_estimateCurrentBill() {
	// 可推进的时钟：初始当前时刻为 2026-01-10 12:00 UTC（一月中旬）。
	now := mustParseTime("2026-01-10T12:00:00Z")
	s := meter.NewServiceWithClock(func() time.Time { return now })

	// plan-est：月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-est", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	// plan-b：安排换套餐时锁定的目标套餐，月费 2000、包含 20、
	// 超额单价 200、税率 6%。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-b", MonthlyFee: 2000, IncludedUnits: 20,
		OveragePrice: 200, TaxRateBasisPoints: 600,
	}); err != nil {
		panic(err)
	}
	if err := s.CreateAccount("acct-estimate"); err != nil {
		panic(err)
	}
	// 月中开通：当月仍按完整月费与完整额度预估，不按天折算。
	if err := s.Subscribe("acct-estimate", "plan-est",
		mustParseTime("2026-01-10T12:00:00Z")); err != nil {
		panic(err)
	}

	// 无任何用量也能预估：累计与超额为零，月费 1000 分完整计入，
	// 税 1000×10%=100 分，预计应付 1100 分。
	e, err := s.EstimateCurrentBill("acct-estimate")
	if err != nil {
		panic(err)
	}
	fmt.Printf("no usage    estimated=%t period=%s plan=%s totalUsage=%d overageUnits=%d monthlyFee=%d overageFee=%d tax=%d totalDue=%d\n",
		e.Estimated, e.Period, e.Terms.PlanID, e.TotalUsage, e.OverageUnits,
		e.MonthlyFee, e.OverageFee, e.Tax, e.EstimatedTotalDue)

	// 推进到一月中旬再上报并预估。
	now = mustParseTime("2026-01-15T12:00:00Z")
	// 接收 15 单位：超额 5 × 100 = 500 分；税 (1000+500) × 10% = 150 分。
	if _, err := s.RecordEvent(meter.Event{
		AccountID: "acct-estimate", EventID: "e1",
		At: mustParseTime("2026-01-12T00:00:00Z"), Quantity: 15,
	}); err != nil {
		panic(err)
	}
	e, err = s.EstimateCurrentBill("acct-estimate")
	if err != nil {
		panic(err)
	}
	fmt.Printf("with usage  estimated=%t period=%s totalUsage=%d includedUnits=%d overageUnits=%d monthlyFee=%d overageFee=%d tax=%d totalDue=%d\n",
		e.Estimated, e.Period, e.TotalUsage, e.IncludedUnits, e.OverageUnits,
		e.MonthlyFee, e.OverageFee, e.Tax, e.EstimatedTotalDue)

	// 预估不保存账单、不关闭当月：当月仍是未出账状态。
	if _, err := s.GetBill("acct-estimate", meter.MonthOf(now)); err != nil {
		fmt.Printf("bill saved  errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))
	}

	// 一月中旬安排二月换到 plan-b，随后修改 plan-est 与 plan-b 的定义：
	// 一月预估继续使用订阅保存的快照，二月安排继续使用安排时锁定的快照。
	if _, err := s.SchedulePlanChange("acct-estimate", "plan-b"); err != nil {
		panic(err)
	}
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-est", MonthlyFee: 9, IncludedUnits: 0,
		OveragePrice: 9, TaxRateBasisPoints: 0,
	}); err != nil {
		panic(err)
	}
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-b", MonthlyFee: 5000, IncludedUnits: 5,
		OveragePrice: 900, TaxRateBasisPoints: 2500,
	}); err != nil {
		panic(err)
	}
	e, err = s.EstimateCurrentBill("acct-estimate")
	if err != nil {
		panic(err)
	}
	fmt.Printf("jan kept    plan=%s monthlyFee=%d overagePrice=%d taxRateBasisPoints=%d totalDue=%d\n",
		e.Terms.PlanID, e.MonthlyFee, e.Terms.OveragePrice,
		e.Terms.TaxRateBasisPoints, e.EstimatedTotalDue)

	// 推进到 2026-02-01 00:00 UTC：安排已生效，直接预估即使用安排接受时
	// 锁定的 plan-b 条件（2000/20/200/600），无须先上报用量；二月尚无
	// 用量，月费 2000、税 2000×6%=120、预计应付 2120。
	now = mustParseTime("2026-02-01T00:00:00Z")
	e, err = s.EstimateCurrentBill("acct-estimate")
	if err != nil {
		panic(err)
	}
	fmt.Printf("feb terms   estimated=%t period=%s plan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d totalUsage=%d totalDue=%d\n",
		e.Estimated, e.Period, e.Terms.PlanID, e.MonthlyFee, e.IncludedUnits,
		e.Terms.OveragePrice, e.Terms.TaxRateBasisPoints,
		e.TotalUsage, e.EstimatedTotalDue)

	// 不可预估的情况：不存在的账户、从未开通的账户、提前登记但开通时刻
	// 未到的账户，分别返回各自的已有错误，不输出预估。
	if err := s.CreateAccount("acct-no-sub"); err != nil {
		panic(err)
	}
	_, err = s.EstimateCurrentBill("ghost")
	fmt.Printf("ghost       errAccountNotFound=%t\n", errors.Is(err, meter.ErrAccountNotFound))
	_, err = s.EstimateCurrentBill("acct-no-sub")
	fmt.Printf("no sub      errSubscriptionNotFound=%t\n", errors.Is(err, meter.ErrSubscriptionNotFound))
	if err := s.CreateAccount("acct-future-sub"); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-future-sub", "plan-est",
		mustParseTime("2026-02-10T00:00:00Z")); err != nil {
		panic(err)
	}
	_, err = s.EstimateCurrentBill("acct-future-sub")
	fmt.Printf("future sub  errSubscriptionNotActivated=%t\n", errors.Is(err, meter.ErrSubscriptionNotActivated))

	// Output:
	// no usage    estimated=true period=2026-01 plan=plan-est totalUsage=0 overageUnits=0 monthlyFee=1000 overageFee=0 tax=100 totalDue=1100
	// with usage  estimated=true period=2026-01 totalUsage=15 includedUnits=10 overageUnits=5 monthlyFee=1000 overageFee=500 tax=150 totalDue=1650
	// bill saved  errBillNotFound=true
	// jan kept    plan=plan-est monthlyFee=1000 overagePrice=100 taxRateBasisPoints=1000 totalDue=1650
	// feb terms   estimated=true period=2026-02 plan=plan-b monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600 totalUsage=0 totalDue=2120
	// ghost       errAccountNotFound=true
	// no sub      errSubscriptionNotFound=true
	// future sub  errSubscriptionNotActivated=true
}
