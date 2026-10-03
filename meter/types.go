package meter

import "time"

// Plan 是套餐的当前定义。修改套餐只影响之后开通的订阅。
type Plan struct {
	// ID 套餐唯一标识，不可为空。
	ID string
	// MonthlyFee 月费，单位为分，非负。
	MonthlyFee int64
	// IncludedUnits 每月包含用量，非负。
	IncludedUnits int64
	// OveragePrice 超出部分每单位价格（分/单位），非负。
	OveragePrice int64
	// TaxRateBasisPoints 税率，万分比，取值 [0, 10000]。
	TaxRateBasisPoints int64
}

// PlanTerms 是开通订阅时保存的套餐条件快照。
// 之后修改套餐不影响使用该快照的订阅计费。
type PlanTerms struct {
	PlanID             string
	MonthlyFee         int64
	IncludedUnits      int64
	OveragePrice       int64
	TaxRateBasisPoints int64
}

func termsOf(p Plan) PlanTerms {
	return PlanTerms{
		PlanID:             p.ID,
		MonthlyFee:         p.MonthlyFee,
		IncludedUnits:      p.IncludedUnits,
		OveragePrice:       p.OveragePrice,
		TaxRateBasisPoints: p.TaxRateBasisPoints,
	}
}

// Subscription 是一份有效订阅。每个账户只允许一份。
type Subscription struct {
	AccountID string
	// PlanID 开通时所依据的套餐标识。
	PlanID string
	// Terms 开通时刻保存的套餐条件快照。
	Terms PlanTerms
	// ActivatedAt 开通时刻，UTC。用量事件不得早于该时刻。
	ActivatedAt time.Time
}

// PlanChange 是一条尚未生效的换套餐安排。
type PlanChange struct {
	// TargetPlanID 目标套餐标识。
	TargetPlanID string
	// Terms 安排被接受时锁定的目标套餐完整条件快照；
	// 之后修改套餐定义不影响本安排。
	Terms PlanTerms
	// EffectivePeriod 生效账期：安排被接受时的下一个 UTC 自然月。
	EffectivePeriod Month
}

// PlanChangeResult 是换套餐安排成功提交的结果。
type PlanChangeResult struct {
	// Created 为 true 表示本次新建或替换了安排；
	// 为 false 表示目标套餐与已有安排相同，返回原安排，未重新取价。
	Created bool
	// Change 安排内容：目标套餐完整计费条件与生效账期。
	Change PlanChange
}

// SubscriptionCancellation 是一次按月取消订阅安排。
type SubscriptionCancellation struct {
	// EndAt 终止时刻（UTC）：取消请求时刻的下一个 UTC 自然月月初零点。
	// 终止时刻计入空档，不计入订阅期间。
	EndAt time.Time
}

// CancelSubscriptionResult 是取消订阅请求的结果。
type CancelSubscriptionResult struct {
	// Cancelled 为 true 表示本次新登记了取消安排；
	// 为 false 表示此前已安排取消，本次原样返回原终止时刻，不后移期限。
	Cancelled bool
	// Cancellation 取消安排内容。
	Cancellation SubscriptionCancellation
}

// termsEntry 是订阅条件历史中的一条：自 EffectivePeriod（含）起适用 Terms。
type termsEntry struct {
	effective Month
	terms     PlanTerms
}

// Event 是一条用量事件。
type Event struct {
	// AccountID 所属账户。
	AccountID string
	// EventID 事件标识，在同一账户内去重；不同账户可复用。
	EventID string
	// At 事件发生时刻，按此时刻归入账期（UTC 自然月）。
	At time.Time
	// Quantity 数量，非负。
	Quantity int64
}

// EventResult 是用量上报的结果。
type EventResult struct {
	// Accepted 为 true 表示该事件本次被接收并累计；
	// 为 false 表示这是此前已接收事件的完全相同重报，未再次累计。
	Accepted bool
	// Period 事件归入账期。
	Period Month
}

// Month 标识一个 UTC 自然月账期。
type Month struct {
	Year  int
	Month time.Month
}

// MonthOf 返回时刻 t（UTC）所在账期。
func MonthOf(t time.Time) Month {
	u := t.UTC()
	return Month{Year: u.Year(), Month: u.Month()}
}

// Start 返回账期开始时刻（UTC，当月 1 日 00:00:00）。
func (m Month) Start() time.Time {
	return time.Date(m.Year, m.Month, 1, 0, 0, 0, 0, time.UTC)
}

// End 返回账期结束时刻（次月 1 日 00:00:00 UTC），即下月账期开始。
func (m Month) End() time.Time {
	return m.Start().AddDate(0, 1, 0)
}

// Before 报告 m 是否早于 other。
func (m Month) Before(other Month) bool {
	if m.Year != other.Year {
		return m.Year < other.Year
	}
	return m.Month < other.Month
}

// String 返回 YYYY-MM 形式。
func (m Month) String() string {
	return m.Start().Format("2006-01")
}

// Bill 是一个账期的账单。出账后金额与用量明细固定，付款状态可继续变化。
type Bill struct {
	AccountID string
	Period    Month
	// Terms 出账采用的套餐条件：该账期当时适用的条件快照
	// （开通快照或某次已生效换套餐安排锁定的快照）。
	Terms PlanTerms
	// TotalUsage 账期内总用量。
	TotalUsage int64
	// IncludedUnits 包含额度。
	IncludedUnits int64
	// OverageUnits 超额用量 = max(0, TotalUsage - IncludedUnits)。
	OverageUnits int64
	// MonthlyFee 月费（分）。
	MonthlyFee int64
	// OverageFee 超额费用（分）。
	OverageFee int64
	// Tax 税额（分），对月费与超额费用之和按万分比四舍五入。
	Tax int64
	// TotalDue 应付总额（分）= MonthlyFee + OverageFee + Tax。
	TotalDue int64
	// Paid 已付金额（分）。
	Paid int64
	// Balance 剩余应付（分）= TotalDue - Paid。
	Balance int64
	// Settled 是否已付清；零金额账单出账时即为 true。
	Settled bool
	// DueAt 付款截止时刻：账期结束后七天（UTC）。
	DueAt time.Time
}

// Payment 是一次本地付款登记记录。
type Payment struct {
	AccountID string
	// PaymentID 付款标识，在同一账户内去重。
	PaymentID string
	// Period 付款所属账期（账单）。
	Period Month
	// Amount 付款金额（分），大于零且不超过账单当时余额。
	Amount int64
}

// PaymentResult 是付款登记结果。
type PaymentResult struct {
	// Registered 为 true 表示本次实际登记；false 表示完全相同重报。
	Registered bool
	Payment    Payment
	// BillBalance 登记后账单余额。
	BillBalance int64
	// Settled 登记后账单是否已付清。
	Settled bool
}

// Usage 是某个账期的累计用量查询结果。
type Usage struct {
	Period Month
	// Total 该账期已累计用量（出账后也保持固定）。
	Total int64
}

// AccountStatus 是账户当前状态查询结果。
type AccountStatus struct {
	AccountID string
	// Subscribed 当前是否有已实际生效的订阅。
	// 提前登记但开通时刻尚未到达的订阅不计入：以实际开通时刻为界，不按开通
	// 月份提前生效（开通当日凌晨仍为 false，到达开通时刻才为 true）。
	// 含已安排取消、尚未到终止时刻的等待期；到达终止时刻后即使没有上报用量
	// 或生成账单，也立即变为 false。
	Subscribed bool
	// Suspended 是否因到期欠费被停用（结清全部到期欠费账单后恢复；
	// 结清债务只解除停用，不复活已取消的订阅，也不让等待开通的订阅提前
	// 生效；等待开通本身不清除旧欠费，Subscribed 为 false 时仍可能停用）。
	Suspended bool
	// CurrentTerms 当前账期实际生效的套餐条件；无已生效订阅（含等待开通
	// 期间）时为零值。到达生效月份月初零点后，无需任何操作即为换套餐后
	// 的条件。
	CurrentTerms PlanTerms
	// PendingChange 尚未生效的换套餐安排；没有安排时为 nil。
	// 订阅未生效期间不允许安排，故等待开通时必为 nil。
	// 登记取消时会清除待生效安排，等待取消期间拒绝新安排。
	PendingChange *PlanChange
	// ScheduledEnd 已安排的订阅终止时刻（下一个 UTC 自然月月初）；
	// 未安排取消、等待开通期间或终止已发生时为 nil。等待取消期间
	// Subscribed 仍为 true。
	ScheduledEnd *time.Time
	// MonthlyUsage 各账期累计用量，按账期先后排列。
	MonthlyUsage []Usage
	// Bills 已生成账单的余额与付清状态，按账期先后排列。
	Bills []BillSummary
}

// BillSummary 是账单的付款状态摘要。
type BillSummary struct {
	Period   Month
	TotalDue int64
	Paid     int64
	Balance  int64
	Settled  bool
	DueAt    time.Time
}
