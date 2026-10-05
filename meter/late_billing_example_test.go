package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_lateBilling 演示“延迟出账”：账期结束一段时间后才生成账单时，
// 付款截止时刻与账户停用行为如何变化。示例只使用 meter 的公开入口，并通过
// NewServiceWithClock 把当前时刻固定在 2026-02-10 00:00 UTC，因此无论实际
// 运行日期为何，输出都固定，不必等到某个真实日期。
//
// 账户 acct-late 持有一份持续有效的订阅：2026-01-01 00:00 UTC 开通，套餐
// 月费 1000 分、无超额、无税，账户没有其他账单。2026-02-10 才为一月出账：
// 付款截止仍是 2026-02-08 00:00 UTC——七天期限从账期结束（2026-02-01
// 00:00 UTC）算起，晚出账不会顺延。截止时刻本身已属于到期：当前时刻已过
// 截止，账单应付与余额都是 1000 分、尚未结清，因此出账后直接查询即显示
// 欠费停用；订阅本身仍有效，当前套餐条件保持原样。
//
// 停用期间上报属于尚未出账的二月的新用量会被拒绝（ErrSuspended），且拒收
// 不消耗事件标识。登记 1000 分本地付款后，一月账单已付 1000 分、余额为零、
// 已结清，账户解除停用，原订阅继续有效；再次提交刚才被拒绝的事件即作为
// 首次接收，二月累计变为 3。付款不改变一月账单原有的应付金额与截止时刻。
func ExampleService_lateBilling() {
	const (
		accountID = "acct-late"
		jan1st    = "2026-01-01T00:00:00Z"
		now       = "2026-02-10T00:00:00Z"
	)
	jan := meter.MonthOf(mustParseTime(jan1st))
	feb := meter.MonthOf(mustParseTime(now))

	// 把“当前时刻”固定在 2026-02-10 00:00 UTC：2026-01 账期早已结束，
	// 此时才为它出账，即“延迟出账”。
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

	// 出账前查询：没有其他欠款，账户未停用，订阅有效。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("before bill subscribed=%t suspended=%t planID=%s monthlyFee=%d\n",
		st.Subscribed, st.Suspended, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee)

	// 2026-02-10 才为 2026-01 出账。付款截止时刻仍是账期结束后七天，
	// 即 2026-02-08 00:00 UTC：从账期结束算起，晚出账不顺延。
	// 当前时刻已过截止，账单余额 1000 分未结清。
	bill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill        period=%s totalDue=%d balance=%d settled=%t dueAt=%s\n",
		bill.Period, bill.TotalDue, bill.Balance, bill.Settled,
		bill.DueAt.Format(time.RFC3339))

	// 出账后直接查询：因一月账单到期未结清而欠费停用；订阅本身仍有效，
	// 当前套餐条件保持原样。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after bill  subscribed=%t suspended=%t planID=%s monthlyFee=%d\n",
		st.Subscribed, st.Suspended, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee)

	// 上报一条发生在当前时刻、数量为 3、属于尚未出账的二月的新用量：
	// 因欠费停用被拒绝（ErrSuspended）。拒收不消耗事件标识 e1。
	_, err = s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e1",
		At:        mustParseTime(now),
		Quantity:  3,
	})
	fmt.Printf("event e1    errSuspended=%t\n", errors.Is(err, meter.ErrSuspended))

	// 被拒收的事件没有累计：二月用量仍为零。
	usage, err := s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 登记 1000 分本地付款，结清一月账单。付款只减少余额，不改变账单
	// 原有的应付金额与截止时刻。
	pay, err := s.RecordPayment(accountID, "pay-jan", jan, 1000)
	if err != nil {
		panic(err)
	}
	fmt.Printf("payment     registered=%t paymentID=%s amount=%d billBalance=%d settled=%t\n",
		pay.Registered, pay.Payment.PaymentID, pay.Payment.Amount, pay.BillBalance, pay.Settled)

	// 查询一月账单：已付 1000 分、余额为零、已结清；应付仍是 1000 分，
	// 截止时刻仍是 2026-02-08 00:00 UTC。
	current, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill    totalDue=%d paid=%d balance=%d settled=%t dueAt=%s\n",
		current.TotalDue, current.Paid, current.Balance, current.Settled,
		current.DueAt.Format(time.RFC3339))

	// 结清全部到期欠费后账户解除停用，原订阅继续有效，套餐条件不变。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after pay   subscribed=%t suspended=%t planID=%s monthlyFee=%d\n",
		st.Subscribed, st.Suspended, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee)

	// 再次提交刚才被拒绝的事件 e1：拒收没有占用事件标识，本次作为
	// 首次接收，归入 2026-02。
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e1",
		At:        mustParseTime(now),
		Quantity:  3,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e1    accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 二月累计变为 3。
	usage, err = s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// Output:
	// before bill subscribed=true suspended=false planID=plan1000 monthlyFee=1000
	// bill        period=2026-01 totalDue=1000 balance=1000 settled=false dueAt=2026-02-08T00:00:00Z
	// after bill  subscribed=true suspended=true planID=plan1000 monthlyFee=1000
	// event e1    errSuspended=true
	// usage       period=2026-02 total=0
	// payment     registered=true paymentID=pay-jan amount=1000 billBalance=0 settled=true
	// get bill    totalDue=1000 paid=1000 balance=0 settled=true dueAt=2026-02-08T00:00:00Z
	// after pay   subscribed=true suspended=false planID=plan1000 monthlyFee=1000
	// event e1    accepted=true period=2026-02
	// usage       period=2026-02 total=3
}
