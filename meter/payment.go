package meter

import (
	"math"
	"time"
)

// RegisterPayment 为指定账期的账单登记一笔本地付款。
//
// 每次付款必须大于零且不超过账单余额。付款标识在同一账户内去重：
// 相同账单与金额的重报返回原结果；改成其他账单或金额时报冲突。
// 失败请求不消耗付款标识。
func (m *Meter) RegisterPayment(accountID, paymentID string, year int, month time.Month, amount int64) (*Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.accounts[accountID]; !ok {
		return nil, ErrAccountNotFound
	}
	if amount <= 0 {
		return nil, ErrInvalidPayment
	}
	p := Period{Year: year, Month: month}
	bill, ok := m.bills[accountID][p]
	if !ok {
		return nil, ErrBillNotFound
	}

	// 去重：相同账单与金额返回原结果，否则冲突。
	if existing, ok := m.payments[accountID][paymentID]; ok {
		if existing.Period == p && existing.Amount == amount {
			return existing, nil
		}
		return nil, ErrPaymentConflict
	}

	balance, ok := sub64(bill.TotalAmount, bill.PaidAmount)
	if !ok || amount > balance {
		return nil, ErrPaymentExceedsBalance
	}

	pay := &Payment{
		AccountID: accountID,
		PaymentID: paymentID,
		Period:    p,
		Amount:    amount,
		PaidAt:    m.now(),
	}
	if m.payments[accountID] == nil {
		m.payments[accountID] = map[string]*Payment{}
	}
	m.payments[accountID][paymentID] = pay

	bill.PaidAmount += amount
	if bill.PaidAmount >= bill.TotalAmount {
		bill.Status = BillStatusPaid
	}
	return pay, nil
}

// sub64 做带溢出检查的 int64 减法。
func sub64(a, b int64) (int64, bool) {
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	return a - b, true
}
