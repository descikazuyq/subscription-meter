package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_lateUsage 演示“用量补报”：账期结束之后，在出账前后提交的
// 晚到用量分别得到什么结果。示例只使用 meter 的公开入口，并通过
// NewServiceWithClock 把提交时刻固定在 2026-02-01 00:00 UTC，因此无论实际
// 运行日期为何，输出都固定，不必等到某个真实日期。
//
// 要区分三个时刻：事件发生时刻（Event.At，决定事件归属哪个 UTC 自然月账期）、
// 提交时刻（调用 RecordEvent 时时钟的当前时刻）与出账时刻（CreateBill 成功）。
// 账期结束只意味着自然月已经翻篇，不等于不能补报：一条首次出现的新事件只要
// 参数合法、发生时刻属于实际订阅期间（开通时刻计入、终止时刻不计入）、所属
// 月份尚未成功出账且账户未因欠费停用，就仍会被接收。
//
// 账户 acct-backfill 自 2026-01-01 00:00 UTC 开通，一月已有用量 8；提交时刻
// 固定在 2026-02-01 00:00 UTC，一月尚未出账、没有其他欠款。补报一条发生在
// 2026-01-31 23:59:59 UTC、数量 5 的事件：按发生时刻归入 2026-01，一月累计
// 变为 13。随后为一月出账，账单总用量也是 13；账单成功生成后，该月累计用量
// 与账单中的用量即固定下来，晚到的新事件得到 ErrMonthBilled，账单不会重新
// 计算。最后原样重报补报成功的事件：调用成功但 Accepted=false、Period 仍是
// 2026-01，累计不再增加——Accepted=false 表示“此前已经接收过，本次没有再次
// 累计”，不是失败；而因出账被拒绝的新事件再次提交仍然得到 ErrMonthBilled，
// 它从未被接收，不能被当作成功事件的重报。
func ExampleService_lateUsage() {
	const (
		accountID = "acct-backfill"
		jan1st    = "2026-01-01T00:00:00Z"
		now       = "2026-02-01T00:00:00Z"
	)
	jan := meter.MonthOf(mustParseTime(jan1st))

	// 把“当前时刻”（提交时刻）固定在 2026-02-01 00:00 UTC：2026-01 账期
	// 已经结束，但尚未出账。事件归属只看发生时刻，与提交时刻落在哪一天无关。
	s := meter.NewServiceWithClock(func() time.Time {
		return mustParseTime(now)
	})

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 示例只关注用量，套餐取零超额、零税即可。
	if err := s.CreatePlan(meter.Plan{
		ID:                 "plan1000",
		MonthlyFee:         1000,
		IncludedUnits:      0,
		OveragePrice:       0,
		TaxRateBasisPoints: 0,
	}); err != nil {
		panic(err)
	}
	// 订阅期间自 2026-01-01 00:00 UTC 起持续有效；补报事件的发生时刻
	// 必须落在这段实际订阅期间内（开通计入、终止不计入）。
	if err := s.Subscribe(accountID, "plan1000", mustParseTime(jan1st)); err != nil {
		panic(err)
	}

	// 一月已有用量 8：发生在一月内、此前已接收。
	ev8, err := s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "jan-used-8",
		At:        mustParseTime("2026-01-20T00:00:00Z"),
		Quantity:  8,
	})
	if err != nil {
		panic(err)
	}
	u, err := s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("seed usage  accepted=%t period=%s total=%d\n",
		ev8.Accepted, ev8.Period, u.Total)

	// 补报一条晚到的新事件：发生时刻是 2026-01-31 23:59:59 UTC（仍在一月、
	// 仍在订阅期间内），提交时刻已是 2026-02-01 00:00 UTC。一月账期虽已
	// 结束但尚未出账、账户也没有欠费停用，因此本次仍被接收。
	late := meter.Event{
		AccountID: accountID,
		EventID:   "late-jan-31-5",
		At:        mustParseTime("2026-01-31T23:59:59Z"),
		Quantity:  5,
	}
	ev, err := s.RecordEvent(late)
	if err != nil {
		panic(err)
	}
	fmt.Printf("backfill    accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 按发生时刻归入 2026-01：一月累计由 8 变为 13。
	u, err = s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", u.Period, u.Total)

	// 现在才为一月出账：账单总用量取出账时已经累计到的 13。
	// 提交时刻晚于发生时刻不影响归属，也不影响账单金额。
	bill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill        period=%s totalUsage=%d\n", bill.Period, bill.TotalUsage)

	// 出账成功后再提交另一条一月新事件：一月已经封闭，得到
	// ErrMonthBilled。被拒收不会累计，也不会把该标识登记成“已接收”。
	another := meter.Event{
		AccountID: accountID,
		EventID:   "late-jan-30-2",
		At:        mustParseTime("2026-01-30T12:00:00Z"),
		Quantity:  2,
	}
	_, err = s.RecordEvent(another)
	fmt.Printf("new event   errMonthBilled=%t\n", errors.Is(err, meter.ErrMonthBilled))

	// 原样再次提交这条刚被拒绝的事件：仍然是 ErrMonthBilled。它此前从未
	// 被接收过，重复提交不会让它变成“成功事件的幂等重报”。
	_, err = s.RecordEvent(another)
	fmt.Printf("retry       errMonthBilled=%t\n", errors.Is(err, meter.ErrMonthBilled))

	// 被拒新事件不改变任何数字：一月累计与账单总用量仍为 13。
	u, err = s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", u.Period, u.Total)
	got, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill    totalUsage=%d\n", got.TotalUsage)

	// 最后原样重报此前补报成功的事件（账户、标识、发生时刻、数量完全一致）：
	// 调用成功，但 Accepted=false 表示“此前已经接收过同一事件、本次没有再次
	// 累计”，这是幂等返回而不是失败；Period 仍是按发生时刻归属的 2026-01。
	replay, err := s.RecordEvent(late)
	if err != nil {
		panic(err)
	}
	fmt.Printf("replay      accepted=%t period=%s\n", replay.Accepted, replay.Period)

	// 累计不会增加，账单中的用量也保持出账时固定下来的 13：晚到事件不会
	// 让已成功生成的账单重新计算。
	u, err = s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", u.Period, u.Total)
	got, err = s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill    totalUsage=%d\n", got.TotalUsage)

	// Output:
	// seed usage  accepted=true period=2026-01 total=8
	// backfill    accepted=true period=2026-01
	// usage       period=2026-01 total=13
	// bill        period=2026-01 totalUsage=13
	// new event   errMonthBilled=true
	// retry       errMonthBilled=true
	// usage       period=2026-01 total=13
	// get bill    totalUsage=13
	// replay      accepted=false period=2026-01
	// usage       period=2026-01 total=13
	// get bill    totalUsage=13
}
