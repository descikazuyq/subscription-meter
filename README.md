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
- `RecordPayment`：分次登记付款，账户内付款标识去重。
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

## 付款登记：重报返回的是历史结果，不是当前余额

`RecordPayment` 支持分次付款，付款标识只在**所属账户内**去重。需要特别注意：
**原样重报一笔旧付款时，返回的 `BillBalance` / `Settled` 是该笔付款首次成功
登记完成时的历史结果，不会在本次调用时重新计算**，因此不能把它当成账单当前
余额。在后续又付过款之后，旧付款重报里的余额可能比账单查询得到的当前余额更大
（例如账单已结清，重报早先的部分付款仍返回当时的未清余额）。`Registered=false`
只表示命中了此前已登记的同一笔付款（请求仍然成功，且不会再次扣款），并不表示
付款失败。要展示最新欠款，应读取 `GetBill` 或 `Status`——其中的余额始终反映
当前状态。

下面的完整示例只使用公开入口：用 `NewServiceWithClock` 注入固定时钟，把当前
时刻固定在 2026-02-01 00:00 UTC，于是账期 2026-01（UTC 自然月）已经结束，
任何一天运行都能得到相同结果，无需等待真实日期。金额单位均为分。

```go
const accountID = "acct-demo"
period := meter.Month{Year: 2026, Month: time.January} // 已结束的 UTC 自然月账期

now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
s := meter.NewServiceWithClock(func() time.Time { return now })

if err := s.CreateAccount(accountID); err != nil {
    panic(err)
}
// 月费 1000 分、额度与超额单价为 0、税率为 0：无用量时应付恰好 1000 分。
if err := s.CreatePlan(meter.Plan{
    ID: "plan-1000", MonthlyFee: 1000, IncludedUnits: 0,
    OveragePrice: 0, TaxRateBasisPoints: 0,
}); err != nil {
    panic(err)
}
if err := s.Subscribe(accountID, "plan-1000",
    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
    panic(err)
}
bill, err := s.CreateBill(accountID, period) // 应付 1000 分
if err != nil {
    panic(err)
}

// p1 先登记 400 分：本次实际登记，登记后账单余额 600、未付清。
first, err := s.RecordPayment(accountID, "p1", period, 400)
if err != nil {
    panic(err)
}
// first.Registered == true, first.BillBalance == 600, first.Settled == false

// p2 登记剩余 600 分：账单结清，当前余额 0。
if _, err := s.RecordPayment(accountID, "p2", period, 600); err != nil {
    panic(err)
}

// 结清后原样重报 p1（账户、标识、账期、金额完全一致）：成功，但不再登记。
replay, err := s.RecordPayment(accountID, "p1", period, 400)
if err != nil {
    panic(err)
}
// replay.Registered == false（不是失败，而是命中此前已登记的付款）
// replay.Payment 仍是 p1 对 2026-01 登记的 400 分
// replay.BillBalance == 600、replay.Settled == false —— 这是 p1 首次登记
// 完成那一刻的历史结果，不是此刻重新计算的欠款（此刻当前余额其实是 0）。

// 当前账单状态要读 GetBill / Status：累计已付 1000、余额 0、已付清。
cur, err := s.GetBill(accountID, period)
if err != nil {
    panic(err)
}
// cur.Paid == 1000, cur.Balance == 0, cur.Settled == true
// 旧付款重报不会再次增加已付金额，也不会使账单重新变成未付清。
_ = bill
_ = first
_ = replay
_ = cur
```

对照输出可直接区分两个时点（可运行版本见
`meter/example_payment_replay_test.go`，`go test ./...` 会校验其输出）：

```text
p1 首次登记 400: registered=true billBalance=600 settled=false      # 付款确认：登记完成时
p2 登记 600: registered=true billBalance=0 settled=true
原样重报 p1: registered=false payment={id:p1 period:2026-01 amount:400} billBalance=600 settled=false  # 历史时点
GetBill 当前状态: totalDue=1000 paid=1000 balance=0 settled=true    # 账单查询：当前时点
Status 当前状态: paid=1000 balance=0 settled=true
```

原样重报要求**同一账户内**付款标识、账期、金额三者一致；任一不同都返回
`ErrPaymentConflict`，原账单与原付款记录保持不变：

```go
// 沿用 p1 却把金额改成 401：冲突。
_, err = s.RecordPayment(accountID, "p1", period, 401)
// errors.Is(err, meter.ErrPaymentConflict) == true

// 把 p1 改指向尚未出账的另一账期（哪怕那里还没有账单）：仍是冲突，
// 不会被当作新付款，也不会返回 ErrBillNotFound。
_, err = s.RecordPayment(accountID, "p1",
    meter.Month{Year: 2026, Month: time.March}, 400)
// errors.Is(err, meter.ErrPaymentConflict) == true
```

付款标识只在所属账户内去重：另一个账户复用 `p1` 会作为它自己的首次付款独立
登记（`Registered=true`），两个账户的账单互不影响。
