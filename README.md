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
- `RecordEvent`：上报用量，按发生时刻归入 UTC 自然月账期，事件时刻须落在某段实际订阅期间（开通计入、终止不计入）；账户内事件标识去重。
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

## 延迟出账与欠费停用

账单的付款截止时刻在出账时即按账期固定：`DueAt` 等于**账期结束时刻**（次月
1 日 00:00 UTC）后七天，只取决于账期本身，与实际调用 `CreateBill` 的时刻无关。
因此可以在账期结束后任意时间补出账单，晚出账不会把截止时刻顺延，也不会重新
计算七天期限。例如 2026-01 账期于 2026-02-01 00:00 UTC 结束，无论 2 月 1 日
就出账还是拖到 2 月 10 日才出账，截止时刻始终是 2026-02-08 00:00 UTC。

停用按“当前时刻 vs 截止时刻”即时判定：**截止时刻本身已经属于到期**（当前时刻
不早于 `DueAt` 即到期，同一瞬间也算），只要账单仍有余额，到达该瞬间就停用，
无需先出账或发生其他操作，直接 `Status` / `GetBill` 查询即可看到；反过来，
应付为零的账单出账时即 `Settled=true`、余额为零，永远不会造成欠费停用。账单
尚未生成时不存在到期欠款，账户也不会因该账期被停用。

停用只影响“能否继续接收用量”，不退订：`Subscribed` 仍为 `true`，订阅保存的
当前套餐条件快照保持原样；停用期间新上报的事件返回 `ErrSuspended`（此前已接收
事件的完全相同重报仍按幂等成功）。被拒收的事件既不累计用量，也**不占用事件
标识**；之后用同一标识、同一内容重新提交，按首次接收处理。登记付款只改变账单
的 `Paid` / `Balance` / `Settled` 与账户停用状态，**不改变出账时固定的应付金额
和截止时刻**；到期账单全部结清后停用立即解除，原订阅继续有效。尚未到期的账单
即使有余额也不导致停用。

下面的示例只用公开入口即可运行：账户 `acct-late` 于 2026-01-01 00:00 UTC
开通一份持续有效的订阅，套餐月费 1000 分、超额单价与税率均为零，账户没有其他
账单。通过 `NewServiceWithClock` 把当前时刻固定在 2026-02-10 00:00 UTC（一月
账期已结束 9 天，2 月 8 日的截止时刻也已过去），再为一月出账，随后上报一条
发生在当前时刻、数量为 3、归属尚未出账二月的事件、付款结清并重提该事件。该
示例以 Example 测试形式保存在
`meter/delayed_billing_example_test.go`，`go test ./...` 会校验其输出：

```go
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
    ID: "plan1000", MonthlyFee: 1000, IncludedUnits: 0,
    OveragePrice: 0, TaxRateBasisPoints: 0,
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
```

输出（`mustParseTime` 用 `time.Parse(time.RFC3339, value)` 解析上述常量即可）：

```text
before bill  subscribed=true suspended=false plan=plan1000 monthlyFee=1000 bills=0
jan bill     totalDue=1000 paid=0 balance=1000 settled=false dueAt=2026-02-08T00:00:00Z
after bill   subscribed=true suspended=true plan=plan1000 monthlyFee=1000
event reject errSuspended=true
feb reject   total=0
payment      registered=true billBalance=0 settled=true
jan bill     totalDue=1000 paid=1000 balance=0 settled=true dueAt=2026-02-08T00:00:00Z
after pay    subscribed=true suspended=false plan=plan1000 monthlyFee=1000
event retry  accepted=true period=2026-02
feb retry    total=3
```
