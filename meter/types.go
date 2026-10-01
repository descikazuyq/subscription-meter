package meter

import "time"

// BillStatus 表示账单状态。
type BillStatus string

const (
	BillStatusUnpaid BillStatus = "unpaid"
	BillStatusPaid   BillStatus = "paid"
)

// Account 是业务账户。
type Account struct {
	ID        string
	CreatedAt time.Time
}

// Plan 是套餐条件。金额单位为分，用量为非负整数，税率为万分比（0..10000）。
type Plan struct {
	ID            string
	Name          string
	MonthlyFee    int64 // 月费，单位分
	IncludedUsage int64 // 每月包含用量
	OveragePrice  int64 // 超出部分每单位价格，单位分
	TaxRateBP     int64 // 税率，万分比
}

// Subscription 是开通时保存的套餐条件快照，之后修改套餐不影响其计费。
type Subscription struct {
	AccountID     string
	PlanID        string
	PlanName      string
	MonthlyFee    int64
	IncludedUsage int64
	OveragePrice  int64
	TaxRateBP     int64
	ActivatedAt   time.Time
}

// UsageEvent 是一条用量事件。
type UsageEvent struct {
	AccountID string
	EventID   string
	At        time.Time
	Quantity  int64
}

// Bill 是一张月账单，金额单位为分。出账后用量与金额固定，付款状态可继续变化。
type Bill struct {
	AccountID     string
	Period        Period
	PlanID        string
	PlanName      string
	MonthlyFee    int64
	IncludedUsage int64
	OveragePrice  int64
	TaxRateBP     int64

	TotalUsage   int64
	OverageUsage int64
	OverageFee   int64
	Tax          int64
	TotalAmount  int64
	PaidAmount   int64
	Status       BillStatus

	GeneratedAt time.Time
	DueAt       time.Time
}

// Payment 是一笔本地付款，针对某张账单的某个账期。
type Payment struct {
	AccountID string
	PaymentID string
	Period    Period
	Amount    int64
	PaidAt    time.Time
}
