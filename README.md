# 订阅、用量与账单

在本机运行的订阅、用量与账单服务，以 Go 包 `meter` 提供，数据保存在内存中。

## 使用

```bash
go test ./...
```

## 能力

- `NewService` / `NewServiceWithClock`：创建并发安全的服务（后者可注入时钟）。
- `CreateAccount`：创建业务账户，标识唯一。
- `CreatePlan` / `UpdatePlan`：维护套餐（月费、包含用量、超额单价、万分比税率）。修改套餐定义只影响之后新保存的条件快照（之后开通的订阅与之后新接受的换套餐安排），不改写已开通订阅保存的快照，也不改写已接受安排锁定的条件。
- `Subscribe`：开通订阅并保存当时的套餐条件快照；每账户一份有效订阅；订阅按月终止后可用同一入口重新开通。允许登记开通时刻晚于当前时刻的订阅：到达实际开通时刻前查询显示未生效（`Subscribed` 为 false、`CurrentTerms` 为零值，亦无待换套餐安排与待取消终止时刻），此期间再次开通按已有订阅拒绝，安排换套餐、登记取消返回订阅尚未生效的错误；以实际开通时刻为界（同一瞬间与时区无关），不按开通月份提前生效，到达后直接查询即显示登记时保存的完整套餐条件，等待期间修改套餐不改变该快照。旧订阅终止后登记未来重新开通的空档同样不显示生效套餐，历史用量与账单继续可查。
- `SchedulePlanChange` / `CancelPlanChange`：安排或取消从下一个 UTC 自然月起换用另一套餐；安排时锁定目标套餐完整条件快照，每账户至多一条待生效安排。
- `CancelSubscription` / `UndoCancelSubscription`：登记按月取消（终止时刻为请求时刻的下一个 UTC 自然月月初，当月仍按完整月费与额度计费）或在生效前撤回；重复取消返回原终止时刻。
- `RecordEvent`：上报用量，按发生时刻归入 UTC 自然月账期，事件时刻须落在某段实际订阅期间（开通计入、终止不计入）；账户内事件标识去重。账期结束后、出账前仍可补报晚到用量（详见下文“用量补报”）。
- `CreateBill` / `GetBill`：为已结束且被某段订阅覆盖的账期出账，完全无订阅的空档月份失败；重复出账得到同一张账单；每张账单按该账期当时适用的套餐条件计费。
- `RecordPayment`：分次登记付款，账户内付款标识去重（详见下文“付款登记”）。
- `MonthlyUsage` / `Status`：查询各月累计用量、当前生效套餐条件（订阅以实际开通时刻为界，等待开通期间为零值）、待生效安排、已安排的终止时刻、账单余额与欠费停用状态（等待开通不清旧欠费，`Subscribed` 与停用分别表示订阅是否生效与是否存在到期未结清账单）。月度历史列表的收录规则详见下文“月度历史查询”。
- `EstimateCurrentBill`：账期尚未结束时查询当前 UTC 自然月截至查询时刻的**预计账单**（结果以 `Estimated=true` 明确标为预估），不必先出账；纯读操作，不保存账单、不关闭当月、不产生欠费（详见下文“账期内预计费用查询”）。

金额与用量均为非负 `int64`，金额单位为分，税率为万分比（0–10000）。
到期欠费的账户会被停用，结清全部到期欠费账单后恢复；结清债务不复活已取消的订阅。
换套餐在生效月月初零点自动生效：安排当月仍按旧套餐收取完整月费、提供完整额度，
不按天补差、不迁移本月用量；已生成账单的金额与税额不因后续切换或取消而变化。

按月取消订阅在请求时刻的下一个 UTC 自然月月初零点终止订阅：当月照常接收用量并
按完整月计费，不按天退款；登记取消清除尚未生效的换套餐安排，等待取消期间拒绝新
安排，撤回取消不恢复被清除的安排。终止时刻到达即无有效订阅（无需上报用量或
出账），历史用量与账单继续可查；终止后可通过 `Subscribe` 重新开通（开通时刻不得
早于上次终止时刻），支持多次取消与重新开通，旧月份归属与计价不被后来订阅覆盖。

## 修改套餐定义与安排换套餐

`UpdatePlan` 与 `SchedulePlanChange` 是两类不同的操作，不要混淆：

- **修改套餐定义**（`UpdatePlan`）只影响之后新保存的条件快照：之后开通的
  订阅、之后新接受的换套餐安排会按修改后的定义取得条件。它不会直接改写
  任何账户正在使用的条件——已开通订阅继续使用开通时保存的快照；也不会
  改写此前已接受的换套餐安排——安排锁定的条件保持安排被接受时的取值。
- **安排换套餐**（`SchedulePlanChange`）针对具体账户：被接受时锁定目标
  套餐当时的完整条件（月费、包含额度、超额单价、税率）与生效账期（请求
  被接受时的下一个 UTC 自然月），之后修改套餐定义不影响该安排。

安排生效前再次安排同一目标套餐，返回的是原安排（`Created` 为 `false`），
不重新取价——即使套餐定义在此期间已被修改，也不要把它当成按新定义重新
安排。这一结果仅限于安排尚未生效时；安排生效后目标套餐已成为当前套餐，
再安排它仍按现有规则返回 `ErrPlanChangeSamePlan`。到达生效账期月初零点
（UTC）后，直接查询即显示安排锁定的条件，待生效安排消失，无须先上报
用量或生成账单。

账单只能在各自账期结束后生成；每张账单按该账期当时适用的条件计费：安排
当月仍按旧套餐收完整月费、给完整额度，生效月起按锁定的目标套餐条件计费，
修改后的套餐定义不会混入已锁定安排的计费。已生成的账单不因后续出账或
定义修改而变化。欠费停用规则保持不变：上一账期账单到期未结清会阻止上报
新用量，示例在付款截止后先正常登记付款再上报。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 注入可推进的
时钟，输出不依赖运行当天日期。账户 `acct-switch` 2026-01-01 00:00 UTC
开通 plan-a（月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%），
一月用量 15 单位；2026-01-15 12:00 UTC 安排二月换到 plan-b（当时定义为
月费 2000 分、包含 20 单位、超额单价 200 分、税率 6%）。随后 `UpdatePlan`
把 plan-b 改为月费 5000 分、包含 5 单位、超额单价 900 分、税率 25%，
再次安排同一目标仍返回原安排。二月用量 25 单位；一月账单应付 1650 分
（月费 1000 + 超额 5×100 + 税 150），二月账单按锁定的 plan-b 条件应付
3180 分（月费 2000 + 超额 5×200 + 税 180），修改后的定义没有混入这次
切换的计费。该示例以 Example 测试形式保存在
`meter/plan_change_example_test.go`，`go test ./...` 会校验其输出：

```go
const accountID = "acct-switch"
jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))
feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))

// 可推进的时钟：初始当前时刻为 2026-01-15 12:00 UTC（一月中旬），
// 之后逐段推进到二月、三月，复制后在任何真实日期运行结果都相同。
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
// 套餐 plan-b 当前定义：月费 2000 分、包含 20 单位、超额单价 200 分、税率 6%。
if err := s.CreatePlan(meter.Plan{
    ID: "plan-b", MonthlyFee: 2000, IncludedUnits: 20,
    OveragePrice: 200, TaxRateBasisPoints: 600,
}); err != nil {
    panic(err)
}
// 账户一月起使用 plan-a。
if err := s.Subscribe(accountID, "plan-a", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
    panic(err)
}

// 一月用量 15 单位，超出 plan-a 包含的 10 单位。
ev, err := s.RecordEvent(meter.Event{
    AccountID: accountID, EventID: "e-jan",
    At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 15,
})
if err != nil {
    panic(err)
}
fmt.Printf("event e-jan accepted=%t period=%s\n", ev.Accepted, ev.Period)

// 一月中旬安排二月换到 plan-b：安排被接受时锁定目标套餐当时的
// 完整条件与生效账期（请求被接受时的下一个 UTC 自然月）。
r, err := s.SchedulePlanChange(accountID, "plan-b")
if err != nil {
    panic(err)
}
fmt.Printf("schedule    created=%t target=%s effective=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d\n",
    r.Created, r.Change.TargetPlanID, r.Change.EffectivePeriod,
    r.Change.Terms.MonthlyFee, r.Change.Terms.IncludedUnits,
    r.Change.Terms.OveragePrice, r.Change.Terms.TaxRateBasisPoints)

// 安排当月仍按旧套餐：查询仍显示 plan-a，待生效安排为 plan-b。
st, err := s.Status(accountID)
if err != nil {
    panic(err)
}
fmt.Printf("status      current=%s monthlyFee=%d pending=%s pendingEffective=%s\n",
    st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
    st.PendingChange.TargetPlanID, st.PendingChange.EffectivePeriod)

// 修改 plan-b 的定义：月费、包含额度、超额单价、税率全部改变。
// 修改只影响之后新保存的条件快照，不改写已接受的安排。
if err := s.UpdatePlan(meter.Plan{
    ID: "plan-b", MonthlyFee: 5000, IncludedUnits: 5,
    OveragePrice: 900, TaxRateBasisPoints: 2500,
}); err != nil {
    panic(err)
}

// 再次安排同一个目标 plan-b：返回原安排，Created=false，条件仍是
// 首次安排时锁定的快照，不按修改后的定义重新取价。
r, err = s.SchedulePlanChange(accountID, "plan-b")
if err != nil {
    panic(err)
}
fmt.Printf("reschedule  created=%t target=%s effective=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d\n",
    r.Created, r.Change.TargetPlanID, r.Change.EffectivePeriod,
    r.Change.Terms.MonthlyFee, r.Change.Terms.IncludedUnits,
    r.Change.Terms.OveragePrice, r.Change.Terms.TaxRateBasisPoints)

// 待生效安排的条件也没有被修改后的定义改写。
st, err = s.Status(accountID)
if err != nil {
    panic(err)
}
fmt.Printf("after update current=%s pending=%s pendingMonthlyFee=%d pendingTaxRateBasisPoints=%d\n",
    st.CurrentTerms.PlanID, st.PendingChange.TargetPlanID,
    st.PendingChange.Terms.MonthlyFee, st.PendingChange.Terms.TaxRateBasisPoints)

// 推进到 2026-02-01 00:00 UTC（二月月初零点）：安排自动生效，
// 直接查询即显示锁定的 plan-b 条件，待生效安排消失，
// 无须先上报用量或生成账单。
now = mustParseTime("2026-02-01T00:00:00Z")
st, err = s.Status(accountID)
if err != nil {
    panic(err)
}
fmt.Printf("status      current=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d pending=%v\n",
    st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
    st.CurrentTerms.IncludedUnits, st.CurrentTerms.OveragePrice,
    st.CurrentTerms.TaxRateBasisPoints, st.PendingChange)

// “重复安排返回原安排”仅限于安排尚未生效时：生效后 plan-b 已是
// 当前套餐，再安排它按现有规则返回同套餐错误。
_, err = s.SchedulePlanChange(accountID, "plan-b")
fmt.Printf("same plan   errSamePlan=%t\n", errors.Is(err, meter.ErrPlanChangeSamePlan))

// 一月账期已结束，为一月出账：仍按 plan-a 收完整月费、给完整额度。
// 用量 15，超额 5 × 100 = 500 分；税 (1000+500) × 10% = 150 分。
janBill, err := s.CreateBill(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("bill jan    plan=%s totalUsage=%d monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
    janBill.Terms.PlanID, janBill.TotalUsage, janBill.MonthlyFee,
    janBill.IncludedUnits, janBill.OverageUnits, janBill.OverageFee,
    janBill.Tax, janBill.TotalDue, janBill.DueAt.Format(time.RFC3339))

// 一月账单截止 2026-02-08 00:00 UTC。当前时刻推进到 2 月 10 日：
// 若不结清，欠费停用会阻止二月上报新用量，因此先正常登记付款。
now = mustParseTime("2026-02-10T00:00:00Z")
pay, err := s.RecordPayment(accountID, "pay-jan", jan, janBill.TotalDue)
if err != nil {
    panic(err)
}
fmt.Printf("payment     registered=%t paymentID=%s amount=%d billBalance=%d settled=%t\n",
    pay.Registered, pay.Payment.PaymentID, pay.Payment.Amount, pay.BillBalance, pay.Settled)

// 二月用量 25 单位，超出锁定的 plan-b 包含的 20 单位。
ev, err = s.RecordEvent(meter.Event{
    AccountID: accountID, EventID: "e-feb",
    At: mustParseTime("2026-02-10T00:00:00Z"), Quantity: 25,
})
if err != nil {
    panic(err)
}
fmt.Printf("event e-feb accepted=%t period=%s\n", ev.Accepted, ev.Period)

// 二月账期结束后为二月出账：按安排时锁定的 plan-b 条件计费
// （月费 2000 分，不是修改后的 5000 分）。用量 25，超额
// 5 × 200 = 1000 分；税 (2000+1000) × 6% = 180 分。
now = mustParseTime("2026-03-01T00:00:00Z")
febBill, err := s.CreateBill(accountID, feb)
if err != nil {
    panic(err)
}
fmt.Printf("bill feb    plan=%s totalUsage=%d monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
    febBill.Terms.PlanID, febBill.TotalUsage, febBill.MonthlyFee,
    febBill.IncludedUnits, febBill.OverageUnits, febBill.OverageFee,
    febBill.Tax, febBill.TotalDue, febBill.DueAt.Format(time.RFC3339))

// 二月出账后查询一月账单：超额费用、税额与总应付保持原样。
janAgain, err := s.GetBill(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("get bill jan plan=%s totalUsage=%d overageFee=%d tax=%d totalDue=%d\n",
    janAgain.Terms.PlanID, janAgain.TotalUsage, janAgain.OverageFee,
    janAgain.Tax, janAgain.TotalDue)
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
event e-jan accepted=true period=2026-01
schedule    created=true target=plan-b effective=2026-02 monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600
status      current=plan-a monthlyFee=1000 pending=plan-b pendingEffective=2026-02
reschedule  created=false target=plan-b effective=2026-02 monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600
after update current=plan-a pending=plan-b pendingMonthlyFee=2000 pendingTaxRateBasisPoints=600
status      current=plan-b monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600 pending=<nil>
same plan   errSamePlan=true
bill jan    plan=plan-a totalUsage=15 monthlyFee=1000 includedUnits=10 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
payment     registered=true paymentID=pay-jan amount=1650 billBalance=0 settled=true
event e-feb accepted=true period=2026-02
bill feb    plan=plan-b totalUsage=25 monthlyFee=2000 includedUnits=20 overageUnits=5 overageFee=1000 tax=180 totalDue=3180 dueAt=2026-03-08T00:00:00Z
get bill jan plan=plan-a totalUsage=15 overageFee=500 tax=150 totalDue=1650
```

## 提前登记订阅：“已经登记”不等于“已经生效”

`Subscribe` 允许登记开通时刻晚于当前时刻的订阅，但登记成功只表示这份订阅
**已经登记**，不表示**已经生效**。到达登记的开通时刻前，`Status` 始终显示
没有生效订阅：

- `Subscribed` 为 **false**，`CurrentTerms` 为**零值**，待换套餐安排
  （`PendingChange`）与已安排的终止时刻（`ScheduledEnd`）均为空。
- 这份登记仍然存在：等待期间再次调用 `Subscribe` 为该账户开通，得到
  `ErrSubscriptionExists`，原登记原样保留。**不要因为看到 `Subscribed` 为
  false 就当成“没开通”而重复开通**——它表示“尚未到生效时刻”，不是“登记
  丢失”。安排换套餐、登记取消在等待期间也按已有订阅处理，返回
  `ErrSubscriptionNotActivated`。
- 生效以**登记的开通时刻这一瞬间**为界，不按开通月份的月初提前生效：即使
  当前时刻已经是开通当天零点，只要还没到登记的具体时刻，`Subscribed` 仍为
  false。时刻按**同一瞬间**比较，与输入使用的时区和本地钟面无关：把开通
  时刻写成东八区的 2026-01-20 20:00（与 UTC 12:00 是同一瞬间），结果与
  写 2026-01-20T12:00:00Z 完全相同，不会按本地钟面或本地月份另行判断。
- 到达开通时刻后**直接查询**即显示订阅已生效，无须先上报用量或生成账单；
  显示的四项计费条件全部是**首次登记时保存的快照**，等待期间用
  `UpdatePlan` 修改套餐定义不会混入。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 注入可推进的
时钟，复制后不需要等待任何真实日期。当前时刻固定为 2026-01-15 09:00 UTC，
账户 `acct-future` 登记在 2026-01-20 12:00 UTC 开通 plan-future；登记时
套餐月费 1000 分、包含 10 单位、超额单价 100 分、税率 600（万分比，即
6%）。登记成功后把同一套餐的定义改为月费 2000 分、包含 20 单位、超额单价
200 分、税率 1000（10%）。等待期间查询显示未生效，再次开通得到
`ErrSubscriptionExists` 且原登记保留；推进到开通当天零点仍未生效；恰好
到达 12:00 UTC 后查询显示已生效，四项条件仍是首次登记时的
1000/10/100/600。最后用独立的服务把同一场景以“东八区 2026-01-20
20:00 开通、东八区时钟观察”重演一遍，当地 19:59 尚未生效、当地 20:00
（即 UTC 12:00）生效，条件与 UTC 写法完全一致。正常步骤失败即 `panic`
停止；重复开通的错误是预期结果，代码显式判断并打印，不会忽略错误后继续
输出成功状态。同样的流程以 Example 测试形式保存在
`meter/future_activation_example_test.go`，`go test ./...` 会校验其输出。
下面是自包含程序，保存为仓库根目录下的 `main.go` 后 `go run main.go`
即可得到文末输出：

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

func mustParseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

func main() {
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
}
```

输出：

```text
waiting     subscribed=false currentPlan="" monthlyFee=0 includedUnits=0 overagePrice=0 taxRateBasisPoints=0 pendingChange=<nil> scheduledEnd=<nil>
duplicate   errSubscriptionExists=true
after dup   subscribed=false currentPlan="" pendingChange=<nil> scheduledEnd=<nil>
month start subscribed=false currentPlan=""
activated   subscribed=true currentPlan=plan-future monthlyFee=1000 includedUnits=10 overagePrice=100 taxRateBasisPoints=600 pendingChange=<nil> scheduledEnd=<nil>
tz waiting  local=2026-01-20 19:59 +0800 subscribed=false currentPlan=""
tz instant  local=2026-01-20 20:00 +0800 subscribed=true currentPlan=plan-future monthlyFee=1000 includedUnits=10 overagePrice=100 taxRateBasisPoints=600
```

对照输出可以确认：前三行与 `month start` 行都是“已经登记、尚未生效”，
此时 `Subscribed=false` 是正常等待状态，重复开通只得到
`errSubscriptionExists=true`；`activated` 行才是“已经生效”，条件取首次
登记的快照而不是修改后的定义；最后两行证明生效判断只认同一瞬间——东八区
钟面 20:00 与 UTC 12:00 结果一致，与开通月份月初、本地钟面都无关。

## 按月取消订阅

`CancelSubscription` 把取消请求、实际终止和当月账单连在一起：登记取消只是
安排终止时刻，订阅在终止时刻前继续有效；到达终止时刻订阅自动结束；取消当月
的账单仍按完整月出账。

- **终止时刻**为请求时刻的下一个 UTC 自然月月初零点（UTC）。它按请求时刻
  所在的 UTC 自然月计算，不能按调用者看到的本地月份解释：同一瞬间换任何
  时区表示，结果都相同（例如本地钟面已跨入下一个月、实际仍是 UTC 当月末
  尾时，终止时刻仍取 UTC 当月的下一月月初，不会按本地月份再往后推）。
- **等待终止期间**查询仍显示订阅有效（`Subscribed` 为 true）、保留原套餐
  条件，并通过 `Status` 的 `ScheduledEnd` 展示已安排的终止时刻；当月照常
  接收用量。再次取消不会新建安排，返回原终止时刻且 `Cancelled` 为
  `false`——这不是取消失败，也不会把期限后移。生效前可用
  `UndoCancelSubscription` 撤回。
- **到达终止时刻**后直接查询即显示订阅已终止（`Subscribed` 为 false）、
  当前套餐为空、不再展示终止安排，无须先上报用量或生成账单。
- **取消当月仍出完整账**：月费按整月收取、包含额度按整月提供，超额与税额
  照常计算，不按天退款；取消不会自动生成账单，也不会免除应付款项，付款
  截止仍为账期结束后七天。历史用量与账单在终止后继续可查。

两处容易误用的边界：

- 提前登记但尚未到实际开通时刻的订阅不能取消，`CancelSubscription` 返回
  `ErrSubscriptionNotActivated`（订阅尚未生效）。
- 没有重新开通时，恰在终止时刻发生的新用量不属于旧订阅（终止时刻不计入
  订阅期间），`RecordEvent` 返回 `ErrEventBeforeSubscription`，且不增加
  任何月的累计。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 注入可推进的
时钟，输出不依赖运行当天日期。账户 `acct-cancel` 2026-01-01 00:00 UTC
开通 plan-a（月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%），
没有欠费。2026-01-15 登记取消，返回的终止时刻为 2026-02-01 00:00 UTC；
等待期间再次取消仍返回原终止时刻；登记取消后一月用量照常接收，累计 15
单位。到达 2026-02-01 00:00 UTC 后直接查询即显示订阅已终止；为一月出账：
用量 15、超额费用 500 分、税额 150 分、应付 1650 分，付款截止仍为
2026-02-08 00:00 UTC。该示例以 Example 测试形式保存在
`meter/cancel_example_test.go`，`go test ./...` 会校验其输出：

```go
const accountID = "acct-cancel"
jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))
feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))

// 可推进的时钟：初始当前时刻为 2026-01-15 12:00 UTC（一月中旬），
// 之后逐段推进到二月，复制后在任何真实日期运行结果都相同。
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
// 账户 2026-01-01 00:00 UTC 开通，当前无欠费。
if err := s.Subscribe(accountID, "plan-a", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
    panic(err)
}

// 1 月 15 日登记按月取消：终止时刻为请求时刻的下一个 UTC 自然月
// 月初零点，即 2026-02-01 00:00 UTC。
r, err := s.CancelSubscription(accountID)
if err != nil {
    panic(err)
}
fmt.Printf("cancel      cancelled=%t endAt=%s\n",
    r.Cancelled, r.Cancellation.EndAt.Format(time.RFC3339))

// 登记取消后、到达终止时刻前：查询仍显示订阅有效、保留原套餐条件，
// 并展示已安排的终止时刻。
st, err := s.Status(accountID)
if err != nil {
    panic(err)
}
fmt.Printf("status      subscribed=%t current=%s monthlyFee=%d scheduledEnd=%s\n",
    st.Subscribed, st.CurrentTerms.PlanID, st.CurrentTerms.MonthlyFee,
    st.ScheduledEnd.Format(time.RFC3339))

// 等待终止期间再次取消：本次没有新建安排（Cancelled=false），仍返回
// 原终止时刻。这不是取消失败，也不会把期限后移。
now = mustParseTime("2026-01-20T09:00:00Z")
r, err = s.CancelSubscription(accountID)
if err != nil {
    panic(err)
}
fmt.Printf("recancel    cancelled=%t endAt=%s\n",
    r.Cancelled, r.Cancellation.EndAt.Format(time.RFC3339))

// 取消登记后仍能接收一月的用量：一月累计达到 15 单位。
now = mustParseTime("2026-01-25T12:00:00Z")
ev, err := s.RecordEvent(meter.Event{
    AccountID: accountID, EventID: "e-jan",
    At: mustParseTime("2026-01-25T00:00:00Z"), Quantity: 15,
})
if err != nil {
    panic(err)
}
fmt.Printf("event e-jan accepted=%t period=%s\n", ev.Accepted, ev.Period)

usage, err := s.MonthlyUsage(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

// 终止时刻按请求时刻所在的 UTC 自然月计算，不能按调用者看到的本地
// 月份解释：当前时刻用 UTC+8 表示为 2026-02-01 07:30（本地钟面已
// 跨入二月），实际仍是 UTC 2026-01-31 23:30，终止时刻仍取 UTC
// 一月的下一月月初 2026-02-01 00:00 UTC，而不是按本地二月再往后
// 推一个月。
plus8 := time.FixedZone("UTC+8", 8*60*60)
now = time.Date(2026, time.February, 1, 7, 30, 0, 0, plus8)
if err := s.CreateAccount("acct-tz"); err != nil {
    panic(err)
}
if err := s.Subscribe("acct-tz", "plan-a", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
    panic(err)
}
r, err = s.CancelSubscription("acct-tz")
if err != nil {
    panic(err)
}
fmt.Printf("cancel tz+8 local=%s endAt=%s\n",
    now.Format("2006-01-02 15:04 -0700"), r.Cancellation.EndAt.Format(time.RFC3339))

// 推进到 2026-02-01 00:00 UTC（终止时刻）：直接查询即显示订阅已
// 终止、当前套餐为空、不再展示终止安排，无须先上报用量或生成账单。
now = mustParseTime("2026-02-01T00:00:00Z")
st, err = s.Status(accountID)
if err != nil {
    panic(err)
}
fmt.Printf("status      subscribed=%t currentPlan=%q scheduledEnd=%v\n",
    st.Subscribed, st.CurrentTerms.PlanID, st.ScheduledEnd)

// 取消不会自动生成账单：一月账期虽已结束，尚未出账。
_, err = s.GetBill(accountID, jan)
fmt.Printf("get bill jan errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))

// 没有重新开通时，恰在终止时刻发生的新用量不属于旧订阅：被拒绝
// （ErrEventBeforeSubscription），不增加累计。
_, err = s.RecordEvent(meter.Event{
    AccountID: accountID, EventID: "e-at-end",
    At: mustParseTime("2026-02-01T00:00:00Z"), Quantity: 1,
})
fmt.Printf("event e-at-end errEventBeforeSubscription=%t\n",
    errors.Is(err, meter.ErrEventBeforeSubscription))

// 被拒绝的事件没有累计：一月仍是 15，二月为零。
usage, err = s.MonthlyUsage(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)
usage, err = s.MonthlyUsage(accountID, feb)
if err != nil {
    panic(err)
}
fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

// 为一月出账：取消当月仍收完整月费、提供完整额度，不按天退款。
// 用量 15，超额 5 × 100 = 500 分；税 (1000+500) × 10% = 150 分；
// 应付 1650 分。付款截止仍为账期结束后七天，即 2026-02-08 00:00
// UTC，取消不会免除应付款项。
janBill, err := s.CreateBill(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("bill jan    plan=%s totalUsage=%d monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d tax=%d totalDue=%d dueAt=%s\n",
    janBill.Terms.PlanID, janBill.TotalUsage, janBill.MonthlyFee,
    janBill.IncludedUnits, janBill.OverageUnits, janBill.OverageFee,
    janBill.Tax, janBill.TotalDue, janBill.DueAt.Format(time.RFC3339))

// 订阅终止后历史账单仍然可查：应付与余额保持原样。
janAgain, err := s.GetBill(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("get bill jan plan=%s totalUsage=%d totalDue=%d balance=%d settled=%t\n",
    janAgain.Terms.PlanID, janAgain.TotalUsage, janAgain.TotalDue,
    janAgain.Balance, janAgain.Settled)

// 边界一：提前登记但尚未到实际开通时刻的订阅不能取消。当前时刻为
// 2026-02-01 00:00 UTC，acct-future 的开通时刻登记为 2 月 10 日，
// 取消请求返回订阅尚未生效的错误。
if err := s.CreateAccount("acct-future"); err != nil {
    panic(err)
}
if err := s.Subscribe("acct-future", "plan-a", mustParseTime("2026-02-10T00:00:00Z")); err != nil {
    panic(err)
}
_, err = s.CancelSubscription("acct-future")
fmt.Printf("cancel future errSubscriptionNotActivated=%t\n",
    errors.Is(err, meter.ErrSubscriptionNotActivated))
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
cancel      cancelled=true endAt=2026-02-01T00:00:00Z
status      subscribed=true current=plan-a monthlyFee=1000 scheduledEnd=2026-02-01T00:00:00Z
recancel    cancelled=false endAt=2026-02-01T00:00:00Z
event e-jan accepted=true period=2026-01
usage       period=2026-01 total=15
cancel tz+8 local=2026-02-01 07:30 +0800 endAt=2026-02-01T00:00:00Z
status      subscribed=false currentPlan="" scheduledEnd=<nil>
get bill jan errBillNotFound=true
event e-at-end errEventBeforeSubscription=true
usage       period=2026-01 total=15
usage       period=2026-02 total=0
bill jan    plan=plan-a totalUsage=15 monthlyFee=1000 includedUnits=10 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
get bill jan plan=plan-a totalUsage=15 totalDue=1650 balance=1650 settled=false
cancel future errSubscriptionNotActivated=true
```

## 重新开通后补开旧账单

订阅按月取消并到达终止时刻后，可以用同一个 `Subscribe` 入口重新开通
（开通时刻不得早于上次终止时刻），终止前尚未出账的旧月份之后仍可补开。
两份条件快照各归各次开通，互不覆盖：

- **当前账户条件取自重新开通。** 重新开通保存的是重新开通当时的套餐
  定义快照，它成为 `Status` 的 `CurrentTerms` 与之后各月出账的依据。
  重新开通沿用同一个套餐标识也一样：标识相同不代表条件相同，快照在
  开通那一刻固定，不能把新价格套到旧月份。
- **旧账期条件取自最初开通。** 为旧订阅覆盖的账期补开账单时，计价用
  该账期当时适用的条件快照，即最初开通（或当时已生效的换套餐安排）
  保存的条件。即使套餐定义在空档期间已经改价、甚至已经用新价格重新
  开通，旧账单仍按旧条件计算；反过来，补开旧账单也不会把账户当前
  套餐改回旧价格。

两点需要特别注意：

- **付款截止不顺延，补开可能当即欠费停用。** 旧账单的 `DueAt` 仍是
  其账期结束后七天（UTC），从账期结束时刻算起，与何时补开无关。若
  补开时截止时刻已过且账单未结清，账户立即进入欠费停用，
  `RecordEvent` 对新事件返回 `ErrSuspended`，结清全部到期欠费后
  恢复。
- **空档月份不能出账。** 两段订阅之间完全无订阅的月份不被任何订阅
  覆盖，`CreateBill` 返回 `ErrBillBeforeSubscription`——这个错误
  不只表示“早于首次开通”，也用于两段订阅之间的空档月份；相应地
  `GetBill` 返回 `ErrBillNotFound`。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 注入可
推进的时钟，把“当前时刻”沿时间线逐段推进，输出固定、不依赖运行
当天日期。账户 `acct-reopen` 与套餐 `plan-flex` 全程复用：
2026-01-01 00:00 UTC 开通时套餐月费 1000 分、包含 10 单位、超额
单价 100 分、税率 10%；一月上报 15 单位，1 月 15 日登记取消，
2 月 1 日零点终止，一月暂不出账；二月整月没有订阅，期间把套餐改为
月费 2000 分、包含 20 单位、超额单价 200 分、税率 6%；3 月 15 日
重新开通，三月上报 25 单位；4 月 1 日先为三月出账，再补开一月账单。
所有用量都发生在各自已经生效的订阅期间内，接收用量时账户没有到期
未结清账单。示例依次展示：重新开通后当前条件是新快照；三月账单按
新条件收完整月费与完整额度（月中开通不折算），超额 5 单位、超额费
1000 分、税 180 分、应付 3180 分，截止 2026-04-08 00:00 UTC；一月
账单仍按最初条件计，超额费 500 分、税 150 分、应付 1650 分，截止
仍是 2026-02-08 00:00 UTC，查询与出账结果一致；补开旧账当即带来
欠费停用，而当前套餐条件保持新价格不变；二月空档出账与查询分别
返回 `ErrBillBeforeSubscription` 与 `ErrBillNotFound`。示例中正常
操作失败直接 panic，只有这两个空档错误是预期结果。该示例以
Example 测试形式保存在 `meter/resubscribe_backbill_example_test.go`，
`go test ./...` 会校验其输出：

```go
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
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
event e-jan accepted=true period=2026-01
cancel      cancelled=true endAt=2026-02-01T00:00:00Z
resubscribe subscribed=true plan=plan-flex monthlyFee=2000 includedUnits=20 overagePrice=200 taxBps=600
event e-mar accepted=true period=2026-03
bill mar    period=2026-03 plan=plan-flex monthlyFee=2000 includedUnits=20 overagePrice=200 taxBps=600
bill mar    totalUsage=25 overageUnits=5 overageFee=1000 tax=180 totalDue=3180 dueAt=2026-04-08T00:00:00Z
status      suspended=false
bill jan    period=2026-01 plan=plan-flex monthlyFee=1000 includedUnits=10 overagePrice=100 taxBps=1000
bill jan    totalUsage=15 overageUnits=5 overageFee=500 tax=150 totalDue=1650 dueAt=2026-02-08T00:00:00Z
get bill jan plan=plan-flex totalUsage=15 totalDue=1650 balance=1650 settled=false dueAt=2026-02-08T00:00:00Z
status      subscribed=true suspended=true plan=plan-flex monthlyFee=2000
bill feb    errBillBeforeSubscription=true
get bill feb errBillNotFound=true
```

## 单张账单的金额计算

`CreateBill` 为单个已经结束的 UTC 自然月账期出一张账单，调用者可用账单
上的明细自行核对税额与应付金额。计价只看该账期的**整月累计用量**与当时
适用的套餐条件，与事件分几次上报无关，也不对每条用量事件单独算钱算税：

- **金额单位为分**，所有金额都是非负 `int64`；用量按**整个 UTC 自然月**
  累计，账单 `TotalUsage` 是该账期内全部已接收事件数量之和，出账后固定。
- **包含额度按整月给、月费按整月收。** 超额单位为
  `max(0, TotalUsage - IncludedUnits)`：整月总用量没有超过包含额度时，
  超额费为零；超过时只对**超出的单位**按套餐约定的超额单价计费
  （`OverageFee = OverageUnits × OveragePrice`）。无论用多用少，哪怕用量
  为零，月费都完整计入，额度用不完也不抵扣月费、不退费。
- **用账单所属月份适用的套餐条件计税。** 条件来自订阅开通时保存的快照、
  或某次已生效换套餐安排锁定的快照；出账之后再用 `UpdatePlan` 修改套餐
  定义，不会改变这张账单（包括税率）。
- **税率是万分比**，`TaxRateBasisPoints=1000` 表示 10%（取值 0–10000）。
- **月费与超额费先相加，再对合计金额计算一次税额**：
  `Tax = 四舍五入到整分((MonthlyFee + OverageFee) × TaxRateBasisPoints / 10000)`，
  不足半分舍去、**恰好半分向上进一分**；`TotalDue = MonthlyFee + OverageFee + Tax`。
  不分别给月费、超额费各算一次税再各自舍入，也不对每条用量事件单独计税。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 把当前时刻
固定在 2026-02-01 00:00 UTC，使 2026-01 成为已经结束的账期，复制后不
依赖运行当天的日期。主账户 `acct-bill` 的套餐为月费 3 分、包含 2 单位、
超额单价 3 分、税率 1000（10%），订阅与事件时刻都写死在一月，当月累计
用量 3 单位：超额 1 单位、超额费 3 分、税前合计 6 分、税额 1 分、应付
7 分。账期结束后还修改了一次套餐定义，账单仍按一月适用的开通快照计价。
`acct-bill-within` 使用同套餐但只用 2 单位（恰好用完额度），用来对照
“没有超额时超额费为零、月费仍完整计入”；`acct-round-down` 与
`acct-round-half` 分别是月费 4 分、5 分且税率均为 1000 的零用量账单，
展示不足半分与恰好半分的区别。示例每一步失败都立即 `panic` 停止，不会
继续输出貌似成功的账单。该示例以 Example 测试形式保存在
`meter/bill_amount_example_test.go`，`go test ./...` 会校验其输出：

```go
jan := meter.MonthOf(mustParseTime("2026-01-01T00:00:00Z"))

// 把当前时刻固定在 2026-02-01 00:00 UTC：2026-01 账期恰好结束，
// 可以立即为一月出账；复制后在任何真实日期运行结果都相同。
now := mustParseTime("2026-02-01T00:00:00Z")
s := meter.NewServiceWithClock(func() time.Time { return now })

// 主示例套餐 plan-small：月费 3 分、包含 2 单位、超额单价 3 分、
// 税率 1000（万分比，即 10%）。
if err := s.CreatePlan(meter.Plan{
    ID: "plan-small", MonthlyFee: 3, IncludedUnits: 2,
    OveragePrice: 3, TaxRateBasisPoints: 1000,
}); err != nil {
    panic(err)
}
// 两个只有月费的套餐，用于展示税前 4 分、5 分在 10% 税率下的舍入：
// 0.4 分不足半分舍去，0.5 分恰好半分向上。
if err := s.CreatePlan(meter.Plan{
    ID: "plan-round-down", MonthlyFee: 4, IncludedUnits: 0,
    OveragePrice: 0, TaxRateBasisPoints: 1000,
}); err != nil {
    panic(err)
}
if err := s.CreatePlan(meter.Plan{
    ID: "plan-round-half", MonthlyFee: 5, IncludedUnits: 0,
    OveragePrice: 0, TaxRateBasisPoints: 1000,
}); err != nil {
    panic(err)
}

for _, id := range []string{
    "acct-bill", "acct-bill-within", "acct-round-down", "acct-round-half",
} {
    if err := s.CreateAccount(id); err != nil {
        panic(err)
    }
}

// 订阅时刻明确：四个账户都在 2026-01-01 00:00 UTC 开通，一月整月
// 被订阅覆盖，首月按完整月费与完整额度计费。
if err := s.Subscribe("acct-bill", "plan-small",
    mustParseTime("2026-01-01T00:00:00Z")); err != nil {
    panic(err)
}
if err := s.Subscribe("acct-bill-within", "plan-small",
    mustParseTime("2026-01-01T00:00:00Z")); err != nil {
    panic(err)
}
if err := s.Subscribe("acct-round-down", "plan-round-down",
    mustParseTime("2026-01-01T00:00:00Z")); err != nil {
    panic(err)
}
if err := s.Subscribe("acct-round-half", "plan-round-half",
    mustParseTime("2026-01-01T00:00:00Z")); err != nil {
    panic(err)
}

// 事件时刻明确：acct-bill 在 1 月 10 日上报 3 单位，按发生时刻
// 全部归入 2026-01 账期，当月累计为 3。
ev, err := s.RecordEvent(meter.Event{
    AccountID: "acct-bill", EventID: "e1",
    At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 3,
})
if err != nil {
    panic(err)
}
fmt.Printf("event over   accepted=%t period=%s\n", ev.Accepted, ev.Period)
usage, err := s.MonthlyUsage("acct-bill", jan)
if err != nil {
    panic(err)
}
fmt.Printf("usage over   period=%s total=%d\n", usage.Period, usage.Total)

// acct-bill-within 只用 2 单位，恰好等于包含额度，没有超额。
ev, err = s.RecordEvent(meter.Event{
    AccountID: "acct-bill-within", EventID: "e1",
    At: mustParseTime("2026-01-11T00:00:00Z"), Quantity: 2,
})
if err != nil {
    panic(err)
}
fmt.Printf("event within accepted=%t period=%s\n", ev.Accepted, ev.Period)
usage, err = s.MonthlyUsage("acct-bill-within", jan)
if err != nil {
    panic(err)
}
fmt.Printf("usage within period=%s total=%d\n", usage.Period, usage.Total)

// 账期结束后修改 plan-small 的定义：一月账单仍按一月适用的开通
// 快照计费，不套用修改后的定义。
if err := s.UpdatePlan(meter.Plan{
    ID: "plan-small", MonthlyFee: 9, IncludedUnits: 0,
    OveragePrice: 9, TaxRateBasisPoints: 0,
}); err != nil {
    panic(err)
}

// 为一月出账（当前时刻固定在 2026-02-01 00:00 UTC，账期已结束）：
// 用量 3，超额 3-2=1 单位；超额费 1×3=3 分；月费 3 分完整计入；
// 税前合计 3+3=6；税额对合计只算一次：6×1000/10000=0.6 分，
// 四舍五入为 1 分；应付 6+1=7 分。不能分别给两项 3 分费用各算
// 一次税：0.3+0.3 会被分别舍成 0+0。
bill, err := s.CreateBill("acct-bill", jan)
if err != nil {
    panic(err)
}
fmt.Printf("bill over   plan=%s monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d subtotal=%d taxRateBasisPoints=%d tax=%d totalDue=%d dueAt=%s\n",
    bill.Terms.PlanID, bill.MonthlyFee, bill.IncludedUnits,
    bill.OverageUnits, bill.OverageFee, bill.MonthlyFee+bill.OverageFee,
    bill.Terms.TaxRateBasisPoints, bill.Tax, bill.TotalDue,
    bill.DueAt.Format(time.RFC3339))

// 恰好用完包含额度：超额单位 0、超额费 0，月费 3 分仍完整计入；
// 税前 3 分，3×10%=0.3 不足半分，税额 0，应付 3 分。
within, err := s.CreateBill("acct-bill-within", jan)
if err != nil {
    panic(err)
}
fmt.Printf("bill within plan=%s monthlyFee=%d includedUnits=%d overageUnits=%d overageFee=%d subtotal=%d taxRateBasisPoints=%d tax=%d totalDue=%d dueAt=%s\n",
    within.Terms.PlanID, within.MonthlyFee, within.IncludedUnits,
    within.OverageUnits, within.OverageFee, within.MonthlyFee+within.OverageFee,
    within.Terms.TaxRateBasisPoints, within.Tax, within.TotalDue,
    within.DueAt.Format(time.RFC3339))

// 税前 4 分：4×10%=0.4 分，不足半分舍去，税额 0，应付 4 分。
down, err := s.CreateBill("acct-round-down", jan)
if err != nil {
    panic(err)
}
fmt.Printf("bill down   plan=%s subtotal=%d taxRateBasisPoints=%d tax=%d totalDue=%d\n",
    down.Terms.PlanID, down.MonthlyFee+down.OverageFee,
    down.Terms.TaxRateBasisPoints, down.Tax, down.TotalDue)

// 税前 5 分：5×10%=0.5 分，恰好半分向上，税额 1，应付 6 分。
half, err := s.CreateBill("acct-round-half", jan)
if err != nil {
    panic(err)
}
fmt.Printf("bill half   plan=%s subtotal=%d taxRateBasisPoints=%d tax=%d totalDue=%d\n",
    half.Terms.PlanID, half.MonthlyFee+half.OverageFee,
    half.Terms.TaxRateBasisPoints, half.Tax, half.TotalDue)
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
event over   accepted=true period=2026-01
usage over   period=2026-01 total=3
event within accepted=true period=2026-01
usage within period=2026-01 total=2
bill over   plan=plan-small monthlyFee=3 includedUnits=2 overageUnits=1 overageFee=3 subtotal=6 taxRateBasisPoints=1000 tax=1 totalDue=7 dueAt=2026-02-08T00:00:00Z
bill within plan=plan-small monthlyFee=3 includedUnits=2 overageUnits=0 overageFee=0 subtotal=3 taxRateBasisPoints=1000 tax=0 totalDue=3 dueAt=2026-02-08T00:00:00Z
bill down   plan=plan-round-down subtotal=4 taxRateBasisPoints=1000 tax=0 totalDue=4
bill half   plan=plan-round-half subtotal=5 taxRateBasisPoints=1000 tax=1 totalDue=6
```

主示例里“必须合并计税、只舍入一次”可以直接用数字核对：月费 3 分与
超额费 3 分各自的 10% 都是 0.3 分，若错误地分别四舍五入，两项税各为
0，合计税额 0、应付 6 分；正确做法是先把两项费用相加得到 6 分，再对
6 分计一次税：0.6 分向上取整为 1 分，应付 7 分。旁边两个小账单把舍入
边界单独摆出来：同样 10% 税率，税前 4 分的税是 0.4 分（不足半分，舍
去 → 税额 0、应付 4 分），税前 5 分的税是 0.5 分（恰好半分，向上 →
税额 1、应付 6 分）。

### 金额超出 int64 时

超额费用（`OverageUnits × OveragePrice`）、税前合计（`MonthlyFee +
OverageFee`）或含税应付（税前合计 + 税额）中任何一项超出非负 `int64`
范围时，`CreateBill` 返回可通过 `errors.Is(err, meter.ErrOverflow)`
识别的 `ErrOverflow`，并返回零值账单：**不会保存账单**（随后 `GetBill`
仍是 `ErrBillNotFound`，该账期没有被当成已出账而关闭，也不会凭空成为
欠费来源；重复出账仍是同样的溢出错误），**已有用量保持原值**不变，
账期仍开放、可以继续补报用量。例如月费取 `int64` 上限、再产生 1 分
超额费时税前合计溢出；月费 9000000000000000000 分、税率 1000 时税前
合法，但税额 900000000000000000 分与税前合计之和超过上限，同样返回
`ErrOverflow`。

需要特别区分**计税中间数**与最终金额：税前金额乘税率的数学乘积本身
可能超过 `int64` 上限，但这**不代表出账必然失败**。计税按“商与余数
拆分”的方式完成，不依赖那个巨大的中间乘积；只要最终的超额费、税前
合计、税额与应付金额都能表示为非负 `int64`，当前功能仍能按分精确
出账，不误报溢出、也不损失精度。例如税前 8000000000000000005 分、
税率 1000 时，直接相乘约为 8×10²¹（远超 `int64` 上限
9223372036854775807），出账仍正常完成：税额 800000000000000001 分、
应付 8800000000000000006 分。这些边界由
`meter/bill_large_amounts_test.go` 与
`meter/bill_overflow_late_usage_test.go` 持续校验。

## 账期内预计费用查询

`EstimateCurrentBill` 在账期尚未结束时回答“**按现在已经接收的用量，本月
预计要付多少钱**”。它与正式出账的区别只在时点，不在计价规则：

- **只查当前 UTC 自然月。** 账期固定为服务当前时刻所在的 UTC 自然月，
  不能指定历史月份；账期不必结束，也不需要先出账。结果中的 `Period`
  就是这个账期。
- **结果明确标为预估。** 返回的 `EstimatedBill` 上 `Estimated` 恒为
  `true`，并给出账期、当前适用的完整套餐条件（`Terms`）、累计用量
  （`TotalUsage`）、包含额度（`IncludedUnits`）、超额用量
  （`OverageUnits`）、月费（`MonthlyFee`）、超额费用（`OverageFee`）、
  税额（`Tax`）与预计应付金额（`EstimatedTotalDue`）；金额单位仍为分。
  没有任何用量时也能查询：累计与超额用量为零，完整月费与税额照常计算。
- **与正式出账同一套计价。** 月费按整月收取、额度按整月提供，月中开通
  也不按天折算；超额单位为 `max(0, TotalUsage - IncludedUnits)`，按
  **订阅保存的当月套餐条件快照**中的超额单价计费；税额以月费与超额费用
  之和为基数，按快照中的万分比税率四舍五入到分（不足半分舍去、恰好半分
  向上）。`UpdatePlan` 修改套餐定义不会改写已保存的订阅快照。
- **尚未生效的换套餐安排不会提前混入本月预估。** 安排只在生效月月初
  零点（与其他入口一样由查询时的状态结算自动承认）之后才成为当月条件；
  到达后**直接查询**就使用安排接受时锁定的条件，不依赖先上报用量，
  修改套餐定义同样不影响该锁定快照。
- **纯读操作，没有任何副作用。** 查询不保存正式账单、不关闭当月、不
  产生欠费，也不改变已有账单的金额与付款状态；查询之后同月合法的新用量
  仍按原规则接收（因欠费停用期间的新用量仍被拒绝），再次查询反映新增
  累计量；换套餐和取消安排仍按原时刻生效。正式出账继续只处理已经结束的
  月份，预估永远不会把当月变成“已出账”。
- **等待取消与欠费停用期间都可查询。** 已登记按月取消但尚未到终止时刻
  的订阅仍生效，当月按完整月费与额度预估；因欠费暂停接收新用量但订阅仍
  生效的账户也可查询，查询本身不会解除停用。
- **不可预估的情况各自返回已有错误，不输出预估。** 账户不存在返回
  `ErrAccountNotFound`；从未开通、订阅已经终止（含已到终止时刻）返回
  `ErrSubscriptionNotFound`；提前登记但开通时刻尚未到达返回
  `ErrSubscriptionNotActivated`。
- **金额溢出与正式出账同样处理。** 超额费用、税前合计或预计应付金额超出
  非负 `int64` 范围时返回 `ErrOverflow`，不返回部分金额；预估不落库，
  因此不会留下任何需要清理的中间结果。

由于预估只统计**已接收**的用量，账期结束后若没有新增用量，正式出账
（`CreateBill`）的各项金额与最后一次预估一致；期间又接收了用量时，正式
账单按出账时的累计计算，金额可能高于预估。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 注入可推进的
时钟，输出不依赖运行当天日期。账户 `acct-estimate` 2026-01-10 12:00 UTC
开通 plan-est（月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%），
月中开通也按整月计费。示例依次展示：无用量时预估仍收完整月费与税额；
接收 15 单位后再次预估，超额 5 单位、超额费 500 分、税 150 分、预计
应付 1650 分；一月中旬安排二月换套餐并把套餐定义改价，一月预估不混入
新安排也不被改价影响；推进到二月月初后直接预估即使用安排时锁定的
plan-b 条件（2000/20/200/600）。该示例以 Example 测试形式保存在
`meter/estimate_example_test.go`，`go test ./...` 会校验其输出：

```go
// 可推进的时钟：初始当前时刻为 2026-01-10 12:00 UTC（一月中旬）。
now := mustParseTime("2026-01-10T12:00:00Z")
s := meter.NewServiceWithClock(func() time.Time { return now })

if err := s.CreatePlan(meter.Plan{
    ID: "plan-est", MonthlyFee: 1000, IncludedUnits: 10,
    OveragePrice: 100, TaxRateBasisPoints: 1000,
}); err != nil {
    panic(err)
}
if err := s.CreatePlan(meter.Plan{
    ID: "plan-b", MonthlyFee: 2000, IncludedUnits: 20,
    OveragePrice: 200, TaxRateBasisPoints: 600,
}); err != nil {
    panic(err)
}
if err := s.CreateAccount("acct-estimate"); err != nil {
    panic(err)
}
// 月中开通：当月仍按完整月费与完整额度预估，不按天折算。
if err := s.Subscribe("acct-estimate", "plan-est",
    mustParseTime("2026-01-10T12:00:00Z")); err != nil {
    panic(err)
}

// 无任何用量也能预估：累计与超额为零，月费 1000 分完整计入，
// 税 1000×10%=100 分，预计应付 1100 分。
e, err := s.EstimateCurrentBill("acct-estimate")
if err != nil {
    panic(err)
}
fmt.Printf("no usage    estimated=%t period=%s plan=%s totalUsage=%d overageUnits=%d monthlyFee=%d overageFee=%d tax=%d totalDue=%d\n",
    e.Estimated, e.Period, e.Terms.PlanID, e.TotalUsage, e.OverageUnits,
    e.MonthlyFee, e.OverageFee, e.Tax, e.EstimatedTotalDue)

// 推进到一月中旬再上报并预估。
now = mustParseTime("2026-01-15T12:00:00Z")
// 接收 15 单位：超额 5 × 100 = 500 分；税 (1000+500) × 10% = 150 分。
if _, err := s.RecordEvent(meter.Event{
    AccountID: "acct-estimate", EventID: "e1",
    At: mustParseTime("2026-01-12T00:00:00Z"), Quantity: 15,
}); err != nil {
    panic(err)
}
e, err = s.EstimateCurrentBill("acct-estimate")
if err != nil {
    panic(err)
}
fmt.Printf("with usage  estimated=%t period=%s totalUsage=%d includedUnits=%d overageUnits=%d monthlyFee=%d overageFee=%d tax=%d totalDue=%d\n",
    e.Estimated, e.Period, e.TotalUsage, e.IncludedUnits, e.OverageUnits,
    e.MonthlyFee, e.OverageFee, e.Tax, e.EstimatedTotalDue)

// 预估不保存账单、不关闭当月。
if _, err := s.GetBill("acct-estimate", meter.MonthOf(now)); err != nil {
    fmt.Printf("bill saved errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))
}

// 一月中旬安排二月换到 plan-b，随后修改 plan-est 与 plan-b 的定义：
// 一月预估继续使用订阅快照，二月安排继续使用安排时锁定的快照。
if _, err := s.SchedulePlanChange("acct-estimate", "plan-b"); err != nil {
    panic(err)
}
if err := s.UpdatePlan(meter.Plan{
    ID: "plan-est", MonthlyFee: 9, IncludedUnits: 0,
    OveragePrice: 9, TaxRateBasisPoints: 0,
}); err != nil {
    panic(err)
}
if err := s.UpdatePlan(meter.Plan{
    ID: "plan-b", MonthlyFee: 5000, IncludedUnits: 5,
    OveragePrice: 900, TaxRateBasisPoints: 2500,
}); err != nil {
    panic(err)
}
e, err = s.EstimateCurrentBill("acct-estimate")
if err != nil {
    panic(err)
}
fmt.Printf("jan kept    plan=%s monthlyFee=%d overagePrice=%d taxRateBasisPoints=%d totalDue=%d\n",
    e.Terms.PlanID, e.MonthlyFee, e.Terms.OveragePrice,
    e.Terms.TaxRateBasisPoints, e.EstimatedTotalDue)

// 推进到 2026-02-01 00:00 UTC：安排已生效，直接预估即使用安排接受时
// 锁定的 plan-b 条件（2000/20/200/600），无须先上报用量；二月尚无
// 用量，月费 2000、税 2000×6%=120、预计应付 2120。
now = mustParseTime("2026-02-01T00:00:00Z")
e, err = s.EstimateCurrentBill("acct-estimate")
if err != nil {
    panic(err)
}
fmt.Printf("feb terms   estimated=%t period=%s plan=%s monthlyFee=%d includedUnits=%d overagePrice=%d taxRateBasisPoints=%d totalUsage=%d totalDue=%d\n",
    e.Estimated, e.Period, e.Terms.PlanID, e.MonthlyFee, e.IncludedUnits,
    e.Terms.OveragePrice, e.Terms.TaxRateBasisPoints,
    e.TotalUsage, e.EstimatedTotalDue)

// 不可预估的情况：不存在的账户、从未开通的账户、提前登记但开通时刻
// 未到的账户，分别返回各自的已有错误。
if err := s.CreateAccount("acct-no-sub"); err != nil {
    panic(err)
}
_, err = s.EstimateCurrentBill("ghost")
fmt.Printf("ghost       errAccountNotFound=%t\n", errors.Is(err, meter.ErrAccountNotFound))
_, err = s.EstimateCurrentBill("acct-no-sub")
fmt.Printf("no sub      errSubscriptionNotFound=%t\n", errors.Is(err, meter.ErrSubscriptionNotFound))
if err := s.CreateAccount("acct-future-sub"); err != nil {
    panic(err)
}
if err := s.Subscribe("acct-future-sub", "plan-est",
    mustParseTime("2026-02-10T00:00:00Z")); err != nil {
    panic(err)
}
_, err = s.EstimateCurrentBill("acct-future-sub")
fmt.Printf("future sub  errSubscriptionNotActivated=%t\n", errors.Is(err, meter.ErrSubscriptionNotActivated))
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
no usage    estimated=true period=2026-01 plan=plan-est totalUsage=0 overageUnits=0 monthlyFee=1000 overageFee=0 tax=100 totalDue=1100
with usage  estimated=true period=2026-01 totalUsage=15 includedUnits=10 overageUnits=5 monthlyFee=1000 overageFee=500 tax=150 totalDue=1650
bill saved  errBillNotFound=true
jan kept    plan=plan-est monthlyFee=1000 overagePrice=100 taxRateBasisPoints=1000 totalDue=1650
feb terms   estimated=true period=2026-02 plan=plan-b monthlyFee=2000 includedUnits=20 overagePrice=200 taxRateBasisPoints=600 totalUsage=0 totalDue=2120
ghost       errAccountNotFound=true
no sub      errSubscriptionNotFound=true
future sub  errSubscriptionNotActivated=true
```

## 付款登记

`RecordPayment` 为指定账期的账单分次登记付款，金额单位为分，须大于零且不超过
账单当时余额。付款标识只在所属账户内去重：不同账户可以复用同一标识，互不影响。

同一账户、同一账单上的一次重报分为两种情况：

- **原样重报**（账户、付款标识、账期、金额与首次完全一致）：调用仍然成功，但
  本次不再登记、不再增加已付金额，`Registered` 为 `false`。返回的付款内容以及
  `BillBalance`、`Settled` 两个字段是**该笔付款首次成功登记完成那一刻**账单的
  历史结果，原样保存、原样返回，不会在这次调用时重新计算，也不随后续付款或账单
  当前状态变化。因此在后续又付过款之后，重报旧付款看到的 `BillBalance` 可能比
  `GetBill` 查到的当前余额更大（例如旧付款登记时账单还欠 600 分，后来已全部
  付清，重报仍返回 600 与 `Settled=false`）。`Registered=false` 的含义是
  “此前已登记过同一笔、本次幂等返回”，**不表示付款失败**。
- **同一标识但账期或金额不一致**：返回 `ErrPaymentConflict`，即使原账单已经
  结清、或改指向的账期还没有账单，也按冲突处理而不是当作新付款；失败不消耗付款
  标识，原账单与原付款记录保持不变。

要展示账单的最新欠款，应读取 `GetBill` 返回的 `Paid` / `Balance` / `Settled`，
或 `Status` 中 `Bills` 摘要的同名字段；它们始终反映查询时点的当前状态，不受付款
重报影响。可以这样区分两类结果的时点：`PaymentResult` 对应“这笔付款首次登记完成
时”，`GetBill` / `Status` 对应“本次查询时”。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 把当前时刻固定在
2026-02-01 00:00 UTC，使 2026-01 成为一个已经结束的 UTC 自然月账期，无需等到
某个真实日期。账户 `acct-pay` 的该月账单应付 1000 分（月费 1000 分，无超额、
无税），先用 `p1` 登记 400 分，再用 `p2` 登记剩余 600 分，结清后原样重报 `p1`，
然后查询当前账单状态并演示两种冲突。该示例以 Example 测试形式保存在
`meter/payment_replay_example_test.go`，`go test ./...` 会校验其输出：

```go
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
    ID: "plan1000", MonthlyFee: 1000, IncludedUnits: 0,
    OveragePrice: 0, TaxRateBasisPoints: 0,
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

// 第一笔：p1 登记 400 分，Registered=true 表示本次实际登记。
p1, err := s.RecordPayment(accountID, "p1", period, 400)
if err != nil {
    panic(err)
}
fmt.Printf("p1 first    registered=%t paymentID=%s period=%s amount=%d billBalance=%d settled=%t\n",
    p1.Registered, p1.Payment.PaymentID, p1.Payment.Period, p1.Payment.Amount,
    p1.BillBalance, p1.Settled)

// 第二笔：另一标识 p2 登记剩余 600 分，账单结清。
p2, err := s.RecordPayment(accountID, "p2", period, 600)
if err != nil {
    panic(err)
}
fmt.Printf("p2 first    registered=%t paymentID=%s period=%s amount=%d billBalance=%d settled=%t\n",
    p2.Registered, p2.Payment.PaymentID, p2.Payment.Period, p2.Payment.Amount,
    p2.BillBalance, p2.Settled)

// 结清后原样重报 p1：仍成功，但 Registered=false、不再次登记；
// 付款内容仍是 p1 对 2026-01 的 400 分，BillBalance=600、Settled=false
// 是 p1 首次登记完成时的历史结果，不是此刻的账单余额。
p1Replay, err := s.RecordPayment(accountID, "p1", period, 400)
if err != nil {
    panic(err)
}
fmt.Printf("p1 replay   registered=%t paymentID=%s period=%s amount=%d billBalance=%d settled=%t\n",
    p1Replay.Registered, p1Replay.Payment.PaymentID, p1Replay.Payment.Period,
    p1Replay.Payment.Amount, p1Replay.BillBalance, p1Replay.Settled)

// 当前欠款查账单：累计已付 1000 分、余额为零、已经付清。
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

// 沿用 p1 却把金额改成 401 分：ErrPaymentConflict。
_, err = s.RecordPayment(accountID, "p1", period, 401)
fmt.Printf("p1 amount=401 errPaymentConflict=%t\n", errors.Is(err, meter.ErrPaymentConflict))

// 沿用 p1 却指向尚未出账的另一账期（2026-02）：同样 ErrPaymentConflict。
future := meter.MonthOf(mustParseTime("2026-02-15T00:00:00Z"))
_, err = s.RecordPayment(accountID, "p1", future, 400)
fmt.Printf("p1 period=2026-02 errPaymentConflict=%t\n", errors.Is(err, meter.ErrPaymentConflict))

// 两次冲突都不改动数据：当前账单仍是已付 1000、余额 0、已付清。
after, err := s.GetBill(accountID, period)
if err != nil {
    panic(err)
}
fmt.Printf("get bill    totalDue=%d paid=%d balance=%d settled=%t\n",
    after.TotalDue, after.Paid, after.Balance, after.Settled)
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
bill        totalDue=1000 paid=0 balance=1000 settled=false
p1 first    registered=true paymentID=p1 period=2026-01 amount=400 billBalance=600 settled=false
p2 first    registered=true paymentID=p2 period=2026-01 amount=600 billBalance=0 settled=true
p1 replay   registered=false paymentID=p1 period=2026-01 amount=400 billBalance=600 settled=false
get bill    totalDue=1000 paid=1000 balance=0 settled=true
status bill period=2026-01 totalDue=1000 paid=1000 balance=0 settled=true
p1 amount=401 errPaymentConflict=true
p1 period=2026-02 errPaymentConflict=true
get bill    totalDue=1000 paid=1000 balance=0 settled=true
```

## 用量补报

讨论补报前要区分三个时刻：**事件发生时刻**（`Event.At`）、**提交时刻**
（调用 `RecordEvent` 时的当前时刻）和**出账时刻**（`CreateBill` 成功生成
该账期账单的时刻）。用量一律按**发生时刻**归入 UTC 自然月账期，与何时
提交无关；账期已经结束不等于不能补报。

符合原有参数要求（标识非空、数量非负、时刻非零且不晚于当前时刻）与
累计范围要求（累计不溢出 int64）的新事件，只要同时满足以下条件仍可接收：

- 发生时刻属于某段**实际订阅期间**（开通计入、终止不计入）；
- 发生时刻所属月份**尚未成功出账**；
- 账户**未因欠费停用**。

订阅已经按月取消也不妨碍补报终止前尚未出账的用量：取消安排只决定终止
时刻，终止前发生的事件仍属于订阅期间。反过来，发生在终止时刻或之后、
且没有新订阅覆盖的用量仍被拒绝（`ErrEventBeforeSubscription`）。

账单成功生成后，该月累计用量和账单中的用量就固定下来：晚到的新事件
不会重新计算账单，`RecordEvent` 对其返回 `ErrMonthBilled`，该月
`MonthlyUsage` 与 `GetBill` 的结果保持出账时的数值。拒收不消耗事件
标识，但同一标识再次提交仍会因账期已出账而失败。

同一账户内事件标识去重与上述校验的先后关系决定了重报的语义：**原样
重报**（账户、事件标识、发生时刻、数量与首次完全一致）即使发生在出账
之后也仍然成功，但 `Accepted` 为 `false`、`Period` 仍是首次归入的账期，
本次不再累计。`Accepted=false` 表示“此前已经接收过，本次幂等返回”，
**不能把它当成失败**；而因出账被拒绝的新事件再次提交仍然失败，也不能
被当作成功事件的重报。标识相同但时刻或数量不同的提交返回
`ErrEventConflict`，与是否出账无关。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 把当前时刻
固定在 2026-02-01 00:00 UTC，账户 `acct-late-usage` 自 2026-01-01
00:00 UTC 起持有持续有效的订阅（套餐月费 1000 分、无超额、无税），
一月已有累计用量 8，一月尚未出账且没有其他欠款。示例依次展示：补报
一条发生在 2026-01-31 23:59:59 UTC、数量 5 的事件，本次已接收、归属
2026-01，一月累计变为 13；随后生成一月账单，账单总用量也是 13；再
提交另一标识的一月事件得到 `ErrMonthBilled`，用量和账单总用量仍为
13；最后原样重报刚才成功补报的事件，调用成功、`Accepted` 为 false、
`Period` 仍是 2026-01，累计不再增加。该示例以 Example 测试形式保存在
`meter/late_usage_example_test.go`，`go test ./...` 会校验其输出：

```go
const (
    accountID = "acct-late-usage"
    jan1st    = "2026-01-01T00:00:00Z"
    now       = "2026-02-01T00:00:00Z"
)
jan := meter.MonthOf(mustParseTime(jan1st))

// 把“当前时刻”固定在 2026-02-01 00:00 UTC：2026-01 账期恰好结束，
// 此时提交发生在一月的事件即“补报”；当前时刻之后的任何真实日期
// 也能得到同样结果。
s := meter.NewServiceWithClock(func() time.Time {
    return mustParseTime(now)
})

if err := s.CreateAccount(accountID); err != nil {
    panic(err)
}
// 套餐月费恰好 1000 分、无超额、无税：账单 TotalDue 就是 1000 分。
if err := s.CreatePlan(meter.Plan{
    ID: "plan1000", MonthlyFee: 1000, IncludedUnits: 0,
    OveragePrice: 0, TaxRateBasisPoints: 0,
}); err != nil {
    panic(err)
}
// 订阅自 2026-01-01 00:00 UTC 起持续有效，不取消、不换套餐。
if err := s.Subscribe(accountID, "plan1000", mustParseTime(jan1st)); err != nil {
    panic(err)
}

// 一月已有用量 8：一条发生在 1 月 15 日、数量 8 的事件。
ev, err := s.RecordEvent(meter.Event{
    AccountID: accountID,
    EventID:   "e0",
    At:        mustParseTime("2026-01-15T00:00:00Z"),
    Quantity:  8,
})
if err != nil {
    panic(err)
}
fmt.Printf("event e0    accepted=%t period=%s\n", ev.Accepted, ev.Period)

usage, err := s.MonthlyUsage(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

// 补报：账期已经结束但尚未出账，发生时刻 2026-01-31 23:59:59 UTC
// 仍属于实际订阅期间，账户也未停用，事件 e1 被接收并归入 2026-01。
ev, err = s.RecordEvent(meter.Event{
    AccountID: accountID,
    EventID:   "e1",
    At:        mustParseTime("2026-01-31T23:59:59Z"),
    Quantity:  5,
})
if err != nil {
    panic(err)
}
fmt.Printf("event e1    accepted=%t period=%s\n", ev.Accepted, ev.Period)

// 一月累计由 8 变为 13。
usage, err = s.MonthlyUsage(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

// 为一月出账：账单总用量取此时的一月累计，也是 13。
bill, err := s.CreateBill(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("bill        period=%s totalUsage=%d totalDue=%d\n",
    bill.Period, bill.TotalUsage, bill.TotalDue)

// 出账后再提交另一标识 e2 的一月新事件：ErrMonthBilled。
// 账单生成后该月累计与账单用量已经固定，晚到的新事件不会
// 重新计算账单。拒收不消耗事件标识 e2。
_, err = s.RecordEvent(meter.Event{
    AccountID: accountID,
    EventID:   "e2",
    At:        mustParseTime("2026-01-20T00:00:00Z"),
    Quantity:  1,
})
fmt.Printf("event e2    errMonthBilled=%t\n", errors.Is(err, meter.ErrMonthBilled))

// 被拒收的事件没有累计：一月用量仍是 13。
usage, err = s.MonthlyUsage(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)

// 账单也不变：总用量与应付金额保持出账时的结果。
current, err := s.GetBill(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("get bill    period=%s totalUsage=%d totalDue=%d\n",
    current.Period, current.TotalUsage, current.TotalDue)

// 原样重报刚才成功补报的 e1（账户、标识、时刻、数量完全一致）：
// 即使在出账之后，调用仍然成功，但 Accepted=false 表示此前已
// 接收过，本次没有再次累计；Period 仍是 2026-01。
ev, err = s.RecordEvent(meter.Event{
    AccountID: accountID,
    EventID:   "e1",
    At:        mustParseTime("2026-01-31T23:59:59Z"),
    Quantity:  5,
})
if err != nil {
    panic(err)
}
fmt.Printf("event e1    accepted=%t period=%s\n", ev.Accepted, ev.Period)

// 重报没有再次累计：一月用量仍是 13。
usage, err = s.MonthlyUsage(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("usage       period=%s total=%d\n", usage.Period, usage.Total)
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
event e0    accepted=true period=2026-01
usage       period=2026-01 total=8
event e1    accepted=true period=2026-01
usage       period=2026-01 total=13
bill        period=2026-01 totalUsage=13 totalDue=1000
event e2    errMonthBilled=true
usage       period=2026-01 total=13
get bill    period=2026-01 totalUsage=13 totalDue=1000
event e1    accepted=false period=2026-01
usage       period=2026-01 total=13
```

## 延迟出账

账单不一定要在账期刚结束时生成：`CreateBill` 可以为任何已经结束且被
某段订阅覆盖的账期出账，哪怕账期结束已过去一段时间。需要注意两点：

- **付款截止时刻不顺延。** `DueAt` 始终是账期结束后七天（UTC），从账期
  结束时刻算起，与何时调用 `CreateBill` 无关。例如 2026-01 账期于
  2026-02-01 00:00 UTC 结束，无论 2 月 1 日还是 2 月 10 日才出账，
  截止时刻都是 2026-02-08 00:00 UTC。截止时刻本身已属于到期：只要
  账单仍有余额，到达该瞬间账户即被停用；零应付且出账即结清的账单不会
  造成欠费停用。
- **账户能否继续接收用量取决于停用状态。** 出账时若截止时刻已过且账单
  未结清，账户立即进入欠费停用，`RecordEvent` 对新事件返回
  `ErrSuspended`；停用只拒绝新用量，订阅本身仍然有效，当前套餐条件
  保持原样，历史用量与账单继续可查。被拒收的事件不消耗事件标识，结清
  全部到期欠费后账户恢复，同一标识可再次提交并按首次接收。登记付款只
  减少账单余额，不改变账单原有的应付金额与截止时刻。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 把当前时刻
固定在 2026-02-10 00:00 UTC，再为一月出账。账户 `acct-late` 持有一份
持续有效的订阅（2026-01-01 00:00 UTC 开通，套餐月费 1000 分，超额单价
与税率均为零，账户没有其他账单）。示例依次展示：出账前账户未停用；
延迟出账后截止时刻仍为 2026-02-08 00:00 UTC、账单应付与余额都是
1000 分且尚未结清，账户因此欠费停用而订阅仍有效；属于尚未出账的二月
的新用量被拒且二月累计仍为零；登记 1000 分本地付款后一月账单结清、
账户解除停用；再次提交刚才被拒绝的事件作为首次接收，二月累计变为 3。
该示例以 Example 测试形式保存在 `meter/late_billing_example_test.go`，
`go test ./...` 会校验其输出：

```go
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
    ID: "plan1000", MonthlyFee: 1000, IncludedUnits: 0,
    OveragePrice: 0, TaxRateBasisPoints: 0,
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
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
before bill subscribed=true suspended=false planID=plan1000 monthlyFee=1000
bill        period=2026-01 totalDue=1000 balance=1000 settled=false dueAt=2026-02-08T00:00:00Z
after bill  subscribed=true suspended=true planID=plan1000 monthlyFee=1000
event e1    errSuspended=true
usage       period=2026-02 total=0
payment     registered=true paymentID=pay-jan amount=1000 billBalance=0 settled=true
get bill    totalDue=1000 paid=1000 balance=0 settled=true dueAt=2026-02-08T00:00:00Z
after pay   subscribed=true suspended=false planID=plan1000 monthlyFee=1000
event e1    accepted=true period=2026-02
usage       period=2026-02 total=3
```

## 月度历史查询：区分“累计为零”与“没有这个月”

`Status` 返回的两份历史列表（`MonthlyUsage` 与 `Bills`）和 `MonthlyUsage`
单月查询回答的是不同问题。调用时需要区分“某月累计为零”和“历史列表没有
这个月”：

- **`Status` 的 `MonthlyUsage` 只包含已经接收过事件或已经生成账单的
  月份**，每月一条，按年月先后排列。接收数量为零的事件后，该月仍会出现
  且累计为零——“接收过零数量事件”与“没有任何记录”在列表里的区别就是
  这一条零值记录是否存在；没有任何事件但已经出账的月份也会出现，累计
  同样为零。既没有事件也没有账单的月份不会因为订阅一直有效就自动补进
  列表。
- **`Status` 的 `Bills` 只包含实际生成的账单**，同样按账期先后排列。
  有用量记录不代表已经出账：某月出现在 `MonthlyUsage` 里，不表示
  `Bills` 里一定有它。
- **`MonthlyUsage` 单月查询对没有记录的合法月份仍返回该月份和零累计**，
  但不会因此新增历史条目，也不会生成账单。只看这个零值无法判断该月是否
  接收过零数量事件、或是否已经出账——要区分需对照 `Status` 的列表或
  `GetBill` 的结果。
- 一个没有任何事件和账单的已有账户，两份历史列表都为空。
- 查询不存在的账户返回 `ErrAccountNotFound`（`Status` 与 `MonthlyUsage`
  皆然）。**不能把查询失败展示成空历史**：空列表只属于“账户存在但没有
  任何记录”的情况，账户不存在是错误，必须先检查 `err`。

下面的示例只用公开入口即可运行：通过 `NewServiceWithClock` 把当前时刻
固定在 2026-04-01 00:00 UTC，输出不依赖运行当天日期。账户 `acct-hist`
的订阅自 2026-01-01 00:00 UTC 起持续有效，套餐月费 100 分、超额单价与
税率均为零。一月只接收一条数量为零的事件；二月没有任何事件和账单；三月
没有事件但已生成账单（应付与余额都是 100 分）。查询状态时用量列表按一月、
三月排列且两项累计均为零，账单列表只有三月；单独查询二月得到零累计后
再次查询状态，二月仍不出现。该示例以 Example 测试形式保存在
`meter/status_monthly_history_example_test.go`，`go test ./...` 会校验
其输出。下面是自包含程序，保存为仓库根目录下的 `main.go` 后
`go run main.go` 即可得到文末输出：

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

func mustParseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

func main() {
	const accountID = "acct-hist"
	feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))
	mar := meter.MonthOf(mustParseTime("2026-03-01T00:00:00Z"))

	// 把当前时刻固定在 2026-04-01 00:00 UTC：一月、二月、三月账期都已
	// 结束，复制后在任何真实日期运行结果都相同。
	s := meter.NewServiceWithClock(func() time.Time {
		return mustParseTime("2026-04-01T00:00:00Z")
	})

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐月费 100 分，超额单价与税率均为零：任何月份出账应付都是 100 分。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan100", MonthlyFee: 100, IncludedUnits: 0,
		OveragePrice: 0, TaxRateBasisPoints: 0,
	}); err != nil {
		panic(err)
	}
	// 订阅自 2026-01-01 00:00 UTC 起持续有效，不取消、不换套餐。
	if err := s.Subscribe(accountID, "plan100", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 一月只接收一条数量为零的事件：合法事件，接收后一月留下一条累计
	// 为零的用量记录。
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-jan-zero",
		At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 0,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-jan-zero accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 三月没有任何事件，但账期已结束且被订阅覆盖，可以出账：月费 100 分
	// 完整计入，无超额、无税，应付与余额都是 100 分。已出账的月份即使
	// 没有事件也会以零用量出现在用量列表中。
	bill, err := s.CreateBill(accountID, mar)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill mar     period=%s totalUsage=%d totalDue=%d balance=%d settled=%t dueAt=%s\n",
		bill.Period, bill.TotalUsage, bill.TotalDue, bill.Balance, bill.Settled,
		bill.DueAt.Format(time.RFC3339))

	// 查询状态：用量列表只有一月（接收过零数量事件）与三月（已出账），
	// 两项累计都是零；二月既无事件也无账单，不因订阅持续有效而补进列表。
	// 账单列表只有三月一张实际生成的账单：有用量记录不代表已经出账。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	for i, u := range st.MonthlyUsage {
		fmt.Printf("usage[%d]     period=%s total=%d\n", i, u.Period, u.Total)
	}
	for i, b := range st.Bills {
		fmt.Printf("bills[%d]     period=%s totalDue=%d paid=%d balance=%d settled=%t\n",
			i, b.Period, b.TotalDue, b.Paid, b.Balance, b.Settled)
	}

	// 单独查询没有任何记录的二月：仍返回该月份与零累计，但这次查询不会
	// 把二月写进历史，也不会生成账单。只看这个零值无法判断二月是否接收
	// 过零数量事件、或是否已经出账。
	usage, err := s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("query feb    period=%s total=%d\n", usage.Period, usage.Total)
	_, err = s.GetBill(accountID, feb)
	fmt.Printf("get bill feb errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))

	// 再次查询状态：二月仍不在用量列表中，账单列表仍只有三月。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	febInUsage := false
	for _, u := range st.MonthlyUsage {
		if u.Period == feb {
			febInUsage = true
		}
	}
	fmt.Printf("status again usageMonths=%d billMonths=%d febInUsage=%t\n",
		len(st.MonthlyUsage), len(st.Bills), febInUsage)

	// 一个没有任何事件和账单的已有账户：两份历史列表都为空。
	if err := s.CreateAccount("acct-empty"); err != nil {
		panic(err)
	}
	empty, err := s.Status("acct-empty")
	if err != nil {
		panic(err)
	}
	fmt.Printf("empty        usageMonths=%d billMonths=%d\n",
		len(empty.MonthlyUsage), len(empty.Bills))

	// 查询不存在的账户：返回 ErrAccountNotFound。查询失败不能展示成空
	// 历史——空列表只属于“账户存在但没有任何记录”的情况。
	_, err = s.Status("ghost")
	fmt.Printf("ghost status errAccountNotFound=%t\n", errors.Is(err, meter.ErrAccountNotFound))
	_, err = s.MonthlyUsage("ghost", feb)
	fmt.Printf("ghost usage  errAccountNotFound=%t\n", errors.Is(err, meter.ErrAccountNotFound))
}
```

输出：

```text
event e-jan-zero accepted=true period=2026-01
bill mar     period=2026-03 totalUsage=0 totalDue=100 balance=100 settled=false dueAt=2026-04-08T00:00:00Z
usage[0]     period=2026-01 total=0
usage[1]     period=2026-03 total=0
bills[0]     period=2026-03 totalDue=100 paid=0 balance=100 settled=false
query feb    period=2026-02 total=0
get bill feb errBillNotFound=true
status again usageMonths=2 billMonths=1 febInUsage=false
empty        usageMonths=0 billMonths=0
ghost status errAccountNotFound=true
ghost usage  errAccountNotFound=true
```

对照输出可以确认：一月的零累计来自接收过的零数量事件，三月的零累计来自
已生成的账单，两者都出现在用量列表中；二月同样能查出零累计，但它既没有
事件也没有账单，所以始终不在列表里——`query feb` 的零与列表中两条零
含义不同，单凭单月查询的零值无法区分。`empty` 行说明已有账户可以两份
列表都为空，而 `ghost` 两行是查询失败，必须先判错误再使用结果。
