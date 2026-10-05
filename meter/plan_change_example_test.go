package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_schedulePlanChange 演示“修改套餐定义”与“给账户安排换套餐”
// 的区别：UpdatePlan 只影响之后新保存的条件快照（之后开通的订阅与之后新
// 接受的换套餐安排），不改写账户正在使用的条件，也不改写此前已接受的换
// 套餐安排；SchedulePlanChange 在被接受时锁定目标套餐当时的完整条件。
//
// 账户 acct-switch 2026-01-01 00:00 UTC 开通 plan-a（月费 1000 分、包含
// 10 单位、超额单价 100 分、税率 10%），一月中旬安排二月换到 plan-b
// （当时定义为月费 2000 分、包含 20 单位、超额单价 200 分、税率 6%）。
// 随后 UpdatePlan 把 plan-b 的定义改为月费 5000 分、包含 5 单位、超额
// 单价 900 分、税率 25%：已接受的安排不被改写，再次安排同一目标返回原
// 安排（Created=false），不重新取价。二月月初零点（UTC）后直接查询即
// 显示锁定的 plan-b 条件；一月账单仍按 plan-a 计费，二月账单按锁定的
// plan-b 条件计费，修改后的定义不混入这次切换的计费。
//
// 示例通过 NewServiceWithClock 注入可推进的时钟，输出不依赖运行当天日期。
func ExampleService_schedulePlanChange() {
	const accountID = "acct-switch"
	jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))
	feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))

	// 可推进的时钟：初始当前时刻为 2026-01-15 12:00 UTC（一月中旬），
	// 之后逐段推进到二月、三月，复制后在任何真实日期运行结果都相同。
	now := mustParseTime("2026-01-15T12:00:00Z")
	s := meter.NewServiceWithClock(func() time.Time { return now })

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐 plan-a：月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-a", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	// 套餐 plan-b 当前定义：月费 2000 分、包含 20 单位、超额单价 200 分、税率 6%。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-b", MonthlyFee: 2000, IncludedUnits: 20,
		OveragePrice: 200, TaxRateBasisPoints: 600,
	}); err != nil {
		panic(err)
	}
	// 账户一月起使用 plan-a。
	if err := s.Subscribe(accountID, "plan-a", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 一月用量 15 单位，超出 plan-a 包含的 10 单位。
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-jan",
		At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 15,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-jan accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 一月中旬安排二月换到 plan-b：安排被接受时锁定目标套餐当时的
	// 完整条件与生效账期（请求被接受时的下一个 UTC 自然月）。
	r, err := s.SchedulePlanChange(accountID, "plan-b")
	if err != nil {
		panic(err)
	}
	fmt.Printf("schedule    created=%t target=%s effective=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d\n",
		r.Created, r.Change.TargetPlanID, r.Change.EffectivePeriod,
		r.Change.Terms.MonthlyFee, r.Change.Terms.IncludedUnits,
		r.Change.Terms.OveragePrice, r.Change.Terms.TaxRateBasisPoints)

	// 安排当月仍按旧套餐：查询仍显示 plan-a，待生效安排为 plan-b。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status      current=%s monthlyFee=%d pending=%s pendingEffective=%s\n",
		st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.PendingChange.TargetPlanID, st.PendingChange.EffectivePeriod)

	// 修改 plan-b 的定义：月费、包含额度、超额单价、税率全部改变。
	// 修改只影响之后新保存的条件快照，不改写已接受的安排。
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-b", MonthlyFee: 5000, IncludedUnits: 5,
		OveragePrice: 900, TaxRateBasisPoints: 2500,
	}); err != nil {
		panic(err)
	}

	// 再次安排同一个目标 plan-b：返回原安排，Created=false，条件仍是
	// 首次安排时锁定的快照，不按修改后的定义重新取价。
	r, err = s.SchedulePlanChange(accountID, "plan-b")
	if err != nil {
		panic(err)
	}
	fmt.Printf("reschedule  created=%t target=%s effective=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d\n",
		r.Created, r.Change.TargetPlanID, r.Change.EffectivePeriod,
		r.Change.Terms.MonthlyFee, r.Change.Terms.IncludedUnits,
		r.Change.Terms.OveragePrice, r.Change.Terms.TaxRateBasisPoints)

	// 待生效安排的条件也没有被修改后的定义改写。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after update current=%s pending=%s pendingMonthlyFee=%d pendingTaxRateBasisPoints=%d\n",
		st.CurrentTerms.PlanID, st.PendingChange.TargetPlanID,
		st.PendingChange.Terms.MonthlyFee, st.PendingChange.Terms.TaxRateBasisPoints)

	// 推进到 2026-02-01 00:00 UTC（二月月初零点）：安排自动生效，
	// 直接查询即显示锁定的 plan-b 条件，待生效安排消失，
	// 无须先上报用量或生成账单。
	now = mustParseTime("2026-02-01T00:00:00Z")
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status      current=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d pending=%v\n",
		st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
		st.CurrentTerms.TaxRateBasisPoints, st.PendingChange)

	// “重复安排返回原安排”仅限于安排尚未生效时：生效后 plan-b 已是
	// 当前套餐，再安排它按现有规则返回同套餐错误。
	_, err = s.SchedulePlanChange(accountID, "plan-b")
	fmt.Printf("same plan   errSamePlan=%t\n", errors.Is(err, meter.ErrPlanChangeSamePlan))

	// 一月账期已结束，为一月出账：仍按 plan-a 收完整月费、给完整额度。
	// 用量 15，超额 5 × 100 = 500 分；税 (1000+500) × 10% = 150 分。
	janBill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill jan    plan=%s totalUsage=%d monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		janBill.Terms.PlanID, janBill.TotalUsage, janBill.MonthlyFee,
		janBill.IncludedUnits, janBill.OverageUnits, janBill.OverageFee,
		janBill.Tax, janBill.TotalDue, janBill.DueAt.Format(time.RFC3339))

	// 一月账单截止 2026-02-08 00:00 UTC。当前时刻推进到 2 月 10 日：
	// 若不结清，欠费停用会阻止二月上报新用量，因此先正常登记付款。
	now = mustParseTime("2026-02-10T00:00:00Z")
	pay, err := s.RecordPayment(accountID, "pay-jan", jan, janBill.TotalDue)
	if err != nil {
		panic(err)
	}
	fmt.Printf("payment     registered=%t paymentID=%s amount=%d billBalance=%d settled=%t\n",
		pay.Registered, pay.Payment.PaymentID, pay.Payment.Amount, pay.BillBalance, pay.Settled)

	// 二月用量 25 单位，超出锁定的 plan-b 包含的 20 单位。
	ev, err = s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-feb",
		At: mustParseTime("2026-02-10T00:00:00Z"), Quantity: 25,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-feb accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 二月账期结束后为二月出账：按安排时锁定的 plan-b 条件计费
	// （月费 2000 分，不是修改后的 5000 分）。用量 25，超额
	// 5 × 200 = 1000 分；税 (2000+1000) × 6% = 180 分。
	now = mustParseTime("2026-03-01T00:00:00Z")
	febBill, err := s.CreateBill(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill feb    plan=%s totalUsage=%d monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		febBill.Terms.PlanID, febBill.TotalUsage, febBill.MonthlyFee,
		febBill.IncludedUnits, febBill.OverageUnits, febBill.OverageFee,
		febBill.Tax, febBill.TotalDue, febBill.DueAt.Format(time.RFC3339))

	// 二月出账后查询一月账单：超额费用、税额与总应付保持原样。
	janAgain, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill jan plan=%s totalUsage=%d overageFee=%d tax=%d totalDue=%d\n",
		janAgain.Terms.PlanID, janAgain.TotalUsage, janAgain.OverageFee,
		janAgain.Tax, janAgain.TotalDue)

	// Output:
	// event e-jan accepted=true period=2026-01
	// schedule    created=true target=plan-b effective=2026-02 monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600
	// status      current=plan-a monthlyFee=1000 pending=plan-b pendingEffective=2026-02
	// reschedule  created=false target=plan-b effective=2026-02 monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600
	// after update current=plan-a pending=plan-b pendingMonthlyFee=2000 pendingTaxRateBasisPoints=600
	// status      current=plan-b monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600 pending=<nil>
	// same plan   errSamePlan=true
	// bill jan    plan=plan-a totalUsage=15 monthlyFee=1000 includedUnits=10 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
	// payment     registered=true paymentID=pay-jan amount=1650 billBalance=0 settled=true
	// event e-feb accepted=true period=2026-02
	// bill feb    plan=plan-b totalUsage=25 monthlyFee=2000 includedUnits=20 overageUnits=5 overageFee=1000 tax=180 totalDue=3180 dueAt=2026-03-08T00:00:00Z
	// get bill jan plan=plan-a totalUsage=15 overageFee=500 tax=150 totalDue=1650
}
