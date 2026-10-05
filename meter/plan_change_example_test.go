package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_planChangeSnapshot 演示“修改套餐定义”与“给账户安排换套餐”
// 的区别：UpdatePlan 修改的是套餐的当前定义，不会改写账户正在使用的条件，
// 也不会改写此前已接受的换套餐安排；条件只在开通订阅或安排换套餐被接受时
// 取得一份快照。示例只使用 meter 的公开入口，并用 NewServiceWithClock 逐步
// 推进“当前时刻”，因此无论实际运行日期为何，输出都固定（所有时刻均为 UTC，
// 金额单位均为分）。
//
// 账户 acct-switch 自 2026-01-01 00:00 UTC 起使用套餐 A（月费 1000、包含
// 100 单位、超额单价 10、税率 1000‱）。1 月 15 日安排 2 月换到套餐 B，
// 安排时 B 的定义为月费 2000、包含 200、超额单价 8、税率 600‱：安排保存
// 这份完整条件快照与生效账期 2026-02；当时查询仍显示 A，待生效安排为 B。
// 随后把 B 的定义改成月费 5000、包含 50、超额单价 20、税率 1000‱，再次
// 安排同一目标 B：安排尚未生效，调用成功但返回原安排、Created 为 false，
// 不会按新定义重新取价；待生效安排与当前条件都没有变化。
//
// 到 2026-02-01 00:00 UTC 后直接查询，当前条件即为安排锁定的 B（2000/200/
// 8/600‱），待生效安排消失，无需先上报用量或出账；此时再安排 B，因 B 已是
// 当前套餐，按现有规则返回 ErrPlanChangeSamePlan——重复安排返回原安排只适用
// 于安排尚未生效时。
//
// 计费上，1 月一笔 150 单位的用量仍按 A 出账：完整月费 1000、完整额度 100，
// 超额 50×10=500，税 (1000+500)×10%=150，应付 1650。该账单 2026-02-08
// 00:00 UTC 到期未付，账户被欠费停用，2 月一笔 300 单位的用量先被拒收
// （ErrSuspended）；登记 1650 分付款结清一月账单后恢复，同一事件再次提交
// 作为首次接收。3 月 1 日为 2 月出账时使用的是安排锁定的 B 条件而不是修改
// 后的定义：月费 2000、额度 200，超额 100×8=800，税 2800×6%=168，应付
// 2968；二月出账后再查一月账单，仍是原金额 1650。
func ExampleService_planChangeSnapshot() {
	const (
		accountID = "acct-switch"
		jan1st    = "2026-01-01T00:00:00Z"
	)
	jan := meter.MonthOf(mustParseTime(jan1st))
	feb := meter.MonthOf(mustParseTime("2026-02-05T00:00:00Z"))

	// 当前时刻从 2026-01-15 12:00 UTC 起步，随后只通过给 now 赋新值推进，
	// 示例输出与运行当天日期无关。
	now := mustParseTime("2026-01-15T12:00:00Z")
	s := meter.NewServiceWithClock(func() time.Time { return now })

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐 A：账户一月正在使用的条件。月费 1000、包含 100、超额单价 10、税率 1000‱。
	if err := s.CreatePlan(meter.Plan{
		ID:                 "plan-a",
		MonthlyFee:         1000,
		IncludedUnits:      100,
		OveragePrice:       10,
		TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	// 套餐 B：安排换套餐被接受时的定义。月费 2000、包含 200、超额单价 8、税率 600‱。
	if err := s.CreatePlan(meter.Plan{
		ID:                 "plan-b",
		MonthlyFee:         2000,
		IncludedUnits:      200,
		OveragePrice:       8,
		TaxRateBasisPoints: 600,
	}); err != nil {
		panic(err)
	}
	// 账户自 2026-01-01 00:00 UTC 起订阅套餐 A，开通时保存 A 的条件快照。
	if err := s.Subscribe(accountID, "plan-a", mustParseTime(jan1st)); err != nil {
		panic(err)
	}

	// 一月中旬安排二月换到 B：安排被接受即锁定 B 的完整条件与生效账期 2026-02。
	change, err := s.SchedulePlanChange(accountID, "plan-b")
	if err != nil {
		panic(err)
	}
	fmt.Printf("change 1    created=%t target=%s monthlyFee=%d included=%d overagePrice=%d taxBasisPoints=%d effective=%s\n",
		change.Created, change.Change.TargetPlanID, change.Change.Terms.MonthlyFee,
		change.Change.Terms.IncludedUnits, change.Change.Terms.OveragePrice,
		change.Change.Terms.TaxRateBasisPoints, change.Change.EffectivePeriod)

	// 此时查询仍显示 A；待生效安排为 B，安排里保存的就是接受时锁定的完整条件。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status jan  current=%s monthlyFee=%d pendingTarget=%s pendingFee=%d pendingIncluded=%d pendingOverage=%d pendingTaxBp=%d pendingEffective=%s\n",
		st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.PendingChange.TargetPlanID, st.PendingChange.Terms.MonthlyFee,
		st.PendingChange.Terms.IncludedUnits, st.PendingChange.Terms.OveragePrice,
		st.PendingChange.Terms.TaxRateBasisPoints, st.PendingChange.EffectivePeriod)

	// 修改套餐 B 的“定义”：月费、包含额度、超额单价、税率全部改变。
	// 这不会改写账户正在使用的 A 条件，也不会改写上面已接受的待生效安排。
	if err := s.UpdatePlan(meter.Plan{
		ID:                 "plan-b",
		MonthlyFee:         5000,
		IncludedUnits:      50,
		OveragePrice:       20,
		TaxRateBasisPoints: 1000,
	}); err != nil {
		panic(err)
	}
	fmt.Printf("plan B      monthlyFee=5000 included=50 overagePrice=20 taxBasisPoints=1000\n")

	// 安排尚未生效时再次安排同一目标 B：成功返回原安排，Created=false，
	// 条件仍是首次接受时的 2000/200/8/600‱，不会按修改后的定义重新取价。
	again, err := s.SchedulePlanChange(accountID, "plan-b")
	if err != nil {
		panic(err)
	}
	fmt.Printf("change 2    created=%t target=%s monthlyFee=%d included=%d overagePrice=%d taxBasisPoints=%d effective=%s\n",
		again.Created, again.Change.TargetPlanID, again.Change.Terms.MonthlyFee,
		again.Change.Terms.IncludedUnits, again.Change.Terms.OveragePrice,
		again.Change.Terms.TaxRateBasisPoints, again.Change.EffectivePeriod)

	// 查询结果同样不变：当前仍是 A，待生效安排仍锁定修改前的 B 条件。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status jan  current=%s monthlyFee=%d pendingTarget=%s pendingFee=%d pendingIncluded=%d pendingOverage=%d pendingTaxBp=%d pendingEffective=%s\n",
		st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.PendingChange.TargetPlanID, st.PendingChange.Terms.MonthlyFee,
		st.PendingChange.Terms.IncludedUnits, st.PendingChange.Terms.OveragePrice,
		st.PendingChange.Terms.TaxRateBasisPoints, st.PendingChange.EffectivePeriod)

	// 一月一笔 150 单位的用量（超过 A 的 100 单位额度）。
	now = mustParseTime("2026-01-20T10:00:00Z")
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e-jan",
		At:        mustParseTime("2026-01-20T10:00:00Z"),
		Quantity:  150,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event jan   accepted=%t period=%s\n", ev.Accepted, ev.Period)
	usage, err := s.MonthlyUsage(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 推进到 2026-02-01 00:00 UTC：无需上报用量或生成账单，直接查询即显示
	// 安排锁定的 B 条件（修改后的 B 定义没有混入），待生效安排消失。
	now = mustParseTime("2026-02-01T00:00:00Z")
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status feb  subscribed=%t suspended=%t current=%s monthlyFee=%d included=%d overagePrice=%d taxBasisPoints=%d pending=%t\n",
		st.Subscribed, st.Suspended, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
		st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
		st.CurrentTerms.TaxRateBasisPoints, st.PendingChange != nil)

	// 安排已经生效，B 就是当前套餐：再安排 B 按现有规则返回同套餐错误，
	// 而不是“返回原安排、Created=false”——后者只适用于安排尚未生效时。
	_, err = s.SchedulePlanChange(accountID, "plan-b")
	fmt.Printf("schedule b  errPlanChangeSamePlan=%t\n", errors.Is(err, meter.ErrPlanChangeSamePlan))

	// 账期结束后为一月出账：仍收 A 的完整月费并给完整额度。
	// 超额 50 单位×10=500；税 (1000+500)×1000‱=150；应付 1650。
	janBill, err := s.CreateBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill jan    plan=%s monthlyFee=%d usage=%d included=%d overageUnits=%d overagePrice=%d overageFee=%d taxBp=%d tax=%d totalDue=%d paid=%d balance=%d settled=%t dueAt=%s\n",
		janBill.Terms.PlanID, janBill.MonthlyFee, janBill.TotalUsage, janBill.IncludedUnits,
		janBill.OverageUnits, janBill.Terms.OveragePrice, janBill.OverageFee,
		janBill.Terms.TaxRateBasisPoints, janBill.Tax, janBill.TotalDue, janBill.Paid,
		janBill.Balance, janBill.Settled, janBill.DueAt.Format(time.RFC3339))

	// 推进到 2026-02-09 00:00 UTC：一月账单截止时刻 2026-02-08 已过且未付，
	// 账户因欠费停用（订阅仍有效、当前条件不变）。
	now = mustParseTime("2026-02-09T00:00:00Z")
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status due  suspended=%t\n", st.Suspended)

	// 二月一笔 300 单位的用量：停用期间被拒绝（ErrSuspended），不消耗事件标识。
	_, err = s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e-feb",
		At:        mustParseTime("2026-02-05T00:00:00Z"),
		Quantity:  300,
	})
	fmt.Printf("event feb   errSuspended=%t\n", errors.Is(err, meter.ErrSuspended))

	// 正常登记一笔 1600 分的付款结清一月账单，解除欠费停用；
	// 付款只减少余额，不改变一月账单的应付金额。
	pay, err := s.RecordPayment(accountID, "pay-jan", jan, janBill.TotalDue)
	if err != nil {
		panic(err)
	}
	fmt.Printf("payment     registered=%t paymentID=%s period=%s amount=%d billBalance=%d settled=%t\n",
		pay.Registered, pay.Payment.PaymentID, pay.Payment.Period, pay.Payment.Amount,
		pay.BillBalance, pay.Settled)
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("status paid suspended=%t\n", st.Suspended)

	// 解除停用后再次提交同一事件：作为首次接收，归入 2026-02。
	ev, err = s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e-feb",
		At:        mustParseTime("2026-02-05T00:00:00Z"),
		Quantity:  300,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event feb   accepted=%t period=%s\n", ev.Accepted, ev.Period)
	usage, err = s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

	// 推进到 2026-03-01 00:00 UTC，二月账期结束后才为它出账。
	// 使用的是安排锁定的 B 条件（2000/200/8/600‱），不是修改后的定义
	// （5000/50/20/1000‱）：超额 100 单位×8=800；税 2800×600‱=168；应付 2968。
	now = mustParseTime("2026-03-01T00:00:00Z")
	febBill, err := s.CreateBill(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill feb    plan=%s monthlyFee=%d usage=%d included=%d overageUnits=%d overagePrice=%d overageFee=%d taxBp=%d tax=%d totalDue=%d paid=%d balance=%d settled=%t dueAt=%s\n",
		febBill.Terms.PlanID, febBill.MonthlyFee, febBill.TotalUsage, febBill.IncludedUnits,
		febBill.OverageUnits, febBill.Terms.OveragePrice, febBill.OverageFee,
		febBill.Terms.TaxRateBasisPoints, febBill.Tax, febBill.TotalDue, febBill.Paid,
		febBill.Balance, febBill.Settled, febBill.DueAt.Format(time.RFC3339))

	// 二月出账后再查一月账单：金额仍是出账时的 1600，且已付清。
	gotJan, err := s.GetBill(accountID, jan)
	if err != nil {
		panic(err)
	}
	fmt.Printf("get jan bill totalDue=%d paid=%d balance=%d settled=%t\n",
		gotJan.TotalDue, gotJan.Paid, gotJan.Balance, gotJan.Settled)

	// Output:
	// change 1    created=true target=plan-b monthlyFee=2000 included=200 overagePrice=8 taxBasisPoints=600 effective=2026-02
	// status jan  current=plan-a monthlyFee=1000 pendingTarget=plan-b pendingFee=2000 pendingIncluded=200 pendingOverage=8 pendingTaxBp=600 pendingEffective=2026-02
	// plan B      monthlyFee=5000 included=50 overagePrice=20 taxBasisPoints=1000
	// change 2    created=false target=plan-b monthlyFee=2000 included=200 overagePrice=8 taxBasisPoints=600 effective=2026-02
	// status jan  current=plan-a monthlyFee=1000 pendingTarget=plan-b pendingFee=2000 pendingIncluded=200 pendingOverage=8 pendingTaxBp=600 pendingEffective=2026-02
	// event jan   accepted=true period=2026-01
	// usage       period=2026-01 total=150
	// status feb  subscribed=true suspended=false current=plan-b monthlyFee=2000 included=200 overagePrice=8 taxBasisPoints=600 pending=false
	// schedule b  errPlanChangeSamePlan=true
	// bill jan    plan=plan-a monthlyFee=1000 usage=150 included=100 overageUnits=50 overagePrice=10 overageFee=500 taxBp=1000 tax=150 totalDue=1650 paid=0 balance=1650 settled=false dueAt=2026-02-08T00:00:00Z
	// status due  suspended=true
	// event feb   errSuspended=true
	// payment     registered=true paymentID=pay-jan period=2026-01 amount=1650 billBalance=0 settled=true
	// status paid suspended=false
	// event feb   accepted=true period=2026-02
	// usage       period=2026-02 total=300
	// bill feb    plan=plan-b monthlyFee=2000 usage=300 included=200 overageUnits=100 overagePrice=8 overageFee=800 taxBp=600 tax=168 totalDue=2968 paid=0 balance=2968 settled=false dueAt=2026-03-08T00:00:00Z
	// get jan bill totalDue=1650 paid=1650 balance=0 settled=true
}
