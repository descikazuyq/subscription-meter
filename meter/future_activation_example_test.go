package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_subscribeFutureActivation 演示“已经登记”和“已经生效”
// 的区别：Subscribe 允许登记开通时刻晚于当前时刻的订阅，但在实际开通
// 时刻到达前，状态查询显示没有生效订阅——Subscribed 为 false、
// CurrentTerms 为零值，待换套餐安排与待取消终止时刻也都为空。此时再次
// 开通得到 ErrSubscriptionExists，原登记保留，不要因为 Subscribed 为
// false 就重复开通。订阅以实际开通时刻为界（同一瞬间与时区无关），不按
// 开通月份的月初提前生效；到达登记的开通时刻后直接查询，即显示首次登记
// 时保存的完整套餐条件，等待期间对套餐定义的修改不会混入。
//
// 当前时刻固定为 2026-01-15 09:00 UTC，账户 acct-future 登记在
// 2026-01-20 12:00 UTC 开通 plan-future（登记时月费 1000 分、包含 10
// 单位、超额单价 100 分、税率 600 万分比）。登记成功后把同一套餐改为
// 月费 2000 分、包含 20 单位、超额单价 200 分、税率 1000 万分比；等待
// 期间查询始终显示未生效，重复开通得到 ErrSubscriptionExists。推进到
// 开通当天零点仍未生效；恰好到达 2026-01-20 12:00 UTC 后查询显示已生效，
// 四项计费条件全部保持首次登记时的值。把开通时间写成东八区
// 2026-01-20 20:00（与 UTC 12:00 是同一瞬间）、用东八区时钟观察，得到
// 相同结果。
//
// 示例通过 NewServiceWithClock 注入可推进的时钟，输出不依赖运行当天日期。
func ExampleService_subscribeFutureActivation() {
	// 可推进的时钟：初始当前时刻为 2026-01-15 09:00 UTC，之后逐段
	// 推进到 1 月 20 日零点与开通时刻，复制后在任何真实日期运行结果
	// 都相同。
	now := mustParseTime("2026-01-15T09:00:00Z")
	s := meter.NewServiceWithClock(func() time.Time { return now })

	if err := s.CreateAccount("acct-future"); err != nil {
		panic(err)
	}
	// 套餐 plan-future 首次登记时的定义：月费 1000 分、包含 10 单位、
	// 超额单价 100 分、税率 600（万分比，即 6%）。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-future", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 600,
	}); err != nil {
		panic(err)
	}

	// 提前登记：开通时刻为 2026-01-20 12:00 UTC，晚于当前时刻
	// （1 月 15 日 09:00 UTC）。登记成功只表示“已经登记”，不表示
	// 订阅已经生效。
	if err := s.Subscribe("acct-future", "plan-future",
		mustParseTime("2026-01-20T12:00:00Z")); err != nil {
		panic(err)
	}

	// 登记成功后把同一套餐的定义改为另一组条件：月费 2000 分、包含
	// 20 单位、超额单价 200 分、税率 1000（10%）。修改只影响之后新
	// 保存的条件快照，不改写这份登记已经保存的快照。
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-future", MonthlyFee: 2000, IncludedUnits: 20,
		OveragePrice: 200, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}

	// 等待期间查询：订阅尚未生效。Subscribed 为 false，CurrentTerms
	// 为零值，待换套餐安排与待取消终止时刻均为空。这不是没有登记，
	// 不要据此重复开通。
	st, err := s.Status("acct-future")
	if err != nil {
		panic(err)
	}
	fmt.Printf("waiting     subscribed=%t currentPlan=%q monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
		st.CurrentTerms.TaxRateBasisPoints, st.PendingChange, st.ScheduledEnd)

	// 等待期间再次为该账户开通：按已有订阅拒绝（ErrSubscriptionExists），
	// 原登记保留。该错误是预期结果，显式判断并展示，不能忽略错误后继续
	// 打印“开通成功”。
	err = s.Subscribe("acct-future", "plan-future",
		mustParseTime("2026-01-20T12:00:00Z"))
	fmt.Printf("duplicate   errSubscriptionExists=%t\n",
		errors.Is(err, meter.ErrSubscriptionExists))

	// 重复开通被拒绝后，状态仍是等待中的原登记：没有被覆盖，也没有
	// 提前生效。
	st, err = s.Status("acct-future")
	if err != nil {
		panic(err)
	}
	fmt.Printf("after dup   subscribed=%t currentPlan=%q pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID,
		st.PendingChange, st.ScheduledEnd)

	// 推进到开通当天零点（2026-01-20 00:00 UTC）：开通月份已经开始，
	// 但订阅不按开通月份的月初提前生效，仍未到达登记的开通时刻。
	now = mustParseTime("2026-01-20T00:00:00Z")
	st, err = s.Status("acct-future")
	if err != nil {
		panic(err)
	}
	fmt.Printf("month start subscribed=%t currentPlan=%q\n",
		st.Subscribed, st.CurrentTerms.PlanID)

	// 恰好到达登记的开通时刻 2026-01-20 12:00 UTC：直接查询即显示
	// 订阅已生效，无需先上报用量或生成账单。四项计费条件全部是首次
	// 登记时保存的快照（1000/10/100/600），等待期间修改后的定义
	// （2000/20/200/1000）没有混入；待换套餐安排与终止时刻仍为空。
	now = mustParseTime("2026-01-20T12:00:00Z")
	st, err = s.Status("acct-future")
	if err != nil {
		panic(err)
	}
	fmt.Printf("activated   subscribed=%t currentPlan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d pendingChange=%v scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
		st.CurrentTerms.TaxRateBasisPoints, st.PendingChange, st.ScheduledEnd)

	// 开通时刻按同一瞬间比较，与输入里的本地钟面或月份无关：用东八区
	// 表示，2026-01-20 20:00 +08:00 与 2026-01-20 12:00 UTC 是同一
	// 瞬间。另开一个服务重演同样的场景：套餐仍按首次登记时的定义
	// （1000/10/100/600）创建，开通时刻直接用东八区钟面写入，时钟也
	// 用东八区时刻推进——当地 19:59（即 UTC 11:59）尚未生效，当地
	// 20:00（即 UTC 12:00）这一瞬间生效，四项条件与上面 UTC 账户的
	// 结果完全相同。
	plus8 := time.FixedZone("UTC+8", 8*60*60)
	nowTZ := time.Date(2026, 1, 15, 17, 0, 0, 0, plus8) // == 2026-01-15 09:00 UTC
	s2 := meter.NewServiceWithClock(func() time.Time { return nowTZ })
	if err := s2.CreateAccount("acct-future-tz"); err != nil {
		panic(err)
	}
	if err := s2.CreatePlan(meter.Plan{
		ID: "plan-future", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 600,
	}); err != nil {
		panic(err)
	}
	// 开通时刻用东八区钟面 2026-01-20 20:00 写入：与 12:00 UTC 同一瞬间。
	if err := s2.Subscribe("acct-future-tz", "plan-future",
		time.Date(2026, 1, 20, 20, 0, 0, 0, plus8)); err != nil {
		panic(err)
	}
	// 东八区 1 月 20 日 19:59（UTC 11:59）：同一瞬间尚未到达，不生效。
	nowTZ = time.Date(2026, 1, 20, 19, 59, 0, 0, plus8)
	st2, err := s2.Status("acct-future-tz")
	if err != nil {
		panic(err)
	}
	fmt.Printf("tz waiting  local=%s subscribed=%t currentPlan=%q\n",
		nowTZ.Format("2006-01-02 15:04 -0700"), st2.Subscribed, st2.CurrentTerms.PlanID)
	// 东八区 1 月 20 日 20:00（UTC 12:00）：同一瞬间到达，订阅生效，
	// 四项条件与 UTC 表示的结果完全一致。
	nowTZ = time.Date(2026, 1, 20, 20, 0, 0, 0, plus8)
	st2, err = s2.Status("acct-future-tz")
	if err != nil {
		panic(err)
	}
	fmt.Printf("tz instant  local=%s subscribed=%t currentPlan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d\n",
		nowTZ.Format("2006-01-02 15:04 -0700"), st2.Subscribed, st2.CurrentTerms.PlanID,
		st2.CurrentTerms.MonthlyFee, st2.CurrentTerms.IncludedUnits,
		st2.CurrentTerms.OveragePrice, st2.CurrentTerms.TaxRateBasisPoints)

	// Output:
	// waiting     subscribed=false currentPlan="" monthlyFee=0 includedUnits=0 overagePrice=0 taxRateBasisPoints=0 pendingChange=<nil> scheduledEnd=<nil>
	// duplicate   errSubscriptionExists=true
	// after dup   subscribed=false currentPlan="" pendingChange=<nil> scheduledEnd=<nil>
	// month start subscribed=false currentPlan=""
	// activated   subscribed=true currentPlan=plan-future monthlyFee=1000 includedUnits=10 overagePrice=100 taxRateBasisPoints=600 pendingChange=<nil> scheduledEnd=<nil>
	// tz waiting  local=2026-01-20 19:59 +0800 subscribed=false currentPlan=""
	// tz instant  local=2026-01-20 20:00 +0800 subscribed=true currentPlan=plan-future monthlyFee=1000 includedUnits=10 overagePrice=100 taxRateBasisPoints=600
}
