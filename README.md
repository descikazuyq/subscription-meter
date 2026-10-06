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
- `MonthlyUsage` / `Status`：查询各月累计用量、当前生效套餐条件（订阅以实际开通时刻为界，等待开通期间为零值）、待生效安排、已安排的终止时刻、账单余额与欠费停用状态（等待开通不清旧欠费，`Subscribed` 与停用分别表示订阅是否生效与是否存在到期未结清账单）。

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
