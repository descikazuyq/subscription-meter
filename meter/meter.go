package meter

import (
	"sync"
	"time"
)

// Meter 是本地订阅、用量与账单系统。所有状态操作在进程内由同一把互斥锁保护，
// 因此并发重复提交也只会累计一次用量、生成一张月账单、扣减一次余额。
type Meter struct {
	mu            sync.Mutex
	now           func() time.Time
	accounts      map[string]*Account
	plans         map[string]*Plan
	subscriptions map[string]*Subscription
	events        map[string]map[string]*UsageEvent // 账户 -> 事件标识 -> 事件
	bills         map[string]map[Period]*Bill       // 账户 -> 账期 -> 账单
	payments      map[string]map[string]*Payment    // 账户 -> 付款标识 -> 付款
}

// NewMeter 创建一个使用真实时钟的系统。
func NewMeter() *Meter {
	return &Meter{
		now:           func() time.Time { return time.Now().UTC() },
		accounts:      map[string]*Account{},
		plans:         map[string]*Plan{},
		subscriptions: map[string]*Subscription{},
		events:        map[string]map[string]*UsageEvent{},
		bills:         map[string]map[Period]*Bill{},
		payments:      map[string]map[string]*Payment{},
	}
}

// NewMeterWithClock 创建一个使用指定时钟的系统，便于测试。
func NewMeterWithClock(now func() time.Time) *Meter {
	m := NewMeter()
	m.now = now
	return m
}

// CreateAccount 创建业务账户。账户标识必须唯一。
func (m *Meter) CreateAccount(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		return ErrIDEmpty
	}
	if _, ok := m.accounts[id]; ok {
		return ErrAccountExists
	}
	m.accounts[id] = &Account{ID: id, CreatedAt: m.now()}
	return nil
}

// CreatePlan 创建套餐。金额与用量必须非负，税率在 0 到 10000 之间。
func (m *Meter) CreatePlan(p Plan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		return ErrIDEmpty
	}
	if p.MonthlyFee < 0 || p.IncludedUsage < 0 || p.OveragePrice < 0 {
		return ErrNegativeAmount
	}
	if p.TaxRateBP < 0 || p.TaxRateBP > 10000 {
		return ErrInvalidTaxRate
	}
	if _, ok := m.plans[p.ID]; ok {
		return ErrPlanExists
	}
	cp := p
	m.plans[p.ID] = &cp
	return nil
}

// UpdatePlan 修改套餐条件。已有订阅保存的是开通时的快照，不受影响。
func (m *Meter) UpdatePlan(p Plan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		return ErrIDEmpty
	}
	if p.MonthlyFee < 0 || p.IncludedUsage < 0 || p.OveragePrice < 0 {
		return ErrNegativeAmount
	}
	if p.TaxRateBP < 0 || p.TaxRateBP > 10000 {
		return ErrInvalidTaxRate
	}
	if _, ok := m.plans[p.ID]; !ok {
		return ErrPlanNotFound
	}
	cp := p
	m.plans[p.ID] = &cp
	return nil
}

// ActivateSubscription 为账户开通订阅，保存当时的套餐条件。每个账户只允许一份有效订阅。
func (m *Meter) ActivateSubscription(accountID, planID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[accountID]; !ok {
		return ErrAccountNotFound
	}
	plan, ok := m.plans[planID]
	if !ok {
		return ErrPlanNotFound
	}
	if _, ok := m.subscriptions[accountID]; ok {
		return ErrSubscriptionExists
	}
	m.subscriptions[accountID] = &Subscription{
		AccountID:     accountID,
		PlanID:        planID,
		PlanName:      plan.Name,
		MonthlyFee:    plan.MonthlyFee,
		IncludedUsage: plan.IncludedUsage,
		OveragePrice:  plan.OveragePrice,
		TaxRateBP:     plan.TaxRateBP,
		ActivatedAt:   at.UTC(),
	}
	return nil
}

// suspendedLocked 判断账户是否因到期未付账单而停用。调用方须持有锁。
func (m *Meter) suspendedLocked(accountID string, now time.Time) bool {
	for _, b := range m.bills[accountID] {
		if b.Status != BillStatusPaid && now.After(b.DueAt) {
			return true
		}
	}
	return false
}
