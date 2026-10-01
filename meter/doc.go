// Package meter 提供本地运行的订阅、用量与账单能力。
//
// 使用 NewService（或可注入时钟的 NewServiceWithClock）得到一个并发安全的
// 内存服务，典型流程为：
//
//	s := meter.NewService()
//	_ = s.CreateAccount("acct-1")
//	_ = s.CreatePlan(meter.Plan{
//	    ID: "plan-1", MonthlyFee: 1000, IncludedUnits: 100,
//	    OveragePrice: 5, TaxRateBasisPoints: 600,
//	})
//	_ = s.Subscribe("acct-1", "plan-1", time.Now().UTC())
//	_, _ = s.RecordEvent(meter.Event{
//	    AccountID: "acct-1", EventID: "evt-1",
//	    At: time.Now().UTC(), Quantity: 120,
//	})
//
// 规则摘要：
//   - 金额与用量均为非负 int64，金额单位为分；税率为万分比且取值 [0,10000]。
//   - 账户、套餐标识唯一；缺失引用、负数或非法税率的请求失败且不改已有记录。
//   - 开通订阅时保存套餐条件快照，之后修改套餐不影响该订阅计费；每账户一份订阅。
//   - 账期为 UTC 自然月，事件按发生时刻归期；早于开通或晚于当前时刻的事件拒绝。
//   - 事件/付款标识在账户内去重：完全相同的重报返回原结果，关键信息不同报冲突。
//   - 已出账月份拒绝新事件（已接收事件的重报仍成功）；账单只能为开通当月及以后
//     且已结束的月份生成，重复出账得到同一张，金额与用量固定、付款状态可变。
//   - 付款截止为账期结束后七天，支持分次付款；到期仍有余额则停用，结清后恢复。
//   - 用量或金额累计溢出 int64 时请求失败，不留下部分结果。
package meter

// Ready 表示基线可以运行。
func Ready() bool { return true }
