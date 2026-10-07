package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_futureSubscription 演示提前登记一份开通时刻晚于当前时刻的
// 订阅后，“已经登记”与“已经生效”的区别：登记成功后、实际开通时刻到达前，
// Status 显示没有生效订阅（Subscribed 为 false、CurrentTerms 为零值，亦无
// 待换套餐安排与待取消终止时刻），但登记被保留——等待期间再次开通得到
// ErrSubscriptionExists 并保留原登记；到达实际开通瞬间后直接查询即显示
// 登记时保存的完整套餐条件快照，等待期间对同一套餐的定义修改不会混入。
//
// 主账户 acct-future 的当前时刻固定为 2026-01-15 09:00 UTC，登记在
// 2026-01-20 12:00 UTC 才开通。登记时套餐 plan-a 为月费 1000 分、包含
// 10 单位、超额单价 100 分、税率 600 万分比；登记成功后把同一套餐的定义
// 改为月费 2000 分、包含 20 单位、超额单价 200 分、税率 1000 万分比。
// 等待期间查询显示未生效；再次开通得到 ErrSubscriptionExists，原登记保留。
// 推进到开通当天零点订阅仍未生效；恰好到达 1 月 20 日 12:00 UTC 后直接
// 查询，四项条件全部保持首次登记时的值。另一账户把同一开通瞬间写作
// 1 月 20 日 20:00 的东八区表示并在修改定义前登记，得到相同结果，说明
// 时间判断依据同一瞬间，而不是输入中的本地钟面或月份。
//
// 示例通过 NewServiceWithClock 注入可推进的时钟，输出不依赖运行当天日期。
func ExampleService_futureSubscription() {
	const accountID = "acct-future"

	// 可推进的时钟：初始当前时刻为 2026-01-15 09:00 UTC，之后逐段推进
	// 到一月二十日，复制后在任何真实日期运行结果都相同。
	now := mustParseTime("2026-01-15T09:00:00Z")
	s := meter.NewServiceWithClock(func() time.Time { return now })

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 登记时的套餐定义：月费 1000 分、包含 10 单位、超额单价 100 分、
	// 税率 600 万分比（即 6%）。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-a", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 600,
	}); err != nil {
		panic(err)
	}

	// 提前登记：当前时刻 1 月 15 日 09:00，开通时刻填 1 月 20 日 12:00
	// UTC——开通时刻可以晚于当前时刻，调用成功只表示“已登记”，订阅此刻
	// 还没有生效。
	if err := s.Subscribe(accountID, "plan-a",
		mustParseTime("2026-01-20T12:00:00Z")); err != nil {
		panic(err)
	}

	// 等待期间查询：已登记但未生效——Subscribed=false、CurrentTerms 为
	// 零值，PendingChange 与 ScheduledEnd 均为 nil。看到 false 不能认为
	// 登记丢失而再次开通。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("waiting     subscribed=%t planID=%q monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID,
		st.CurrentTerms.MonthlyFee, st.CurrentTerms.IncludedUnits,
		st.CurrentTerms.OveragePrice, st.CurrentTerms.TaxRateBasisPoints,
		st.PendingChange, st.ScheduledEnd)

	// 等待期间再次为该账户开通：按已有订阅拒绝（ErrSubscriptionExists），
	// 原登记保持不变。
	err = s.Subscribe(accountID, "plan-a",
		mustParseTime("2026-01-20T12:00:00Z"))
	fmt.Printf("duplicate   errSubscriptionExists=%t\n",
		errors.Is(err, meter.ErrSubscriptionExists))

	// 重复开通被拒绝后原登记仍在：状态与等待期间完全相同。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after dup   subscribed=%t planID=%q pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID,
		st.PendingChange, st.ScheduledEnd)

	// 另一个账户把同一开通瞬间写作东八区 2026-01-20 20:00 +0800
	// （== 2026-01-20 12:00 UTC）登记。注意它在下方修改套餐定义之前
	// 登记，因此锁定的条件快照与主账户完全相同；之后修改定义不影响它。
	plus8 := time.FixedZone("UTC+8", 8*60*60)
	if err := s.CreateAccount("acct-future-tz"); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-future-tz", "plan-a",
		time.Date(2026, time.January, 20, 20, 0, 0, 0, plus8)); err != nil {
		panic(err)
	}

	// 等待期间修改同一套餐的定义：月费、包含单位、超额单价、税率全部
	// 改变。修改只影响之后新保存的条件快照，不改写两份已登记订阅在
	// 登记时锁定的快照。
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-a", MonthlyFee: 2000, IncludedUnits: 20,
		OveragePrice: 200, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}

	// 定义改完订阅仍未生效，状态没有任何变化。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after update subscribed=%t planID=%q monthlyFee=%d pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.PendingChange, st.ScheduledEnd)

	// 推进到开通当天零点（2026-01-20 00:00 UTC）：虽然已进入开通月份，
	// 但未到登记的开通时刻，订阅仍不生效——不按开通月份提前到月初。
	now = mustParseTime("2026-01-20T00:00:00Z")
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("midnight    subscribed=%t planID=%q pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID,
		st.PendingChange, st.ScheduledEnd)

	// 开通瞬间前一分钟（东八区 19:59 == UTC 11:59）：用本地钟面观察，
	// 仍未生效——判定依据是同一瞬间，不看输入用了哪个时区的钟面。
	now = time.Date(2026, time.January, 20, 19, 59, 0, 0, plus8)
	st, err = s.Status("acct-future-tz")
	if err != nil {
		panic(err)
	}
	fmt.Printf("tz 19:59    subscribed=%t planID=%q pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID,
		st.PendingChange, st.ScheduledEnd)

	// 恰好到达 1 月 20 日 12:00 UTC：直接查询即显示订阅已生效，无须先
	// 上报用量或生成账单；四项计费条件全部是首次登记时的值（1000/10/
	// 100/600），等待期间改成的 2000/20/200/1000 没有混入。
	now = mustParseTime("2026-01-20T12:00:00Z")
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("activated   subscribed=%t planID=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID,
		st.CurrentTerms.MonthlyFee, st.CurrentTerms.IncludedUnits,
		st.CurrentTerms.OveragePrice, st.CurrentTerms.TaxRateBasisPoints,
		st.PendingChange, st.ScheduledEnd)

	// 把时钟改用东八区表示同一瞬间（本地 20:00 == UTC 12:00）：以东八区
	// 钟面登记的账户同样生效，条件快照与 UTC 登记的主账户逐项相同。
	now = time.Date(2026, time.January, 20, 20, 0, 0, 0, plus8)
	st, err = s.Status("acct-future-tz")
	if err != nil {
		panic(err)
	}
	fmt.Printf("tz 20:00    subscribed=%t planID=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID,
		st.CurrentTerms.MonthlyFee, st.CurrentTerms.IncludedUnits,
		st.CurrentTerms.OveragePrice, st.CurrentTerms.TaxRateBasisPoints,
		st.PendingChange, st.ScheduledEnd)

	// Output:
	// waiting     subscribed=false planID="" monthlyFee=0 includedUnits=0 overagePrice=0 taxRateBasisPoints=0 pendingChange=<nil> scheduledEnd=<nil>
	// duplicate   errSubscriptionExists=true
	// after dup   subscribed=false planID="" pendingChange=<nil> scheduledEnd=<nil>
	// after update subscribed=false planID="" monthlyFee=0 pendingChange=<nil> scheduledEnd=<nil>
	// midnight    subscribed=false planID="" pendingChange=<nil> scheduledEnd=<nil>
	// tz 19:59    subscribed=false planID="" pendingChange=<nil> scheduledEnd=<nil>
	// activated   subscribed=true planID=plan-a monthlyFee=1000 includedUnits=10 overagePrice=100 taxRateBasisPoints=600 pendingChange=<nil> scheduledEnd=<nil>
	// tz 20:00    subscribed=true planID=plan-a monthlyFee=1000 includedUnits=10 overagePrice=100 taxRateBasisPoints=600 pendingChange=<nil> scheduledEnd=<nil>
}
