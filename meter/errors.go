package meter

import (
	"errors"
	"fmt"
)

// 业务错误。请求失败时通过 errors.Is 可判定具体原因。
var (
	// ErrInvalidArgument 表示参数非法：空标识、负数、税率越界或非法账期等。
	ErrInvalidArgument = errors.New("meter: invalid argument")
	// ErrOverflow 表示累计用量或金额超出 int64 范围。
	ErrOverflow = errors.New("meter: overflow")

	// ErrAccountExists 账户标识已存在。
	ErrAccountExists = errors.New("meter: account already exists")
	// ErrAccountNotFound 账户不存在。
	ErrAccountNotFound = errors.New("meter: account not found")

	// ErrPlanExists 套餐标识已存在。
	ErrPlanExists = errors.New("meter: plan already exists")
	// ErrPlanNotFound 套餐不存在。
	ErrPlanNotFound = errors.New("meter: plan not found")

	// ErrSubscriptionExists 该账户已有一份有效订阅。
	ErrSubscriptionExists = errors.New("meter: subscription already exists")
	// ErrSubscriptionNotFound 该账户尚未开通订阅。
	ErrSubscriptionNotFound = errors.New("meter: subscription not found")

	// ErrEventConflict 事件标识相同但时刻或数量不同。
	ErrEventConflict = errors.New("meter: event conflict")
	// ErrEventBeforeSubscription 事件发生时刻早于订阅开通时刻。
	ErrEventBeforeSubscription = errors.New("meter: event before subscription")
	// ErrEventInFuture 事件发生时刻晚于当前时刻。
	ErrEventInFuture = errors.New("meter: event in the future")
	// ErrMonthBilled 该账期已出账，拒绝首次出现的新事件。
	ErrMonthBilled = errors.New("meter: month already billed")
	// ErrSuspended 账户因到期欠费被停用，拒绝新用量。
	ErrSuspended = errors.New("meter: account suspended")

	// ErrBillNotFound 指定账期尚未生成账单。
	ErrBillNotFound = errors.New("meter: bill not found")
	// ErrBillBeforeSubscription 只能为开通当月及以后生成账单。
	ErrBillBeforeSubscription = errors.New("meter: bill period before subscription")
	// ErrBillMonthNotEnded 只能为已经结束的月份生成账单。
	ErrBillMonthNotEnded = errors.New("meter: bill period has not ended")

	// ErrPaymentConflict 付款标识相同但账单或金额不同。
	ErrPaymentConflict = errors.New("meter: payment conflict")
	// ErrPaymentExceedsBalance 付款金额大于账单当前余额。
	ErrPaymentExceedsBalance = errors.New("meter: payment exceeds balance")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, args...))
}
