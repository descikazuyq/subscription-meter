package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_cancelSubscription 演示按月取消订阅的完整过程：登记取消、
// 等待终止期间的状态与用量接收、到达终止时刻后的状态翻转，以及取消当月的
// 出账。
//
// 账户 acct-cancel 2026-01-01 00:00 UTC 开通 plan-a（月费 1000 分、包含
// 10 单位、超额单价 100 分、税率 10%），没有欠费。2026-01-15 登记取消，
// 终止时刻为请求时刻的下一个 UTC 自然月月初零点：2026-02-01 00:00 UTC。
// 等待期间查询仍显示订阅有效、保留原套餐条件并展示终止时刻；再次取消返回
// 原终止时刻（Cancelled=false），不是失败，也不后移期限。登记取消后一月
// 用量照常接收，累计 15 单位。到达 2026-02-01 00:00 UTC 后直接查询即显示
// 订阅已终止、当前套餐为空、不再展示终止安排，无须先上报用量或生成账单。
// 为一月出账：仍收完整月费、给完整额度，用量 15、超额费用 500 分、税额
// 150 分、应付 1650 分，付款截止仍为 2026-02-08 00:00 UTC；取消不按天
// 退款、不免除应付款项、不自动生成账单，历史用量与账单继续可查。
//
// 示例还展示两处边界：提前登记但尚未到实际开通时刻的订阅不能取消
// （ErrSubscriptionNotActivated）；没有重新开通时，恰在终止时刻发生的新
// 用量不属于旧订阅，被拒绝（ErrEventBeforeSubscription）且不增加累计。
//
// 示例通过 NewServiceWithClock 注入可推进的时钟，输出不依赖运行当天日期。
func ExampleService_cancelSubscription() {
	const accountID = "acct-cancel"
	jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))
	feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))

	// 可推进的时钟：初始当前时刻为 2026-01-15 12:00 UTC（一月中旬），
	// 之后逐段推进到二月，复制后在任何真实日期运行结果都相同。
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
	// 账户 2026-01-01 00:00 UTC 开通，当前无欠费。
	if err := s.Subscribe(accountID, "plan-a", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 1 月 15 日登记按月取消：终止时刻为请求时刻的下一个 UTC 自然月
	// 月初零点，即 2026-02-01 00:00 UTC。
	r, err := s.CancelSubscription(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("cancel      cancelled=%t endAt=%s\n",
		r.Cancelled, r.Cancellation.EndAt.Format(time.RFC3339))

	// 登记取消后、到达终止时刻前：查询仍显示订阅有效、保留原套餐条件，
	// 并展示已安排的终止时刻。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status      subscribed=%t current=%s monthlyFee=%d scheduledEnd=%s\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.ScheduledEnd.Format(time.RFC3339))

	// 等待终止期间再次取消：本次没有新建安排（Cancelled=false），仍返回
	// 原终止时刻。这不是取消失败，也不会把期限后移。
	now = mustParseTime("2026-01-20T09:00:00Z")
	r, err = s.CancelSubscription(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("recancel    cancelled=%t endAt=%s\n",
		r.Cancelled, r.Cancellation.EndAt.Format(time.RFC3339))

	// 取消登记后仍能接收一月的用量：一月累计达到 15 单位。
	now = mustParseTime("2026-01-25T12:00:00Z")
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-jan",
		At: mustParseTime("2026-01-25T00:00:00Z"), Quantity: 15,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-jan accepted=%t period=%s\n", ev.Accepted, ev.Period)

	usage, err := s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 终止时刻按请求时刻所在的 UTC 自然月计算，不能按调用者看到的本地
	// 月份解释：当前时刻用 UTC+8 表示为 2026-02-01 07:30（本地钟面已
	// 跨入二月），实际仍是 UTC 2026-01-31 23:30，终止时刻仍取 UTC
	// 一月的下一月月初 2026-02-01 00:00 UTC，而不是按本地二月再往后
	// 推一个月。
	plus8 := time.FixedZone("UTC+8", 8*60*60)
	now = time.Date(2026, time.February, 1, 7, 30, 0, 0, plus8)
	if err := s.CreateAccount("acct-tz"); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-tz", "plan-a", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}
	r, err = s.CancelSubscription("acct-tz")
	if err != nil {
		panic(err)
	}
	fmt.Printf("cancel tz+8 local=%s endAt=%s\n",
		now.Format("2006-01-02 15:04 -0700"), r.Cancellation.EndAt.Format(time.RFC3339))

	// 推进到 2026-02-01 00:00 UTC（终止时刻）：直接查询即显示订阅已
	// 终止、当前套餐为空、不再展示终止安排，无须先上报用量或生成账单。
	now = mustParseTime("2026-02-01T00:00:00Z")
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status      subscribed=%t currentPlan=%q scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.ScheduledEnd)

	// 取消不会自动生成账单：一月账期虽已结束，尚未出账。
	_, err = s.GetBill(accountID, jan)
	fmt.Printf("get bill jan errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))

	// 没有重新开通时，恰在终止时刻发生的新用量不属于旧订阅：被拒绝
	// （ErrEventBeforeSubscription），不增加累计。
	_, err = s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-at-end",
		At: mustParseTime("2026-02-01T00:00:00Z"), Quantity: 1,
	})
	fmt.Printf("event e-at-end errEventBeforeSubscription=%t\n",
		errors.Is(err, meter.ErrEventBeforeSubscription))

	// 被拒绝的事件没有累计：一月仍是 15，二月为零。
	usage, err = s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)
	usage, err = s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 为一月出账：取消当月仍收完整月费、提供完整额度，不按天退款。
	// 用量 15，超额 5 × 100 = 500 分；税 (1000+500) × 10% = 150 分；
	// 应付 1650 分。付款截止仍为账期结束后七天，即 2026-02-08 00:00
	// UTC，取消不会免除应付款项。
	janBill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill jan    plan=%s totalUsage=%d monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		janBill.Terms.PlanID, janBill.TotalUsage, janBill.MonthlyFee,
		janBill.IncludedUnits, janBill.OverageUnits, janBill.OverageFee,
		janBill.Tax, janBill.TotalDue, janBill.DueAt.Format(time.RFC3339))

	// 订阅终止后历史账单仍然可查：应付与余额保持原样。
	janAgain, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill jan plan=%s totalUsage=%d totalDue=%d balance=%d settled=%t\n",
		janAgain.Terms.PlanID, janAgain.TotalUsage, janAgain.TotalDue,
		janAgain.Balance, janAgain.Settled)

	// 边界一：提前登记但尚未到实际开通时刻的订阅不能取消。当前时刻为
	// 2026-02-01 00:00 UTC，acct-future 的开通时刻登记为 2 月 10 日，
	// 取消请求返回订阅尚未生效的错误。
	if err := s.CreateAccount("acct-future"); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-future", "plan-a", mustParseTime("2026-02-10T00:00:00Z")); err != nil {
		panic(err)
	}
	_, err = s.CancelSubscription("acct-future")
	fmt.Printf("cancel future errSubscriptionNotActivated=%t\n",
		errors.Is(err, meter.ErrSubscriptionNotActivated))

	// Output:
	// cancel      cancelled=true endAt=2026-02-01T00:00:00Z
	// status      subscribed=true current=plan-a monthlyFee=1000 scheduledEnd=2026-02-01T00:00:00Z
	// recancel    cancelled=false endAt=2026-02-01T00:00:00Z
	// event e-jan accepted=true period=2026-01
	// usage       period=2026-01 total=15
	// cancel tz+8 local=2026-02-01 07:30 +0800 endAt=2026-02-01T00:00:00Z
	// status      subscribed=false currentPlan="" scheduledEnd=<nil>
	// get bill jan errBillNotFound=true
	// event e-at-end errEventBeforeSubscription=true
	// usage       period=2026-01 total=15
	// usage       period=2026-02 total=0
	// bill jan    plan=plan-a totalUsage=15 monthlyFee=1000 includedUnits=10 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
	// get bill jan plan=plan-a totalUsage=15 totalDue=1650 balance=1650 settled=false
	// cancel future errSubscriptionNotActivated=true
}
