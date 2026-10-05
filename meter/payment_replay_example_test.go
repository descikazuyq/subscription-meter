package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// mustParseTime 解析 RFC3339 时刻；示例中的常量均合法，失败即 panic。
func mustParseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

// ExampleService_paymentReplay 演示“原样重报旧付款返回历史结果”以及读取
// 账单当前欠款的正确入口。示例只使用 meter 的公开入口，并通过
// NewServiceWithClock 把当前时刻固定在一个已经结束的 UTC 自然月账期之后，
// 因此无论实际运行日期为何，输出都固定，不必等到某个真实日期。
//
// 账户 acct-pay 的 2026-01 账单应付 1000 分（月费 1000 分，税额与用量均
// 为零；金额单位均为分）。随后分两笔付清：p1 登记 400 分，p2 登记剩余
// 600 分。结清后再用 p1 原样重报，返回的仍是 p1 首次登记完成时的结果：
// Registered 为 false、内容仍是 p1 对原账期的 400 分、余额与付清状态仍是
// 当时的 600 与 false——它们不是本次调用时重新计算的账单当前余额。当前
// 欠款应通过 GetBill / Status 读取。
func ExampleService_paymentReplay() {
	const (
		accountID = "acct-pay"
		jan1st    = "2026-01-01T00:00:00Z"
	)
	period := meter.MonthOf(mustParseTime(jan1st))

	// 把“当前时刻”固定在 2026-02-01 00:00 UTC：2026-01 账期恰好结束，
	// 可以立即为该月出账；当前时刻之后的任何真实日期也能得到同样结果。
	s := meter.NewServiceWithClock(func() time.Time {
		return mustParseTime("2026-02-01T00:00:00Z")
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
	if err := s.Subscribe(accountID, "plan1000", mustParseTime(jan1st)); err != nil {
		panic(err)
	}

	// 为已经结束且被订阅覆盖的 2026-01 出账：应付 1000 分，尚未付款。
	bill, err := s.CreateBill(accountID, period)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill        totalDue=%d paid=%d balance=%d settled=%t\n",
		bill.TotalDue, bill.Paid, bill.Balance, bill.Settled)

	// 第一笔：付款标识 p1 登记 400 分。Registered=true 表示本次实际登记，
	// 登记后余额 600、尚未付清。
	p1, err := s.RecordPayment(accountID, "p1", period, 400)
	if err != nil {
		panic(err)
	}
	fmt.Printf("p1 first    registered=%t paymentID=%s period=%s amount=%d billBalance=%d settled=%t\n",
		p1.Registered, p1.Payment.PaymentID, p1.Payment.Period, p1.Payment.Amount,
		p1.BillBalance, p1.Settled)

	// 第二笔：另一付款标识 p2 登记剩余 600 分，账单结清。
	p2, err := s.RecordPayment(accountID, "p2", period, 600)
	if err != nil {
		panic(err)
	}
	fmt.Printf("p2 first    registered=%t paymentID=%s period=%s amount=%d billBalance=%d settled=%t\n",
		p2.Registered, p2.Payment.PaymentID, p2.Payment.Period, p2.Payment.Amount,
		p2.BillBalance, p2.Settled)

	// 结清后原样重报 p1：账户、付款标识、账期、金额与首次完全一致。
	// 重报仍然成功，但本次不再登记：Registered=false 表示“此前已登记过
	// 同一笔”，不是付款失败；付款内容仍是 p1 对原账期的 400 分，
	// BillBalance=600、Settled=false 是 p1 首次登记完成那一刻账单的
	// 历史快照，不会因为 p2 之后又付了 600 分而变成 0/true。
	p1Replay, err := s.RecordPayment(accountID, "p1", period, 400)
	if err != nil {
		panic(err)
	}
	fmt.Printf("p1 replay   registered=%t paymentID=%s period=%s amount=%d billBalance=%d settled=%t\n",
		p1Replay.Registered, p1Replay.Payment.PaymentID, p1Replay.Payment.Period,
		p1Replay.Payment.Amount, p1Replay.BillBalance, p1Replay.Settled)

	// 要看账单当前欠多少，必须查账单，而不是引用旧付款的返回值。
	// GetBill 显示当前状态：累计已付 1000 分、余额为零、已经付清。
	current, err := s.GetBill(accountID, period)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill    totalDue=%d paid=%d balance=%d settled=%t\n",
		current.TotalDue, current.Paid, current.Balance, current.Settled)

	// Status 中同一张账单的摘要与 GetBill 一致：重报没有再次增加已付金额，
	// 也没有让账单重新变成未付清。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	for _, b := range st.Bills {
		fmt.Printf("status bill period=%s totalDue=%d paid=%d balance=%d settled=%t\n",
			b.Period, b.TotalDue, b.Paid, b.Balance, b.Settled)
	}

	// 原样重报要求同一账户内付款标识、账期、金额三者都一致。
	// 沿用 p1 却把金额改成 401 分：冲突，原账单与 p1 记录保持不变。
	_, err = s.RecordPayment(accountID, "p1", period, 401)
	fmt.Printf("p1 amount=401 errPaymentConflict=%t\n", errors.Is(err, meter.ErrPaymentConflict))

	// 沿用 p1 却指向尚未出账的另一账期（2026-02，该月也尚未结束）：
	// 同样按冲突处理，而不是当作一笔新付款。
	future := meter.MonthOf(mustParseTime("2026-02-15T00:00:00Z"))
	_, err = s.RecordPayment(accountID, "p1", future, 400)
	fmt.Printf("p1 period=2026-02 errPaymentConflict=%t\n", errors.Is(err, meter.ErrPaymentConflict))

	// 两次冲突都没有改动任何数据：当前账单仍是已付 1000、余额 0、已付清。
	after, err := s.GetBill(accountID, period)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get bill    totalDue=%d paid=%d balance=%d settled=%t\n",
		after.TotalDue, after.Paid, after.Balance, after.Settled)

	// Output:
	// bill        totalDue=1000 paid=0 balance=1000 settled=false
	// p1 first    registered=true paymentID=p1 period=2026-01 amount=400 billBalance=600 settled=false
	// p2 first    registered=true paymentID=p2 period=2026-01 amount=600 billBalance=0 settled=true
	// p1 replay   registered=false paymentID=p1 period=2026-01 amount=400 billBalance=600 settled=false
	// get bill    totalDue=1000 paid=1000 balance=0 settled=true
	// status bill period=2026-01 totalDue=1000 paid=1000 balance=0 settled=true
	// p1 amount=401 errPaymentConflict=true
	// p1 period=2026-02 errPaymentConflict=true
	// get bill    totalDue=1000 paid=1000 balance=0 settled=true
}
