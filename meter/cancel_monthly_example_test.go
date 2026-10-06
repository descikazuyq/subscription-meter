package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_cancelMonthlySubscription 把按月取消的三个环节串起来：
// 取消请求（CancelSubscription）、实际终止（到达下一个 UTC 自然月月初）与
// 取消当月的账单（CreateBill）。
//
// 账户 acct-cancel 于 2026-01-01 00:00 UTC 开通一份没有欠费的订阅，套餐
// plan-a 月费 1000 分、每月包含 10 单位、超额每单位 100 分、税率 10%。
// 2026-01-15 登记取消，返回的终止时刻为 2026-02-01 00:00 UTC——终止时刻
// 按请求时刻所在的 UTC 自然月计算，与调用者本地显示的月份无关。等待终止
// 期间订阅仍然有效、保留原套餐条件，状态中可以看到已安排的终止时刻；再次
// 取消不会新建安排（Cancelled=false），仍返回原终止时刻，期限不后移，这
// 不是取消失败。登记取消后一月仍照常接收用量，累计达到 15 单位。
//
// 到达 2026-02-01 00:00 UTC 后直接查询即显示订阅已终止、当前套餐为空、
// 不再展示终止安排，不需要先提交事件或生成账单；恰在终止时刻发生的新用量
// 不属于旧订阅，被拒绝且不增加累计。之后仍可为一月出账：取消当月照收完整
// 月费、提供完整额度，不按天退款，取消也不会自动生成账单；一月用量 15、
// 超额费用 500 分、税额 150 分、应付 1650 分，付款截止仍为 2026-02-08
// 00:00 UTC，取消不免除应付款项，历史用量与账单继续可查。
//
// 示例末尾另用一个账户演示边界：提前登记但尚未到实际开通时刻的订阅不能
// 取消，返回 ErrSubscriptionNotActivated。示例通过 NewServiceWithClock
// 注入时钟，在任何真实日期运行输出都相同。
func ExampleService_cancelMonthlySubscription() {
	const accountID = "acct-cancel"
	jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))
	feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))

	// 可推进的时钟：初始当前时刻为 2026-01-15 12:00 UTC（一月中旬），
	// 之后推进到二月月初；复制后在任何真实日期运行结果都相同。
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
	// 订阅于 2026-01-01 00:00 UTC 开通，开通时保存套餐条件快照。
	if err := s.Subscribe(accountID, "plan-a", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 1 月 15 日登记按月取消：终止时刻是请求时刻的下一个 UTC 自然月月初
	// 零点，即 2026-02-01 00:00 UTC。该时刻只按请求瞬间所在的 UTC 自然月
	// 计算，不能按调用者本地显示的月份解释（例如本地已是另一个日期时，
	// 仍以 UTC 为准）。
	r, err := s.CancelSubscription(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("cancel       cancelled=%t endAt=%s\n",
		r.Cancelled, r.Cancellation.EndAt.Format(time.RFC3339))

	// 等待终止期间查询：订阅仍有效，当前仍是开通时保存的原套餐完整条件，
	// 并能看到已安排的终止时刻。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status jan15 subscribed=%t plan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d scheduledEnd=%s\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
		st.CurrentTerms.TaxRateBasisPoints, st.ScheduledEnd.Format(time.RFC3339))

	// 等待期间再次取消：本次没有新建安排（Cancelled=false），仍返回原终止
	// 时刻。这不是取消失败，也不会把终止期限后移。
	again, err := s.CancelSubscription(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("cancel again cancelled=%t endAt=%s\n",
		again.Cancelled, again.Cancellation.EndAt.Format(time.RFC3339))

	// 推进到 1 月 28 日（仍在等待终止期间）：登记取消后，一月仍照常接收
	// 用量。两条事件合计 15 单位，全部属于 2026-01 账期，超出套餐包含的
	// 10 单位。
	now = mustParseTime("2026-01-28T00:00:00Z")
	for _, ev := range []meter.Event{
		{AccountID: accountID, EventID: "e1", At: mustParseTime("2026-01-16T00:00:00Z"), Quantity: 8},
		{AccountID: accountID, EventID: "e2", At: mustParseTime("2026-01-28T00:00:00Z"), Quantity: 7},
	} {
		res, err := s.RecordEvent(ev)
		if err != nil {
			panic(err)
		}
		fmt.Printf("event %-5s accepted=%t period=%s\n", ev.EventID, res.Accepted, res.Period)
	}
	usage, err := s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage        period=%s total=%d\n", usage.Period, usage.Total)

	// 推进到终止时刻 2026-02-01 00:00 UTC：直接查询即显示订阅已终止、
	// 当前套餐为空、待生效换套餐与终止安排都不再展示。无需先提交事件或
	// 生成账单来“促成”终止。
	now = mustParseTime("2026-02-01T00:00:00Z")
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status feb1  subscribed=%t plan=%s monthlyFee=%d suspended=%t pending=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.Suspended, st.PendingChange, st.ScheduledEnd)

	// 没有重新开通：恰在终止时刻发生的新用量不属于旧订阅（终止时刻不计入
	// 订阅期间），被拒绝且不增加累计。
	_, err = s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-end",
		At: mustParseTime("2026-02-01T00:00:00Z"), Quantity: 1,
	})
	fmt.Printf("event at-end errEventBeforeSubscription=%t\n",
		errors.Is(err, meter.ErrEventBeforeSubscription))
	usage, err = s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage        period=%s total=%d\n", usage.Period, usage.Total)

	// 历史用量在终止后继续可查：一月累计仍是 15。
	usage, err = s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("history      period=%s total=%d\n", usage.Period, usage.Total)

	// 为一月出账：取消当月仍收完整月费、提供完整额度，不按天退款；取消也
	// 不会自动生成账单，需要显式调用 CreateBill。用量 15，超额
	// 5 × 100 = 500 分；税 (1000+500) × 10% = 150 分；应付 1650 分。
	janBill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill jan     plan=%s totalUsage=%d monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		janBill.Terms.PlanID, janBill.TotalUsage, janBill.MonthlyFee,
		janBill.IncludedUnits, janBill.OverageUnits, janBill.OverageFee,
		janBill.Tax, janBill.TotalDue, janBill.DueAt.Format(time.RFC3339))

	// 取消不免除应付款项：账单余额仍是 1650 分、尚未结清，付款截止仍是
	// 账期结束后七天，即 2026-02-08 00:00 UTC，不因取消而改变。
	current, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill     totalDue=%d balance=%d settled=%t dueAt=%s\n",
		current.TotalDue, current.Balance, current.Settled,
		current.DueAt.Format(time.RFC3339))

	// 边界：另一个账户提前登记了 2026-03-01 00:00 UTC 才开通的订阅，
	// 当前时刻（2026-02-20 UTC）尚未到达实际开通时刻。查询不显示有效
	// 订阅，也没有终止安排；此时登记取消返回订阅尚未生效的错误。
	const futureAccount = "acct-cancel-future"
	futureNow := mustParseTime("2026-02-20T00:00:00Z")
	s2 := meter.NewServiceWithClock(func() time.Time { return futureNow })
	if err := s2.CreateAccount(futureAccount); err != nil {
		panic(err)
	}
	if err := s2.CreatePlan(meter.Plan{
		ID: "plan-a", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	if err := s2.Subscribe(futureAccount, "plan-a", mustParseTime("2026-03-01T00:00:00Z")); err != nil {
		panic(err)
	}
	early, err := s2.Status(futureAccount)
	if err != nil {
		panic(err)
	}
	fmt.Printf("early status subscribed=%t plan=%s scheduledEnd=%v\n",
		early.Subscribed, early.CurrentTerms.PlanID, early.ScheduledEnd)
	_, err = s2.CancelSubscription(futureAccount)
	fmt.Printf("early cancel errSubscriptionNotActivated=%t\n",
		errors.Is(err, meter.ErrSubscriptionNotActivated))

	// Output:
	// cancel       cancelled=true endAt=2026-02-01T00:00:00Z
	// status jan15 subscribed=true plan=plan-a monthlyFee=1000 includedUnits=10 overagePrice=100 taxRateBasisPoints=1000 scheduledEnd=2026-02-01T00:00:00Z
	// cancel again cancelled=false endAt=2026-02-01T00:00:00Z
	// event e1    accepted=true period=2026-01
	// event e2    accepted=true period=2026-01
	// usage        period=2026-01 total=15
	// status feb1  subscribed=false plan= monthlyFee=0 suspended=false pending=<nil> scheduledEnd=<nil>
	// event at-end errEventBeforeSubscription=true
	// usage        period=2026-02 total=0
	// history      period=2026-01 total=15
	// bill jan     plan=plan-a totalUsage=15 monthlyFee=1000 includedUnits=10 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
	// get bill     totalDue=1650 balance=1650 settled=false dueAt=2026-02-08T00:00:00Z
	// early status subscribed=false plan= scheduledEnd=<nil>
	// early cancel errSubscriptionNotActivated=true
}
