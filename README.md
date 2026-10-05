# 订阅、用量与账单

在本机运行的订阅、用量与账单服务，以 Go 包 `meter` 提供，数据保存在内存中。

## 使用

```bash
go test ./...
```

## 能力

- `NewService` / `NewServiceWithClock`：创建并发安全的服务（后者可注入时钟）。
- `CreateAccount`：创建业务账户，标识唯一。
- `CreatePlan` / `UpdatePlan`：维护套餐（月费、包含用量、超额单价、万分比税率）。
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

`UpdatePlan` 与 `SchedulePlanChange` 是两件不同的事，调用方需要分清：

- **`UpdatePlan` 修改的是套餐的“当前定义”**。修改只对**之后新取得条件**的
  场合生效：之后开通订阅、或为已开通账户安排换套餐被接受时，按修改后的定义
  取得条件。它**不会**改写账户正在使用的条件（那是开通时保存的快照），也
  **不会**改写此前已接受的换套餐安排（安排接受时已锁定自己的快照）。因此
  “修改套餐只影响之后开通的订阅”这一说法过窄：已开通账户后来安排换用这个
  套餐时，取得的是**安排被接受时**的定义；安排一旦接受，定义再怎么改都与
  它无关。
- **`SchedulePlanChange` 是“给某个账户安排换套餐”**。安排被接受即锁定目标
  套餐的完整条件（月费、包含额度、超额单价、税率）与生效账期（接受时的下一
  个 UTC 自然月），到该账期月初零点自动生效，无需上报用量或出账。每账户至
  多一条待生效安排。

重复安排同一目标套餐的结果要分两种情况：**安排尚未生效时**，再次安排同一
目标成功返回原安排，`Created` 为 `false`，不会重新取价——即使这期间用
`UpdatePlan` 改过定义，返回的仍是首次接受时锁定的条件；**安排生效后**，目标
套餐已是当前套餐，再安排它按现有规则返回 `ErrPlanChangeSamePlan`。

下面的示例只用公开入口即可运行，围绕账户 `acct-switch` 从套餐 A 换到 B
展开，金额单位为分，所有时刻与账期均为 UTC。账户 2026-01-01 00:00 UTC
起使用 A（月费 1000、包含 100、超额单价 10、税率 1000‱）；1 月 15 日
安排 2 月换到 B，安排时 B 的定义为月费 2000、包含 200、超额单价 8、税率
600‱。随后把 B 的定义改成月费 5000、包含 50、超额单价 20、税率 1000‱，
再次安排 B 返回原安排、`Created=false`；2 月月初零点后直接查询即显示锁定
的 B 条件、待生效安排消失，再安排 B 得到同套餐错误。一月用量 150 按 A
出账（完整月费与额度，超额 50×10=500、税 150、应付 1650）；账单到期未付
导致欠费停用，二月用量先被拒收，登记 1650 分付款后恢复并重新提交；三月
一日为二月出账，用的是锁定的 B 条件（超额 100×8=800、税 168、应付
2968），修改后的 B 定义没有混入；二月出账后一月账单金额不变。该示例以
Example 测试形式保存在 `meter/plan_change_example_test.go`，
`go test ./...` 会校验其输出：

```go
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

// 正常登记一笔 1650 分的付款结清一月账单，解除欠费停用；
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

// 二月出账后再查一月账单：金额仍是出账时的 1650，且已付清。
gotJan, err := s.GetBill(accountID, jan)
if err != nil {
    panic(err)
}
fmt.Printf("get jan bill totalDue=%d paid=%d balance=%d settled=%t\n",
    gotJan.TotalDue, gotJan.Paid, gotJan.Balance, gotJan.Settled)
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
change 1    created=true target=plan-b monthlyFee=2000 included=200 overagePrice=8 taxBasisPoints=600 effective=2026-02
status jan  current=plan-a monthlyFee=1000 pendingTarget=plan-b pendingFee=2000 pendingIncluded=200 pendingOverage=8 pendingTaxBp=600 pendingEffective=2026-02
plan B      monthlyFee=5000 included=50 overagePrice=20 taxBasisPoints=1000
change 2    created=false target=plan-b monthlyFee=2000 included=200 overagePrice=8 taxBasisPoints=600 effective=2026-02
status jan  current=plan-a monthlyFee=1000 pendingTarget=plan-b pendingFee=2000 pendingIncluded=200 pendingOverage=8 pendingTaxBp=600 pendingEffective=2026-02
event jan   accepted=true period=2026-01
usage       period=2026-01 total=150
status feb  subscribed=true suspended=false current=plan-b monthlyFee=2000 included=200 overagePrice=8 taxBasisPoints=600 pending=false
schedule b  errPlanChangeSamePlan=true
bill jan    plan=plan-a monthlyFee=1000 usage=150 included=100 overageUnits=50 overagePrice=10 overageFee=500 taxBp=1000 tax=150 totalDue=1650 paid=0 balance=1650 settled=false dueAt=2026-02-08T00:00:00Z
status due  suspended=true
event feb   errSuspended=true
payment     registered=true paymentID=pay-jan period=2026-01 amount=1650 billBalance=0 settled=true
status paid suspended=false
event feb   accepted=true period=2026-02
usage       period=2026-02 total=300
bill feb    plan=plan-b monthlyFee=2000 usage=300 included=200 overageUnits=100 overagePrice=8 overageFee=800 taxBp=600 tax=168 totalDue=2968 paid=0 balance=2968 settled=false dueAt=2026-03-08T00:00:00Z
get jan bill totalDue=1650 paid=1650 balance=0 settled=true
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
