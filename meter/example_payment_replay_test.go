package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_RecordPayment_replayHistoricalResult 演示分次付款以及
// “原样重报返回历史结果”的语义。
//
// 为了让示例在任何一天运行都得到同样的输出，这里用 NewServiceWithClock
// 注入固定时钟，把当前时刻固定在 2026-02-01 00:00 UTC：账期 2026-01
// （UTC 自然月，区间 [2026-01-01 00:00, 2026-02-01 00:00)）已经结束，
// 无需等到某个真实日期即可出账。全部金额单位为分。
func ExampleService_RecordPayment_replayHistoricalResult() {
	const accountID = "acct-demo"
	// period 是已结束的 2026-01 UTC 自然月账期。
	period := meter.Month{Year: 2026, Month: time.January}

	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	s := meter.NewServiceWithClock(func() time.Time { return now })

	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	// 准备：账户、套餐与订阅。月费 1000 分、包含用量与超额单价均为 0、
	// 税率为 0，因此没有用量时该月应付恰好是 1000 分。
	must(s.CreateAccount(accountID))
	must(s.CreatePlan(meter.Plan{
		ID:                 "plan-1000",
		MonthlyFee:         1000, // 金额单位：分
		IncludedUnits:      0,
		OveragePrice:       0,
		TaxRateBasisPoints: 0,
	}))
	must(s.Subscribe(accountID, "plan-1000", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))

	// 账期已结束，为 2026-01 出账：应付 1000 分，尚未付款。
	bill, err := s.CreateBill(accountID, period)
	if err != nil {
		panic(err)
	}
	fmt.Printf("出账: period=%s totalDue=%d paid=%d balance=%d settled=%t\n",
		bill.Period, bill.TotalDue, bill.Paid, bill.Balance, bill.Settled)

	// 第一次付款：用标识 p1 登记 400 分。Registered=true 表示本次实际登记；
	// 登记完成后账单余额 600、尚未付清。
	first, err := s.RecordPayment(accountID, "p1", period, 400)
	if err != nil {
		panic(err)
	}
	fmt.Printf("p1 首次登记 400: registered=%t billBalance=%d settled=%t\n",
		first.Registered, first.BillBalance, first.Settled)

	// 再用另一标识 p2 登记剩余 600 分：本次实际登记，账单结清、当前余额 0。
	rest, err := s.RecordPayment(accountID, "p2", period, 600)
	if err != nil {
		panic(err)
	}
	fmt.Printf("p2 登记 600: registered=%t billBalance=%d settled=%t\n",
		rest.Registered, rest.BillBalance, rest.Settled)

	// 结清之后原样重报 p1（同一账户、同一付款标识、同一账期、同一金额）：
	// 请求成功，但 Registered=false——不会再次登记，也不再次增加已付金额。
	// Payment 仍是 p1 对 2026-01 账期登记的那 400 分；BillBalance=600、
	// Settled=false 描述的是 p1 首次成功登记完成时的历史结果，而不是本次
	// 调用时重新计算的当前欠款——所以这里的 600 大于账单当前余额 0。
	// Registered=false 只表示“命中了此前已登记的付款”，并非付款失败。
	replay, err := s.RecordPayment(accountID, "p1", period, 400)
	if err != nil {
		panic(err)
	}
	fmt.Printf("原样重报 p1: registered=%t payment={id:%s period:%s amount:%d} billBalance=%d settled=%t\n",
		replay.Registered, replay.Payment.PaymentID, replay.Payment.Period,
		replay.Payment.Amount, replay.BillBalance, replay.Settled)

	// 当前账单状态要读 GetBill 或 Status：累计已付 1000、余额 0、已付清。
	// 旧付款重报不会再次增加已付金额，也不会让账单重新变成未付清。
	cur, err := s.GetBill(accountID, period)
	if err != nil {
		panic(err)
	}
	fmt.Printf("GetBill 当前状态: totalDue=%d paid=%d balance=%d settled=%t\n",
		cur.TotalDue, cur.Paid, cur.Balance, cur.Settled)

	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	var summary meter.BillSummary
	for _, b := range st.Bills {
		if b.Period == period {
			summary = b
		}
	}
	fmt.Printf("Status 当前状态: paid=%d balance=%d settled=%t\n",
		summary.Paid, summary.Balance, summary.Settled)

	// 原样重报要求同一账户内付款标识、账期、金额三者一致。沿用 p1 却把
	// 金额改成 401 分：ErrPaymentConflict。
	if _, err := s.RecordPayment(accountID, "p1", period, 401); !errors.Is(err, meter.ErrPaymentConflict) {
		panic(fmt.Sprintf("p1 改金额应返回 ErrPaymentConflict，实际 %v", err))
	}
	fmt.Println("p1 改金额为 401: ErrPaymentConflict")

	// 把 p1 改指向另一个尚未出账的账期（即使那里还没有账单）：同样是冲突，
	// 而不是被当作一笔新付款，也不会返回 ErrBillNotFound。
	future := meter.Month{Year: 2026, Month: time.March}
	if _, err := s.RecordPayment(accountID, "p1", future, 400); !errors.Is(err, meter.ErrPaymentConflict) {
		panic(fmt.Sprintf("p1 改账期应返回 ErrPaymentConflict，实际 %v", err))
	}
	fmt.Println("p1 改指向未出账账期 2026-03: ErrPaymentConflict")

	// 冲突不改变任何记录：账单仍是已付 1000、余额 0、已付清；
	// p1 的历史结果也保持原样。
	after, err := s.GetBill(accountID, period)
	if err != nil {
		panic(err)
	}
	fmt.Printf("冲突后 GetBill: paid=%d balance=%d settled=%t\n",
		after.Paid, after.Balance, after.Settled)
	replay2, err := s.RecordPayment(accountID, "p1", period, 400)
	if err != nil {
		panic(err)
	}
	fmt.Printf("冲突后再重报 p1: registered=%t billBalance=%d settled=%t\n",
		replay2.Registered, replay2.BillBalance, replay2.Settled)

	// 付款标识只在所属账户内去重：另一个账户可以复用 p1，作为它自己的
	// 首次付款独立登记，与 acct-demo 的 p1 互不影响。
	const other = "acct-other"
	must(s.CreateAccount(other))
	must(s.Subscribe(other, "plan-1000", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if _, err := s.CreateBill(other, period); err != nil {
		panic(err)
	}
	otherPay, err := s.RecordPayment(other, "p1", period, 400)
	if err != nil {
		panic(err)
	}
	fmt.Printf("另一账户复用 p1: registered=%t billBalance=%d settled=%t\n",
		otherPay.Registered, otherPay.BillBalance, otherPay.Settled)

	// Output:
	// 出账: period=2026-01 totalDue=1000 paid=0 balance=1000 settled=false
	// p1 首次登记 400: registered=true billBalance=600 settled=false
	// p2 登记 600: registered=true billBalance=0 settled=true
	// 原样重报 p1: registered=false payment={id:p1 period:2026-01 amount:400} billBalance=600 settled=false
	// GetBill 当前状态: totalDue=1000 paid=1000 balance=0 settled=true
	// Status 当前状态: paid=1000 balance=0 settled=true
	// p1 改金额为 401: ErrPaymentConflict
	// p1 改指向未出账账期 2026-03: ErrPaymentConflict
	// 冲突后 GetBill: paid=1000 balance=0 settled=true
	// 冲突后再重报 p1: registered=false billBalance=600 settled=false
	// 另一账户复用 p1: registered=true billBalance=600 settled=false
}
