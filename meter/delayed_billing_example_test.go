package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_delayedBilling 演示延迟出账：付款截止时刻从账期结束时起算、
// 不因晚出账顺延；越过截止时刻后出账且账单有余额时，出账后直接查询即显示
// 欠费停用，停用拒收尚未出账账期的新用量；付款结清后立即恢复，此前被拒的
// 同一事件作为首次接收被累计。示例只使用 meter 的公开入口，并通过
// NewServiceWithClock 把当前时刻固定，输出与真实运行日期无关。
//
// 账户 acct-late 于 2026-01-01 00:00 UTC 开通一份持续有效的订阅，套餐月费
// 1000 分，超额单价与税率均为零，账户没有其他账单。当前时刻固定在
// 2026-02-10 00:00 UTC：一月账期已结束 9 天，一月账单的付款截止时刻
// （账期结束后七天，即 2026-02-08 00:00 UTC）也已经过去 2 天。
func ExampleService_delayedBilling() {
	const (
		accountID = "acct-late"
		jan1st    = "2026-01-01T00:00:00Z"
		feb10th   = "2026-02-10T00:00:00Z"
	)
	jan := meter.MonthOf(mustParseTime(jan1st))
	feb := meter.MonthOf(mustParseTime(feb10th))

	// 把“当前时刻”固定在 2026-02-10 00:00 UTC：一月账期早已结束，
	// 一月账单的七天付款期限（截止 2026-02-08 00:00 UTC）也已过去。
	s := meter.NewServiceWithClock(func() time.Time {
		return mustParseTime(feb10th)
	})

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐月费恰好 1000 分、无超额、无税。
	if err := s.CreatePlan(meter.Plan{
		ID:                 "plan1000",
		MonthlyFee:         1000,
		IncludedUnits:      0,
		OveragePrice:       0,
		TaxRateBasisPoints: 0,
	}); err != nil {
		panic(err)
	}
	// 一份自 2026-01-01 00:00 UTC 起持续有效的订阅。
	if err := s.Subscribe(accountID, "plan1000", mustParseTime(jan1st)); err != nil {
		panic(err)
	}

	// 出账前账户没有任何账单，也就不存在到期欠款：账户未停用、订阅有效、
	// 当前套餐条件就是开通时保存的快照。
	before, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("before bill  subscribed=%t suspended=%t plan=%s monthlyFee=%d bills=%d\n",
		before.Subscribed, before.Suspended, before.CurrentTerms.PlanID,
		before.CurrentTerms.MonthlyFee, len(before.Bills))

	// 延迟到 2026-02-10 才为 2026-01 出账。应付与余额均为 1000 分、尚未
	// 结清；付款截止时刻仍是账期结束后七天的 2026-02-08 00:00 UTC，
	// 不会因为今天才出账而顺延。
	bill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("jan bill     totalDue=%d paid=%d balance=%d settled=%t dueAt=%s\n",
		bill.TotalDue, bill.Paid, bill.Balance, bill.Settled,
		bill.DueAt.Format(time.RFC3339))

	// 出账后直接查询即显示欠费停用（截止时刻已过且账单仍有余额）；
	// 停用不等于退订：订阅本身仍有效，当前套餐条件也保持原样。
	overdue, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after bill   subscribed=%t suspended=%t plan=%s monthlyFee=%d\n",
		overdue.Subscribed, overdue.Suspended, overdue.CurrentTerms.PlanID,
		overdue.CurrentTerms.MonthlyFee)

	// 上报一条发生在当前时刻、数量为 3、归属尚未出账的二月的新用量。
	evt := meter.Event{
		AccountID: accountID,
		EventID:   "evt-feb-3",
		At:        mustParseTime(feb10th),
		Quantity:  3,
	}
	// 预期中的业务失败必须显式辨认：ErrSuspended 表示因到期欠费被停用拒收，
	// 此时不能把它当作成功继续处理。
	_, err = s.RecordEvent(evt)
	fmt.Printf("event reject errSuspended=%t\n", errors.Is(err, meter.ErrSuspended))

	// 拒收不累计用量：二月累计仍为零；事件标识也没有被这次失败占用。
	rejectedUsage, err := s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("feb reject   total=%d\n", rejectedUsage.Total)

	// 登记一笔 1000 分的本地付款，恰好付清一月账单。
	pay, err := s.RecordPayment(accountID, "pay-jan", jan, 1000)
	if err != nil {
		panic(err)
	}
	fmt.Printf("payment      registered=%t billBalance=%d settled=%t\n",
		pay.Registered, pay.BillBalance, pay.Settled)

	// 一月账单现在已付 1000、余额为零、已结清；付款不改变出账时固定的
	// 应付总额（仍是 1000）与付款截止时刻（仍是 2026-02-08 00:00 UTC）。
	paidBill, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("jan bill     totalDue=%d paid=%d balance=%d settled=%t dueAt=%s\n",
		paidBill.TotalDue, paidBill.Paid, paidBill.Balance, paidBill.Settled,
		paidBill.DueAt.Format(time.RFC3339))

	// 到期欠款已结清：欠费停用解除；订阅没有被付款影响，继续有效，
	// 套餐条件仍是原来的 plan1000。
	resumed, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("after pay    subscribed=%t suspended=%t plan=%s monthlyFee=%d\n",
		resumed.Subscribed, resumed.Suspended, resumed.CurrentTerms.PlanID,
		resumed.CurrentTerms.MonthlyFee)

	// 再次提交刚才被拒的同一事件：此前拒收没有占用标识，本次按首次接收，
	// Accepted=true，归入 2026-02；二月累计由 0 变为 3。
	accepted, err := s.RecordEvent(evt)
	if err != nil {
		panic(err)
	}
	fmt.Printf("event retry  accepted=%t period=%s\n", accepted.Accepted, accepted.Period)
	febUsage, err := s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("feb retry    total=%d\n", febUsage.Total)

	// Output:
	// before bill  subscribed=true suspended=false plan=plan1000 monthlyFee=1000 bills=0
	// jan bill     totalDue=1000 paid=0 balance=1000 settled=false dueAt=2026-02-08T00:00:00Z
	// after bill   subscribed=true suspended=true plan=plan1000 monthlyFee=1000
	// event reject errSuspended=true
	// feb reject   total=0
	// payment      registered=true billBalance=0 settled=true
	// jan bill     totalDue=1000 paid=1000 balance=0 settled=true dueAt=2026-02-08T00:00:00Z
	// after pay    subscribed=true suspended=false plan=plan1000 monthlyFee=1000
	// event retry  accepted=true period=2026-02
	// feb retry    total=3
}
