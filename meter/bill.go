package meter

import "time"

// GenerateBill 为账户的指定账期生成账单。
//
// 只能为开通当月及以后、且已经结束的月份生成账单。相同账户与月份反复出账
// 得到同一张账单；出账后用量与金额固定，付款状态可继续变化。
// 账单生成时为未付款，零金额账单直接标为已付清。
// 若累计用量或最终金额超出 int64 范围，请求失败且不留下部分结果。
func (m *Meter) GenerateBill(accountID string, year int, month time.Month) (*Bill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.accounts[accountID]; !ok {
		return nil, ErrAccountNotFound
	}
	sub, ok := m.subscriptions[accountID]
	if !ok {
		return nil, ErrNoSubscription
	}
	p := Period{Year: year, Month: month}

	// 幂等：已出账直接返回原账单。
	if existing, ok := m.bills[accountID][p]; ok {
		return existing, nil
	}

	now := m.now()
	if periodCompare(p, periodOf(sub.ActivatedAt)) < 0 {
		return nil, ErrPeriodBeforeActivation
	}
	if periodCompare(p, periodOf(now)) >= 0 {
		return nil, ErrPeriodNotEnded
	}

	// 汇总该账期用量。
	var total int64
	for _, ev := range m.events[accountID] {
		if periodCompare(periodOf(ev.At), p) != 0 {
			continue
		}
		sum, ok := add64(total, ev.Quantity)
		if !ok {
			return nil, ErrOverflow
		}
		total = sum
	}

	overage := int64(0)
	if total > sub.IncludedUsage {
		overage = total - sub.IncludedUsage
	}
	overageFee, ok := mul64(overage, sub.OveragePrice)
	if !ok {
		return nil, ErrOverflow
	}
	base, ok := add64(sub.MonthlyFee, overageFee)
	if !ok {
		return nil, ErrOverflow
	}
	tax, ok := taxCents(base, sub.TaxRateBP)
	if !ok {
		return nil, ErrOverflow
	}
	totalAmount, ok := add64(base, tax)
	if !ok {
		return nil, ErrOverflow
	}

	bill := &Bill{
		AccountID:     accountID,
		Period:        p,
		PlanID:        sub.PlanID,
		PlanName:      sub.PlanName,
		MonthlyFee:    sub.MonthlyFee,
		IncludedUsage: sub.IncludedUsage,
		OveragePrice:  sub.OveragePrice,
		TaxRateBP:     sub.TaxRateBP,
		TotalUsage:    total,
		OverageUsage:  overage,
		OverageFee:    overageFee,
		Tax:           tax,
		TotalAmount:   totalAmount,
		PaidAmount:    0,
		Status:        BillStatusUnpaid,
		GeneratedAt:   now,
		DueAt:         periodEnd(p).Add(7 * 24 * time.Hour),
	}
	if totalAmount == 0 {
		bill.Status = BillStatusPaid
	}

	if m.bills[accountID] == nil {
		m.bills[accountID] = map[Period]*Bill{}
	}
	m.bills[accountID][p] = bill
	return bill, nil
}
