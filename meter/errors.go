package meter

import "errors"

// 系统中可能出现的明确错误。调用方可以用 errors.Is 判定。
var (
	ErrIDEmpty                = errors.New("meter: 标识为空")
	ErrAccountExists          = errors.New("meter: 账户已存在")
	ErrAccountNotFound        = errors.New("meter: 账户不存在")
	ErrPlanExists             = errors.New("meter: 套餐已存在")
	ErrPlanNotFound           = errors.New("meter: 套餐不存在")
	ErrSubscriptionExists     = errors.New("meter: 账户已有有效订阅")
	ErrNoSubscription         = errors.New("meter: 账户无订阅")
	ErrNegativeAmount         = errors.New("meter: 金额或用量为负")
	ErrInvalidTaxRate         = errors.New("meter: 税率必须在 0 到 10000 之间")
	ErrEventBeforeActivation  = errors.New("meter: 事件早于订阅开通时刻")
	ErrEventInFuture          = errors.New("meter: 事件晚于当前时刻")
	ErrEventConflict          = errors.New("meter: 事件标识冲突")
	ErrPeriodClosed           = errors.New("meter: 账期已出账")
	ErrAccountSuspended       = errors.New("meter: 账户因欠费停用")
	ErrPeriodNotEnded         = errors.New("meter: 账期尚未结束")
	ErrPeriodBeforeActivation = errors.New("meter: 账期早于订阅开通月")
	ErrBillNotFound           = errors.New("meter: 账单不存在")
	ErrInvalidPayment         = errors.New("meter: 付款必须大于零")
	ErrPaymentExceedsBalance  = errors.New("meter: 付款超过账单余额")
	ErrPaymentConflict        = errors.New("meter: 付款标识冲突")
	ErrOverflow               = errors.New("meter: 数值超出 int64 范围")
)
