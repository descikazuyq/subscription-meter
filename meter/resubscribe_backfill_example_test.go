package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_resubscribeBackfillOldBill 演示“订阅按月终止后重新开通，
// 再补开旧账单”的完整过程，重点是两张账单各自的套餐条件从哪一次开通来。
//
// 时间线（全部 UTC，同一账户 acct-resub、同一套餐标识 plan-a）：
//
//	2026-01-01 00:00 首次开通：plan-a 当时为月费 1000 分、包含 10 单位、
//	           超额单价 100 分、税率 10%；这次开通保存的快照就是一月的
//	           计费条件。
//	1 月上报 15 单位；2026-01-15 登记按月取消，2026-02-01 00:00 终止，
//	           一月暂不出账。
//	2026-02 整月没有任何订阅（空档）。期间用 UpdatePlan 把同一套餐标识
//	           plan-a 的定义改为月费 2000 分、包含 20 单位、超额单价 200
//	           分、税率 6%——只影响之后新保存的快照。
//	2026-03-15 12:00 用同一个 Subscribe 入口、同一个套餐标识重新开通：
//	           新订阅保存的是重新开通当时的新条件快照，三月按完整月费与
//	           完整额度计费，月中开通不折算。
//	三月上报 25 单位（接收时没有到期未结清账单：一月尚未出账）。
//	2026-04-01 00:00 先为三月出账，再补开一月账单：
//	           三月超额 5、超额费 1000、税 180、应付 3180；
//	           一月仍按首次开通的旧条件：超额费 500、税 150、应付 1650。
//
// 示例同时锁定四条边界：
//   - 二月空档为二月出账返回 ErrBillBeforeSubscription（这个错误不只表示
//     “早于第一次开通”，两段订阅之间完全无订阅的空档月份同样适用），随后
//     查询得到 ErrBillNotFound；三月重新开通、四月补账之后仍然如此。
//   - 重新开通沿用同一个套餐标识不等于沿用旧价格：当前条件是重新开通时
//     保存的 2000/20/200/600，一月旧账单仍按 1000/10/100/1000。
//   - 两张账单的付款截止分别固定为 2026-02-08 与 2026-04-08 00:00 UTC，
//     从各自账期结束算起，不随四月出账顺延；补开一月账单时其截止时刻早已
//     过去，账户当即因欠费停用——补开旧账可能立刻带来停用。
//   - 补开旧账单不改变当前套餐：补开后查询当前生效条件仍是新价格。
//
// 正常步骤失败即 panic 停止；空档月份的两个错误是预期结果，代码显式判断
// 并打印，与正常步骤的失败明确区分。示例通过 NewServiceWithClock 注入可
// 推进的时钟，输出不依赖运行当天日期。
func ExampleService_resubscribeBackfillOldBill() {
	const accountID = "acct-resub"
	jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))
	feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))
	mar := meter.MonthOf(mustParseTime("2026-03-01T00:00:00Z"))

	// 可推进的时钟：初始当前时刻为 2026-01-01 00:00 UTC，之后逐段推进，
	// 复制后在任何真实日期运行结果都相同。
	now := mustParseTime("2026-01-01T00:00:00Z")
	s := meter.NewServiceWithClock(func() time.Time { return now })

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// plan-a 首次开通时的定义：月费 1000 分、包含 10 单位、超额单价
	// 100 分、税率 10%。一月条件取自这一次开通保存的快照。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-a", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	// 2026-01-01 00:00 UTC 首次开通。
	if err := s.Subscribe(accountID, "plan-a", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 一月中旬上报 15 单位，超出包含的 10 单位；一月暂不出账，因此此时
	// 不存在到期未结清账单，用量正常接收。
	now = mustParseTime("2026-01-15T12:00:00Z")
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-jan",
		At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 15,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-jan  accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 1 月 15 日登记按月取消：终止时刻为 2026-02-01 00:00 UTC，一月仍按
	// 完整月费与额度计费。
	r, err := s.CancelSubscription(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("cancel       cancelled=%t endAt=%s\n",
		r.Cancelled, r.Cancellation.EndAt.Format(time.RFC3339))

	// 推进到终止时刻：订阅已终止，空档期间查询不显示任何生效套餐，也没有
	// 待取消终止时刻；历史用量继续保留。
	now = mustParseTime("2026-02-01T00:00:00Z")
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("gap status   subscribed=%t currentPlan=%q scheduledEnd=%v\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.ScheduledEnd)

	// 二月整月空档期间，把同一个套餐标识 plan-a 的定义改为新价格：月费
	// 2000 分、包含 20 单位、超额单价 200 分、税率 6%。修改只影响之后
	// 新保存的条件快照，不改写一月旧订阅已经保存的快照。
	now = mustParseTime("2026-02-15T12:00:00Z")
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-a", MonthlyFee: 2000, IncludedUnits: 20,
		OveragePrice: 200, TaxRateBasisPoints: 600,
	}); err != nil {
		panic(err)
	}
	fmt.Printf("plan updated id=plan-a monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600\n")

	// 三月一日二月账期已结束：为空档月份二月出账返回 ErrBillBeforeSubscription。
	// 该错误不只表示“早于第一次开通”——两段订阅之间完全无订阅覆盖的空档
	// 月份同样按它拒绝；这次失败不保存账单，随后查询得到 ErrBillNotFound。
	now = mustParseTime("2026-03-01T00:00:00Z")
	_, err = s.CreateBill(accountID, feb)
	fmt.Printf("feb in gap   errBillBeforeSubscription=%t\n",
		errors.Is(err, meter.ErrBillBeforeSubscription))
	_, err = s.GetBill(accountID, feb)
	fmt.Printf("feb in gap   errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))
	// 空档月份没有任何用量记录，单月查询为零。
	usage, err := s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("feb usage    period=%s total=%d\n", usage.Period, usage.Total)

	// 2026-03-15 12:00 UTC 用同一个 Subscribe 入口、同一个套餐标识
	// plan-a 重新开通。沿用标识不代表沿用旧价格：本次开通保存的是重新
	// 开通当时的完整条件 2000/20/200/600；月中开通也按完整月费与完整
	// 额度计三月，不按天折算。
	now = mustParseTime("2026-03-15T12:00:00Z")
	if err := s.Subscribe(accountID, "plan-a", now); err != nil {
		panic(err)
	}
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("resubscribe  subscribed=%t plan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
		st.CurrentTerms.TaxRateBasisPoints)

	// 三月上报 25 单位，事件发生在重新开通之后、属于已生效的新订阅期间；
	// 接收时一月尚未出账，没有到期未结清账单，不会被欠费停用拦截。
	ev, err = s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-mar",
		At: mustParseTime("2026-03-15T12:00:00Z"), Quantity: 25,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-mar  accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 推进到 2026-04-01 00:00 UTC：三月账期结束。此时还没有任何账单，
	// 账户未停用，当前套餐是重新开通保存的新条件。
	now = mustParseTime("2026-04-01T00:00:00Z")
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("before bills subscribed=%t suspended=%t currentPlan=%s monthlyFee=%d\n",
		st.Subscribed, st.Suspended, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee)

	// 先为三月出账：按重新开通时保存的条件收完整月费、给完整额度。
	// 用量 25、超额 25-20=5；超额费 5×200=1000；
	// 税 (2000+1000)×6%=180；应付 3180。付款截止为三月结束后七天，
	// 即 2026-04-08 00:00 UTC（此刻尚未到期）。
	marBill, err := s.CreateBill(accountID, mar)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill mar     period=%s plan=%s monthlyFee=%d includedUnits=%d totalUsage=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		marBill.Period, marBill.Terms.PlanID, marBill.MonthlyFee, marBill.IncludedUnits,
		marBill.TotalUsage, marBill.OverageUnits, marBill.OverageFee,
		marBill.Tax, marBill.TotalDue, marBill.DueAt.Format(time.RFC3339))
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after mar    suspended=%t\n", st.Suspended)

	// 再补开一月账单：账期归当时覆盖它的旧订阅段，条件取自 2026-01-01
	// 首次开通保存的快照（1000/10/100/1000），不能套用重新开通时的新价格。
	// 用量 15、超额 5×100=500；税 (1000+500)×10%=150；应付 1650。
	// 付款截止固定为一月结束后七天，即 2026-02-08 00:00 UTC——四月补账
	// 不顺延。该瞬间早已过去且账单有余额，补账后账户当即欠费停用。
	janBill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill jan     period=%s plan=%s monthlyFee=%d includedUnits=%d totalUsage=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		janBill.Period, janBill.Terms.PlanID, janBill.MonthlyFee, janBill.IncludedUnits,
		janBill.TotalUsage, janBill.OverageUnits, janBill.OverageFee,
		janBill.Tax, janBill.TotalDue, janBill.DueAt.Format(time.RFC3339))
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after jan    suspended=%t currentPlan=%s monthlyFee=%d taxRateBasisPoints=%d\n",
		st.Suspended, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.TaxRateBasisPoints)

	// 三月重新开通不能把空档二月变成可出账月份：补开一月之后再试二月，
	// 仍是 ErrBillBeforeSubscription，查询仍是 ErrBillNotFound。
	_, err = s.CreateBill(accountID, feb)
	fmt.Printf("feb backfill errBillBeforeSubscription=%t\n",
		errors.Is(err, meter.ErrBillBeforeSubscription))
	_, err = s.GetBill(accountID, feb)
	fmt.Printf("feb backfill errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))

	// 两张账单的查询结果与出账结果完全一致：账期、套餐条件、用量、金额与
	// 付款截止都保持出账时的取值。
	marAgain, err := s.GetBill(accountID, mar)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get mar      period=%s plan=%s monthlyFee=%d includedUnits=%d totalUsage=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		marAgain.Period, marAgain.Terms.PlanID, marAgain.MonthlyFee, marAgain.IncludedUnits,
		marAgain.TotalUsage, marAgain.OverageUnits, marAgain.OverageFee,
		marAgain.Tax, marAgain.TotalDue, marAgain.DueAt.Format(time.RFC3339))
	janAgain, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get jan      period=%s plan=%s monthlyFee=%d includedUnits=%d totalUsage=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		janAgain.Period, janAgain.Terms.PlanID, janAgain.MonthlyFee, janAgain.IncludedUnits,
		janAgain.TotalUsage, janAgain.OverageUnits, janAgain.OverageFee,
		janAgain.Tax, janAgain.TotalDue, janAgain.DueAt.Format(time.RFC3339))

	// 账单摘要按账期排列，只含一月与三月（空档二月不在其中）；两张账单都
	// 尚未付款，余额等于应付。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	for i, b := range st.Bills {
		fmt.Printf("status bill[%d] period=%s totalDue=%d balance=%d settled=%t dueAt=%s\n",
			i, b.Period, b.TotalDue, b.Balance, b.Settled, b.DueAt.Format(time.RFC3339))
	}
	// 补开旧账单没有把当前套餐改回旧价格：当前生效条件仍是重新开通时保存
	// 的 2000/20/200/600；订阅仍生效，但账户因一月旧账已过截止而停用。
	fmt.Printf("status       subscribed=%t suspended=%t currentPlan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d\n",
		st.Subscribed, st.Suspended, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
		st.CurrentTerms.TaxRateBasisPoints)

	// Output:
	// event e-jan  accepted=true period=2026-01
	// cancel       cancelled=true endAt=2026-02-01T00:00:00Z
	// gap status   subscribed=false currentPlan="" scheduledEnd=<nil>
	// plan updated id=plan-a monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600
	// feb in gap   errBillBeforeSubscription=true
	// feb in gap   errBillNotFound=true
	// feb usage    period=2026-02 total=0
	// resubscribe  subscribed=true plan=plan-a monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600
	// event e-mar  accepted=true period=2026-03
	// before bills subscribed=true suspended=false currentPlan=plan-a monthlyFee=2000
	// bill mar     period=2026-03 plan=plan-a monthlyFee=2000 includedUnits=20 totalUsage=25 overageUnits=5 overageFee=1000 tax=180 totalDue=3180 dueAt=2026-04-08T00:00:00Z
	// after mar    suspended=false
	// bill jan     period=2026-01 plan=plan-a monthlyFee=1000 includedUnits=10 totalUsage=15 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
	// after jan    suspended=true currentPlan=plan-a monthlyFee=2000 taxRateBasisPoints=600
	// feb backfill errBillBeforeSubscription=true
	// feb backfill errBillNotFound=true
	// get mar      period=2026-03 plan=plan-a monthlyFee=2000 includedUnits=20 totalUsage=25 overageUnits=5 overageFee=1000 tax=180 totalDue=3180 dueAt=2026-04-08T00:00:00Z
	// get jan      period=2026-01 plan=plan-a monthlyFee=1000 includedUnits=10 totalUsage=15 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
	// status bill[0] period=2026-01 totalDue=1650 balance=1650 settled=false dueAt=2026-02-08T00:00:00Z
	// status bill[1] period=2026-03 totalDue=3180 balance=3180 settled=false dueAt=2026-04-08T00:00:00Z
	// status       subscribed=true suspended=true currentPlan=plan-a monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600
}
