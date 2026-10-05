package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_lateUsage 演示“用量补报”：账期已经结束但尚未出账时，
// 晚到的用量事件仍可按发生时刻归入已结束的账期；一旦该账期成功出账，
// 新事件被拒绝（ErrMonthBilled），而此前已接收事件的原样重报仍然成功。
// 示例只使用 meter 的公开入口，并通过 NewServiceWithClock 把当前时刻
// 固定在 2026-02-01 00:00 UTC，因此无论实际运行日期为何，输出都固定，
// 不必等到某个真实日期。
//
// 账户 acct-late-usage 自 2026-01-01 00:00 UTC 起持有一份持续有效的
// 订阅（套餐月费 1000 分、无超额、无税），一月已有累计用量 8，当前
// 时刻为一月账期刚结束的 2026-02-01 00:00 UTC，一月尚未出账，账户
// 也没有其他欠款。示例依次展示：补报一条发生在 2026-01-31 23:59:59
// UTC、数量 5 的事件被接收并归入 2026-01，一月累计变为 13；随后为
// 一月出账，账单总用量也是 13；出账后再提交另一标识的一月新事件被
// 拒绝（ErrMonthBilled），累计与账单总用量仍为 13；最后原样重报刚才
// 成功补报的事件，调用成功、Accepted 为 false、累计不再增加。
func ExampleService_lateUsage() {
	const (
		accountID = "acct-late-usage"
		jan1st    = "2026-01-01T00:00:00Z"
		now       = "2026-02-01T00:00:00Z"
	)
	jan := meter.MonthOf(mustParseTime(jan1st))

	// 把“当前时刻”固定在 2026-02-01 00:00 UTC：2026-01 账期恰好结束，
	// 此时提交发生在一月的事件即“补报”；当前时刻之后的任何真实日期
	// 也能得到同样结果。
	s := meter.NewServiceWithClock(func() time.Time {
		return mustParseTime(now)
	})

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐月费恰好 1000 分、无超额、无税：账单 TotalDue 就是 1000 分。
	if err := s.CreatePlan(meter.Plan{
		ID:                 "plan1000",
		MonthlyFee:         1000,
		IncludedUnits:      0,
		OveragePrice:       0,
		TaxRateBasisPoints: 0,
	}); err != nil {
		panic(err)
	}
	// 订阅自 2026-01-01 00:00 UTC 起持续有效，不取消、不换套餐。
	if err := s.Subscribe(accountID, "plan1000", mustParseTime(jan1st)); err != nil {
		panic(err)
	}

	// 一月已有用量 8：一条发生在 1 月 15 日、数量 8 的事件。
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e0",
		At:        mustParseTime("2026-01-15T00:00:00Z"),
		Quantity:  8,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e0    accepted=%t period=%s\n", ev.Accepted, ev.Period)

	usage, err := s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 补报：账期已经结束但尚未出账，发生时刻 2026-01-31 23:59:59 UTC
	// 仍属于实际订阅期间，账户也未停用，事件 e1 被接收并归入 2026-01。
	ev, err = s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e1",
		At:        mustParseTime("2026-01-31T23:59:59Z"),
		Quantity:  5,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e1    accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 一月累计由 8 变为 13。
	usage, err = s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 为一月出账：账单总用量取此时的一月累计，也是 13。
	bill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill        period=%s totalUsage=%d totalDue=%d\n",
		bill.Period, bill.TotalUsage, bill.TotalDue)

	// 出账后再提交另一标识 e2 的一月新事件：ErrMonthBilled。
	// 账单生成后该月累计与账单用量已经固定，晚到的新事件不会
	// 重新计算账单。拒收不消耗事件标识 e2。
	_, err = s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e2",
		At:        mustParseTime("2026-01-20T00:00:00Z"),
		Quantity:  1,
	})
	fmt.Printf("event e2    errMonthBilled=%t\n", errors.Is(err, meter.ErrMonthBilled))

	// 被拒收的事件没有累计：一月用量仍是 13。
	usage, err = s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 账单也不变：总用量与应付金额保持出账时的结果。
	current, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill    period=%s totalUsage=%d totalDue=%d\n",
		current.Period, current.TotalUsage, current.TotalDue)

	// 原样重报刚才成功补报的 e1（账户、标识、时刻、数量完全一致）：
	// 即使在出账之后，调用仍然成功，但 Accepted=false 表示此前已
	// 接收过，本次没有再次累计；Period 仍是 2026-01。
	ev, err = s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e1",
		At:        mustParseTime("2026-01-31T23:59:59Z"),
		Quantity:  5,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e1    accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 重报没有再次累计：一月用量仍是 13。
	usage, err = s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// Output:
	// event e0    accepted=true period=2026-01
	// usage       period=2026-01 total=8
	// event e1    accepted=true period=2026-01
	// usage       period=2026-01 total=13
	// bill        period=2026-01 totalUsage=13 totalDue=1000
	// event e2    errMonthBilled=true
	// usage       period=2026-01 total=13
	// get bill    period=2026-01 totalUsage=13 totalDue=1000
	// event e1    accepted=false period=2026-01
	// usage       period=2026-01 total=13
}
