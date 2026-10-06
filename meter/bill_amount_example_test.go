package meter_test

import (
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_billAmount 演示单张账单的金额计算：包含额度如何决定超额
// 费用、月费如何完整计入，以及税额只对“月费 + 超额费用”的税前合计计算
// 一次并四舍五入到整分。示例只使用 meter 的公开入口（账户、套餐、订阅、
// 用量、出账），并通过 NewServiceWithClock 把当前时刻固定在
// 2026-02-01 00:00 UTC，使 2026-01 成为已经结束的账期，复制后在任何
// 真实日期运行结果都相同。
//
// 主账户 acct-bill 的套餐为月费 3 分、包含 2 单位、超额单价 3 分、
// 税率 1000（10%），一月累计用量 3 单位：超额 1 单位、超额费 3 分、
// 税前合计 6 分、税额 1 分（6×10%=0.6 向上取整）、应付 7 分。
// acct-bill-within 使用同样套餐但只用 2 单位（恰好等于额度）：超额费 0，
// 月费 3 分仍完整计入。acct-round-down / acct-round-half 分别是月费
// 4 分、5 分且税率 1000 的零用量账单，展示不足半分舍去（0.4→0）与
// 恰好半分向上（0.5→1）。出账前还会修改套餐定义，一月账单仍按一月
// 适用的开通快照计税。任一步失败即 panic 中断，不会继续输出貌似成功
// 的账单。
func ExampleService_billAmount() {
	jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))

	// 把当前时刻固定在 2026-02-01 00:00 UTC：2026-01 账期恰好结束，
	// 可以立即为一月出账；复制后在任何真实日期运行结果都相同。
	now := mustParseTime("2026-02-01T00:00:00Z")
	s := meter.NewServiceWithClock(func() time.Time { return now })

	// 主示例套餐 plan-small：月费 3 分、包含 2 单位、超额单价 3 分、
	// 税率 1000（万分比，即 10%）。
	if err := s.CreatePlan(meter.Plan{
		ID:                 "plan-small",
		MonthlyFee:         3,
		IncludedUnits:      2,
		OveragePrice:       3,
		TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	// 两个只有月费的套餐，用于展示税前 4 分、5 分在 10% 税率下的舍入：
	// 0.4 分不足半分舍去，0.5 分恰好半分向上。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-round-down", MonthlyFee: 4, IncludedUnits: 0,
		OveragePrice: 0, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-round-half", MonthlyFee: 5, IncludedUnits: 0,
		OveragePrice: 0, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}

	for _, id := range []string{
		"acct-bill", "acct-bill-within", "acct-round-down", "acct-round-half",
	} {
		if err := s.CreateAccount(id); err != nil {
			panic(err)
		}
	}

	// 订阅时刻明确：四个账户都在 2026-01-01 00:00 UTC 开通，一月整月
	// 被订阅覆盖，首月按完整月费与完整额度计费。
	if err := s.Subscribe("acct-bill", "plan-small",
		mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-bill-within", "plan-small",
		mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-round-down", "plan-round-down",
		mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-round-half", "plan-round-half",
		mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 事件时刻明确：acct-bill 在 1 月 10 日上报 3 单位，按发生时刻
	// 全部归入 2026-01 账期，当月累计为 3。
	ev, err := s.RecordEvent(meter.Event{
		AccountID: "acct-bill", EventID: "e1",
		At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 3,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event over   accepted=%t period=%s\n", ev.Accepted, ev.Period)
	usage, err := s.MonthlyUsage("acct-bill", jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage over   period=%s total=%d\n", usage.Period, usage.Total)

	// acct-bill-within 只用 2 单位，恰好等于包含额度，没有超额。
	ev, err = s.RecordEvent(meter.Event{
		AccountID: "acct-bill-within", EventID: "e1",
		At: mustParseTime("2026-01-11T00:00:00Z"), Quantity: 2,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event within accepted=%t period=%s\n", ev.Accepted, ev.Period)
	usage, err = s.MonthlyUsage("acct-bill-within", jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage within period=%s total=%d\n", usage.Period, usage.Total)

	// 账期结束后修改 plan-small 的定义：一月账单仍按一月适用的开通
	// 快照计费，不套用修改后的定义。
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-small", MonthlyFee: 9, IncludedUnits: 0,
		OveragePrice: 9, TaxRateBasisPoints: 0,
	}); err != nil {
		panic(err)
	}

	// 为一月出账（当前时刻固定在 2026-02-01 00:00 UTC，账期已结束）：
	// 用量 3，超额 3-2=1 单位；超额费 1×3=3 分；月费 3 分完整计入；
	// 税前合计 3+3=6；税额对合计只算一次：6×1000/10000=0.6 分，
	// 四舍五入为 1 分；应付 6+1=7 分。不能分别给两项 3 分费用各算
	// 一次税：0.3+0.3 会被分别舍成 0+0。
	bill, err := s.CreateBill("acct-bill", jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill over   plan=%s monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d subtotal=%d taxRateBasisPoints=%d tax=%d totalDue=%d dueAt=%s\n",
		bill.Terms.PlanID, bill.MonthlyFee, bill.IncludedUnits,
		bill.OverageUnits, bill.OverageFee, bill.MonthlyFee+bill.OverageFee,
		bill.Terms.TaxRateBasisPoints, bill.Tax, bill.TotalDue,
		bill.DueAt.Format(time.RFC3339))

	// 恰好用完包含额度：超额单位 0、超额费 0，月费 3 分仍完整计入；
	// 税前 3 分，3×10%=0.3 不足半分，税额 0，应付 3 分。
	within, err := s.CreateBill("acct-bill-within", jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill within plan=%s monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d subtotal=%d taxRateBasisPoints=%d tax=%d totalDue=%d dueAt=%s\n",
		within.Terms.PlanID, within.MonthlyFee, within.IncludedUnits,
		within.OverageUnits, within.OverageFee, within.MonthlyFee+within.OverageFee,
		within.Terms.TaxRateBasisPoints, within.Tax, within.TotalDue,
		within.DueAt.Format(time.RFC3339))

	// 税前 4 分：4×10%=0.4 分，不足半分舍去，税额 0，应付 4 分。
	down, err := s.CreateBill("acct-round-down", jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill down   plan=%s subtotal=%d taxRateBasisPoints=%d tax=%d totalDue=%d\n",
		down.Terms.PlanID, down.MonthlyFee+down.OverageFee,
		down.Terms.TaxRateBasisPoints, down.Tax, down.TotalDue)

	// 税前 5 分：5×10%=0.5 分，恰好半分向上，税额 1，应付 6 分。
	half, err := s.CreateBill("acct-round-half", jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill half   plan=%s subtotal=%d taxRateBasisPoints=%d tax=%d totalDue=%d\n",
		half.Terms.PlanID, half.MonthlyFee+half.OverageFee,
		half.Terms.TaxRateBasisPoints, half.Tax, half.TotalDue)

	// Output:
	// event over   accepted=true period=2026-01
	// usage over   period=2026-01 total=3
	// event within accepted=true period=2026-01
	// usage within period=2026-01 total=2
	// bill over   plan=plan-small monthlyFee=3 includedUnits=2 overageUnits=1 overageFee=3 subtotal=6 taxRateBasisPoints=1000 tax=1 totalDue=7 dueAt=2026-02-08T00:00:00Z
	// bill within plan=plan-small monthlyFee=3 includedUnits=2 overageUnits=0 overageFee=0 subtotal=3 taxRateBasisPoints=1000 tax=0 totalDue=3 dueAt=2026-02-08T00:00:00Z
	// bill down   plan=plan-round-down subtotal=4 taxRateBasisPoints=1000 tax=0 totalDue=4
	// bill half   plan=plan-round-half subtotal=5 taxRateBasisPoints=1000 tax=1 totalDue=6
}
