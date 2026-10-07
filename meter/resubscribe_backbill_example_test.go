package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_resubscribeBackBill 演示“订阅终止后重新开通、再补开旧
// 账单”：旧订阅按月取消并到达终止时刻后，用同一个 Subscribe 入口、同一
// 个套餐标识重新开通；之后先为新月份出账，再补开旧订阅期间尚未出账的
// 月份。
//
// 时间线（全部 UTC，通过 NewServiceWithClock 注入可推进的时钟，输出不
// 依赖运行当天日期）：
//
//   - 2026-01-01 00:00 开通 plan-flex：月费 1000 分、包含 10 单位、
//     超额单价 100 分、税率 10%（1000 万分比）。这次开通保存的条件
//     快照是一月账单的唯一计价依据。
//   - 2026-01-10 上报一月用量 15 单位；2026-01-15 登记按月取消，
//     2026-02-01 00:00 终止。一月暂不出账。
//   - 2026-02 整月空档，期间把 plan-flex 的当前定义改为月费 2000 分、
//     包含 20 单位、超额单价 200 分、税率 6%。
//   - 2026-03-15 00:00 用同一套餐标识重新开通：保存的是改后的条件
//     快照，它成为账户当前使用的套餐条件；2026-03-20 上报三月用量
//     25 单位。所有用量都发生在各自已经生效的订阅期间内，接收用量
//     时账户没有到期未结清账单。
//   - 2026-04-01 00:00 先为三月出账，再补开一月账单。
//
// 两段订阅各保存一份条件快照：当前账户条件与三月账单取自 3 月 15 日
// 重新开通保存的快照（2000/20/200/6%），一月账单取自 1 月 1 日最初
// 开通保存的快照（1000/10/100/10%）。重新开通沿用同一个套餐标识，
// 也不会把新价格套到旧月份；补开旧账单同样不会把当前套餐改回旧价格。
//
// 三月账单按完整月费与完整额度计（月中开通不折算）：用量 25、超额 5、
// 超额费 1000 分、税 180 分、应付 3180 分，截止 2026-04-08 00:00 UTC。
// 一月账单：用量 15、超额 5、超额费 500 分、税 150 分、应付 1650 分，
// 截止仍是 2026-02-08 00:00 UTC——从账期结束算起，不随四月出账顺延；
// 当前时刻已过截止，补开当即带来欠费停用。二月不被任何订阅覆盖：出账
// 返回 ErrBillBeforeSubscription（该错误也用于两段订阅之间的空档月份，
// 不限于早于首次开通的月份），查询返回 ErrBillNotFound。
func ExampleService_resubscribeBackBill() {
	const accountID = "acct-reopen"
	jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))
	feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))
	mar := meter.MonthOf(mustParseTime("2026-03-01T00:00:00Z"))

	// 可推进的时钟：初始当前时刻为 2026-01-01 00:00 UTC，之后沿时间线
	// 逐段推进到 2026-04-01，在任何真实日期运行结果都相同。
	now := mustParseTime("2026-01-01T00:00:00Z")
	s := meter.NewServiceWithClock(func() time.Time { return now })

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐 plan-flex 初始定义：月费 1000 分、包含 10 单位、超额单价
	// 100 分、税率 10%。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan-flex", MonthlyFee: 1000, IncludedUnits: 10,
		OveragePrice: 100, TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	// 2026-01-01 00:00 UTC 首次开通：保存上述条件快照。
	if err := s.Subscribe(accountID, "plan-flex", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 一月用量 15 单位，发生在订阅期间内；此时账户没有任何账单，
	// 更无到期未结清账单。
	now = mustParseTime("2026-01-10T00:00:00Z")
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-jan",
		At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 15,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-jan accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 1 月 15 日登记按月取消：2026-02-01 00:00 UTC 终止。一月暂不出账。
	now = mustParseTime("2026-01-15T12:00:00Z")
	r, err := s.CancelSubscription(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("cancel      cancelled=%t endAt=%s\n",
		r.Cancelled, r.Cancellation.EndAt.Format(time.RFC3339))

	// 二月整月是没有订阅的空档。期间把同一套餐的当前定义改为新价格：
	// 月费 2000 分、包含 20 单位、超额单价 200 分、税率 6%。修改只
	// 影响之后新保存的快照，不改写一月订阅已保存的条件。
	now = mustParseTime("2026-02-10T00:00:00Z")
	if err := s.UpdatePlan(meter.Plan{
		ID: "plan-flex", MonthlyFee: 2000, IncludedUnits: 20,
		OveragePrice: 200, TaxRateBasisPoints: 600,
	}); err != nil {
		panic(err)
	}

	// 3 月 15 日用同一个 Subscribe 入口、同一个套餐标识重新开通：
	// 保存改后的条件快照，它成为账户当前使用的套餐条件。
	now = mustParseTime("2026-03-15T00:00:00Z")
	if err := s.Subscribe(accountID, "plan-flex", mustParseTime("2026-03-15T00:00:00Z")); err != nil {
		panic(err)
	}
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("resubscribe subscribed=%t plan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxBps=%d\n",
		st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
		st.CurrentTerms.TaxRateBasisPoints)

	// 三月用量 25 单位，发生在重新开通后的订阅期间内；此时仍无到期
	// 未结清账单。
	now = mustParseTime("2026-03-20T00:00:00Z")
	ev, err = s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-mar",
		At: mustParseTime("2026-03-20T00:00:00Z"), Quantity: 25,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-mar accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 4 月 1 日先为三月出账：按重新开通时保存的条件收完整月费与完整
	// 额度，月中开通不折算。用量 25、超额 5×200=1000 分、
	// 税 (2000+1000)×6%=180 分、应付 3180 分，截止 2026-04-08 00:00
	// UTC（账期结束后七天）。
	now = mustParseTime("2026-04-01T00:00:00Z")
	marBill, err := s.CreateBill(accountID, mar)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill mar    period=%s plan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxBps=%d\n",
		marBill.Period, marBill.Terms.PlanID, marBill.Terms.MonthlyFee,
		marBill.Terms.IncludedUnits, marBill.Terms.OveragePrice,
		marBill.Terms.TaxRateBasisPoints)
	fmt.Printf("bill mar    totalUsage=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		marBill.TotalUsage, marBill.OverageUnits, marBill.OverageFee,
		marBill.Tax, marBill.TotalDue, marBill.DueAt.Format(time.RFC3339))

	// 三月账单截止 2026-04-08 00:00 UTC，尚未到期：账户未停用。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status      suspended=%t\n", st.Suspended)

	// 再补开一月账单：条件取自 1 月 1 日最初开通保存的快照，不套用
	// 二月改后的新价格。用量 15、超额 5×100=500 分、
	// 税 (1000+500)×10%=150 分、应付 1650 分。付款截止仍是账期结束
	// 后七天，即 2026-02-08 00:00 UTC，不随四月出账顺延。
	janBill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill jan    period=%s plan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxBps=%d\n",
		janBill.Period, janBill.Terms.PlanID, janBill.Terms.MonthlyFee,
		janBill.Terms.IncludedUnits, janBill.Terms.OveragePrice,
		janBill.Terms.TaxRateBasisPoints)
	fmt.Printf("bill jan    totalUsage=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
		janBill.TotalUsage, janBill.OverageUnits, janBill.OverageFee,
		janBill.Tax, janBill.TotalDue, janBill.DueAt.Format(time.RFC3339))

	// 旧账单查询与出账结果一致。
	janGot, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill jan plan=%s totalUsage=%d totalDue=%d balance=%d settled=%t dueAt=%s\n",
		janGot.Terms.PlanID, janGot.TotalUsage, janGot.TotalDue,
		janGot.Balance, janGot.Settled, janGot.DueAt.Format(time.RFC3339))

	// 一月账单的截止时刻（2 月 8 日）早已过去：补开旧账当即带来欠费
	// 停用。当前套餐条件仍是重新开通时保存的快照，补开旧账单不会把
	// 当前套餐改回旧价格。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status      subscribed=%t suspended=%t plan=%s monthlyFee=%d\n",
		st.Subscribed, st.Suspended, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee)

	// 二月不被任何订阅覆盖：为二月出账返回 ErrBillBeforeSubscription
	// （该错误同样用于两段订阅之间的空档月份，不限于早于首次开通的
	// 月份），随后查询返回 ErrBillNotFound。这两个是预期的空档错误，
	// 与上面正常操作一出错就 panic 的失败不同。
	_, err = s.CreateBill(accountID, feb)
	fmt.Printf("bill feb    errBillBeforeSubscription=%t\n",
		errors.Is(err, meter.ErrBillBeforeSubscription))
	_, err = s.GetBill(accountID, feb)
	fmt.Printf("get bill feb errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))

	// Output:
	// event e-jan accepted=true period=2026-01
	// cancel      cancelled=true endAt=2026-02-01T00:00:00Z
	// resubscribe subscribed=true plan=plan-flex monthlyFee=2000 includedUnits=20 overagePrice=200 taxBps=600
	// event e-mar accepted=true period=2026-03
	// bill mar    period=2026-03 plan=plan-flex monthlyFee=2000 includedUnits=20 overagePrice=200 taxBps=600
	// bill mar    totalUsage=25 overageUnits=5 overageFee=1000 tax=180 totalDue=3180 dueAt=2026-04-08T00:00:00Z
	// status      suspended=false
	// bill jan    period=2026-01 plan=plan-flex monthlyFee=1000 includedUnits=10 overagePrice=100 taxBps=1000
	// bill jan    totalUsage=15 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
	// get bill jan plan=plan-flex totalUsage=15 totalDue=1650 balance=1650 settled=false dueAt=2026-02-08T00:00:00Z
	// status      subscribed=true suspended=true plan=plan-flex monthlyFee=2000
	// bill feb    errBillBeforeSubscription=true
	// get bill feb errBillNotFound=true
}
