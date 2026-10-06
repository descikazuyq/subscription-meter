package meter_test

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_billAmount 演示单张账单的计价：金额单位为分，用量按整个
// UTC 自然月累计；总用量未超过包含额度时超额费为零，超过时仅对超出的单位
// 按超额单价计费，月费仍完整计入；月费与超额费先相加，对合计金额按万分比
// 税率计算一次税额并四舍五入到整分（恰好半分向上）。计税使用账单所属月份
// 适用的套餐条件快照，不直接套用后来修改的套餐定义。
//
// 账户 acct-bill 2026-01-01 00:00 UTC 开通 plan-tiny（月费 3 分、包含
// 2 单位、超额单价 3 分、税率 1000 即 10%），开通后套餐定义被修改，但
// 一月账单仍按开通时保存的条件计费。一月累计用量 3 单位：超额 1 单位、
// 超额费 3 分，税前合计 3+3=6 分，税额为 6×10%=0.6 分四舍五入得 1 分，
// 应付 7 分。若错误地分别对两项 3 分费用计税，各得 0.3 分舍为 0，税额
// 合计 0 分、应付 6 分——税额必须对合计金额计算一次。
//
// 示例通过 NewServiceWithClock 把当前时刻固定在 2026-02-01 00:00 UTC，
// 使 2026-01 成为已经结束的账期，输出不依赖运行当天日期。
func ExampleService_billAmount() {
	const accountID = "acct-bill"
	jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))

	// 把“当前时刻”固定在 2026-02-01 00:00 UTC：2026-01 账期恰好结束，
	// 可以立即为该月出账；复制后在任何真实日期运行结果都相同。
	s := meter.NewServiceWithClock(func() time.Time {
		return mustParseTime("2026-02-01T00:00:00Z")
	})

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐 plan-tiny：月费 3 分、包含 2 单位、超额单价 3 分、
	// 税率 1000（万分比，即 10%）。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-tiny", MonthlyFee: 3, IncludedUnits: 2,
		OveragePrice: 3, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	// 账户 2026-01-01 00:00 UTC 开通，开通时保存当时的套餐条件快照。
	if err := s.Subscribe(accountID, "plan-tiny", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 开通后修改套餐定义：只影响之后新保存的条件快照，不改写已开通
	// 订阅保存的快照，一月账单仍按开通时的条件计费。
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-tiny", MonthlyFee: 900, IncludedUnits: 0,
		OveragePrice: 900, TaxRateBasisPoints: 2500,
	}); err != nil {
		panic(err)
	}

	// 一月用量分两条事件上报，按发生时刻归入 2026-01 账期，整月累计。
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e1",
		At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 2,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e1    accepted=%t period=%s\n", ev.Accepted, ev.Period)
	ev, err = s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e2",
		At: mustParseTime("2026-01-20T00:00:00Z"), Quantity: 1,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e2    accepted=%t period=%s\n", ev.Accepted, ev.Period)

	usage, err := s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 为一月出账：总用量 3 超过包含额度 2，超额 1 单位 × 3 分 = 3 分；
	// 月费 3 分完整计入；税前合计 6 分；税额对合计计算一次：
	// 6 × 1000 / 10000 = 0.6 分，四舍五入得 1 分；应付 7 分。
	bill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill jan    plan=%s totalUsage=%d includedUnits=%d overageUnits=%d monthlyFee=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		bill.Terms.PlanID, bill.TotalUsage, bill.IncludedUnits,
		bill.OverageUnits, bill.MonthlyFee, bill.OverageFee,
		bill.Tax, bill.TotalDue, bill.DueAt.Format(time.RFC3339))

	// 账单采用的条件是开通时保存的快照，不是修改后的套餐定义。
	fmt.Printf("terms       monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d\n",
		bill.Terms.MonthlyFee, bill.Terms.IncludedUnits,
		bill.Terms.OveragePrice, bill.Terms.TaxRateBasisPoints)

	// 半分边界（税率同为 1000）：税前 4 分的税为 0.4 分，不足半分舍去，
	// 税额 0；税前 5 分的税为 0.5 分，恰好半分向上，税额 1。
	for _, fee := range []int64{4, 5} {
		id := fmt.Sprintf("acct-round-%d", fee)
		if err := s.CreateAccount(id); err != nil {
			panic(err)
		}
		if err := s.CreatePlan(meter.Plan{
			ID: fmt.Sprintf("plan-round-%d", fee), MonthlyFee: fee,
			IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 1000,
		}); err != nil {
			panic(err)
		}
		if err := s.Subscribe(id, fmt.Sprintf("plan-round-%d", fee),
			mustParseTime("2026-01-01T00:00:00Z")); err != nil {
			panic(err)
		}
		b, err := s.CreateBill(id, jan)
		if err != nil {
			panic(err)
		}
		fmt.Printf("bill fee=%d  tax=%d totalDue=%d\n", fee, b.Tax, b.TotalDue)
	}

	// 金额超出非负 int64 范围时出账失败：月费取 int64 上限，加上 10%
	// 税额后应付总额不可表示，CreateBill 返回可识别的 ErrOverflow，
	// 不保存账单，已记录的用量保持原值。
	if err := s.CreateAccount("acct-overflow"); err != nil {
		panic(err)
	}
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-max", MonthlyFee: math.MaxInt64, IncludedUnits: 0,
		OveragePrice: 0, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-overflow", "plan-max", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}
	if _, err := s.RecordEvent(meter.Event{
		AccountID: "acct-overflow", EventID: "e1",
		At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 5,
	}); err != nil {
		panic(err)
	}
	_, err = s.CreateBill("acct-overflow", jan)
	fmt.Printf("overflow    errOverflow=%t\n", errors.Is(err, meter.ErrOverflow))
	_, err = s.GetBill("acct-overflow", jan)
	fmt.Printf("overflow    errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))
	usage, err = s.MonthlyUsage("acct-overflow", jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// Output:
	// event e1    accepted=true period=2026-01
	// event e2    accepted=true period=2026-01
	// usage       period=2026-01 total=3
	// bill jan    plan=plan-tiny totalUsage=3 includedUnits=2 overageUnits=1 monthlyFee=3 overageFee=3 tax=1 totalDue=7 dueAt=2026-02-08T00:00:00Z
	// terms       monthlyFee=3 includedUnits=2 overagePrice=3 taxRateBasisPoints=1000
	// bill fee=4  tax=0 totalDue=4
	// bill fee=5  tax=1 totalDue=6
	// overflow    errOverflow=true
	// overflow    errBillNotFound=true
	// usage       period=2026-01 total=5
}
