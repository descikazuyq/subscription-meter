package meter

import "time"

// ReportUsage 上报一条用量事件。
//
// 事件按发生时刻归入账期；早于订阅开通时刻或晚于当前时刻的事件被拒绝。
// 事件标识在同一账户内去重：完全相同的重报返回原结果、不重复累计；
// 标识相同但时刻或数量不同则报冲突。不同账户可以使用相同事件标识。
// 已出账月份拒绝首次出现的新事件，但已接收事件的相同重报仍返回成功。
// 账户因欠费停用时拒绝新用量，欠费期间已接收事件的相同重报仍成功。
// 失败请求不消耗事件标识。
func (m *Meter) ReportUsage(accountID, eventID string, at time.Time, quantity int64) (*UsageEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.accounts[accountID]; !ok {
		return nil, ErrAccountNotFound
	}
	sub, ok := m.subscriptions[accountID]
	if !ok {
		return nil, ErrNoSubscription
	}
	if quantity < 0 {
		return nil, ErrNegativeAmount
	}
	at = at.UTC()
	now := m.now()
	if at.Before(sub.ActivatedAt) {
		return nil, ErrEventBeforeActivation
	}
	if at.After(now) {
		return nil, ErrEventInFuture
	}

	// 去重优先于账期与停用校验：已接收事件的相同重报一律返回成功。
	if ev, ok := m.events[accountID][eventID]; ok {
		if ev.At.Equal(at) && ev.Quantity == quantity {
			return ev, nil
		}
		return nil, ErrEventConflict
	}

	p := periodOf(at)
	if _, ok := m.bills[accountID][p]; ok {
		return nil, ErrPeriodClosed
	}
	if m.suspendedLocked(accountID, now) {
		return nil, ErrAccountSuspended
	}

	ev := &UsageEvent{AccountID: accountID, EventID: eventID, At: at, Quantity: quantity}
	if m.events[accountID] == nil {
		m.events[accountID] = map[string]*UsageEvent{}
	}
	m.events[accountID][eventID] = ev
	return ev, nil
}
