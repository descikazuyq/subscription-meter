package meter

import (
	"sort"
	"time"
)

// MonthlyUsage 返回账户在指定账期的累计用量。
func (m *Meter) MonthlyUsage(accountID string, year int, month time.Month) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[accountID]; !ok {
		return 0, ErrAccountNotFound
	}
	p := Period{Year: year, Month: month}
	var total int64
	for _, ev := range m.events[accountID] {
		if periodCompare(periodOf(ev.At), p) != 0 {
			continue
		}
		sum, ok := add64(total, ev.Quantity)
		if !ok {
			return 0, ErrOverflow
		}
		total = sum
	}
	return total, nil
}

// BillBalance 返回指定账期账单的未付余额。
func (m *Meter) BillBalance(accountID string, year int, month time.Month) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[accountID]; !ok {
		return 0, ErrAccountNotFound
	}
	bill, ok := m.bills[accountID][Period{Year: year, Month: month}]
	if !ok {
		return 0, ErrBillNotFound
	}
	return bill.TotalAmount - bill.PaidAmount, nil
}

// IsSuspended 返回账户当前是否因到期未付账单而停用。
func (m *Meter) IsSuspended(accountID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[accountID]; !ok {
		return false, ErrAccountNotFound
	}
	return m.suspendedLocked(accountID, m.now()), nil
}

// ListBills 返回账户的全部账单，按账期先后排序。
func (m *Meter) ListBills(accountID string) ([]*Bill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[accountID]; !ok {
		return nil, ErrAccountNotFound
	}
	out := make([]*Bill, 0, len(m.bills[accountID]))
	for _, b := range m.bills[accountID] {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		return periodCompare(out[i].Period, out[j].Period) < 0
	})
	return out, nil
}
