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
- `Subscribe`：开通订阅并保存当时的套餐条件快照；每账户一份有效订阅。
- `RecordEvent`：上报用量，按发生时刻归入 UTC 自然月账期，账户内事件标识去重。
- `CreateBill` / `GetBill`：为已结束的账期出账，重复出账得到同一张账单。
- `RecordPayment`：分次登记付款，账户内付款标识去重。
- `MonthlyUsage` / `Status`：查询各月累计用量、账单余额与欠费停用状态。
- `SchedulePlanSwitch`：安排从下一个 UTC 自然月起切换套餐（升级降级同规则），返回目标套餐条件与生效账期。
- `CancelSchedule`：取消待生效的套餐切换安排。

金额与用量均为非负 `int64`，金额单位为分，税率为万分比（0–10000）。
到期欠费的账户会被停用，结清全部到期欠费账单后恢复。
