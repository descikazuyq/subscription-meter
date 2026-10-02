package meter

import (
	"sort"
	"sync"
	"time"
)

// Service 是订阅、用量与账单的本地内存服务。
// 单个 *Service 可被多个 goroutine 并发使用：重复提交只会生效一次。
type Service struct {
	mu       sync.Mutex
	now      func() time.Time
	accounts map[string]*account
	plans    map[string]Plan
}

type subRecord struct {
	planID      string
	terms       PlanTerms
	activatedAt time.Time
	// terminatedAt 为零值表示尚未终止；非零表示订阅已在该时刻（UTC）终止，
	// 该时刻不计入订阅期间。
	terminatedAt time.Time
	// cancelAt 为零值表示没有待生效的取消；非零表示已申请取消，等待该时刻终止。
	cancelAt time.Time
	// termsHistory 按生效月份升序记录每次换套餐后的条件；
	// 首条为开通当月适用的开通快照。账单按账期从中取当时适用的条件。
	termsHistory []termsEntry
	// pending 尚未生效的换套餐安排，每账户至多一条；nil 表示无安排。
	pending *PlanChange
}

type eventRecord struct {
	at     time.Time
	qty    int64
	period Month
}

type billRecord struct {
	bill Bill
}

type paymentRecord struct {
	period Month
	amount int64
}

type account struct {
	id string
	// subs 保存所有订阅记录，按开通时刻升序；最后一份为当前订阅（若已终止则无有效订阅）。
	// 重新开通时追加新记录，旧记录保留以供历史账期取条件。
	subs     []*subRecord
	events   map[string]eventRecord
	usage    map[Month]int64
	bills    map[Month]*billRecord
	payments map[string]paymentRecord
}

// currentSub 返回当前有效订阅（最后一份未终止的订阅）；没有则返回 nil。
func (acc *account) currentSub() *subRecord {
	if len(acc.subs) == 0 {
		return nil
	}
	last := acc.subs[len(acc.subs)-1]
	if last.terminatedAt.IsZero() {
		return last
	}
	return nil
}

// lastSub 返回最后一份订阅记录（无论是否终止）；没有则返回 nil。
func (acc *account) lastSub() *subRecord {
	if len(acc.subs) == 0 {
		return nil
	}
	return acc.subs[len(acc.subs)-1]
}

// subCovering 返回覆盖指定时刻的订阅（activatedAt <= at < terminatedAt）；没有则返回 nil。
func (acc *account) subCovering(at time.Time) *subRecord {
	for _, sub := range acc.subs {
		if !sub.activatedAt.After(at) && (sub.terminatedAt.IsZero() || sub.terminatedAt.After(at)) {
			return sub
		}
	}
	return nil
}

// subForPeriod 返回覆盖指定账期的订阅（订阅期间与该月有交集）；没有则返回 nil。
func (acc *account) subForPeriod(period Month) *subRecord {
	for _, sub := range acc.subs {
		if sub.activatedAt.Before(period.End()) && (sub.terminatedAt.IsZero() || sub.terminatedAt.After(period.Start())) {
			return sub
		}
	}
	return nil
}

// NewService 创建一个以系统时钟判断“当前时刻”的服务。
func NewService() *Service {
	return NewServiceWithClock(time.Now)
}

// NewServiceWithClock 创建使用自定义时钟的服务，便于测试账期与截止时刻。
func NewServiceWithClock(now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		now:      now,
		accounts: make(map[string]*account),
		plans:    make(map[string]Plan),
	}
}

func (s *Service) nowUTC() time.Time {
	return s.now().UTC()
}

// CreateAccount 创建业务账户。账户标识必须非空且唯一。
func (s *Service) CreateAccount(id string) error {
	if id == "" {
		return invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; ok {
		return ErrAccountExists
	}
	s.accounts[id] = &account{
		id:       id,
		events:   make(map[string]eventRecord),
		usage:    make(map[Month]int64),
		bills:    make(map[Month]*billRecord),
		payments: make(map[string]paymentRecord),
	}
	return nil
}

func validatePlan(p Plan) error {
	if p.ID == "" {
		return invalidf("plan id is empty")
	}
	if p.MonthlyFee < 0 || p.IncludedUnits < 0 || p.OveragePrice < 0 {
		return invalidf("plan %q has negative amount or quantity", p.ID)
	}
	if p.TaxRateBasisPoints < 0 || p.TaxRateBasisPoints > 10000 {
		return invalidf("plan %q tax rate %d out of [0,10000]", p.ID, p.TaxRateBasisPoints)
	}
	return nil
}

// CreatePlan 创建套餐。金额单位为分，税率为万分比且在 [0,10000]。
func (s *Service) CreatePlan(p Plan) error {
	if err := validatePlan(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[p.ID]; ok {
		return ErrPlanExists
	}
	s.plans[p.ID] = p
	return nil
}

// UpdatePlan 更新套餐当前条件。只影响之后开通的订阅，
// 已开通订阅继续使用开通时保存的快照。
func (s *Service) UpdatePlan(p Plan) error {
	if err := validatePlan(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.plans[p.ID]; !ok {
		return ErrPlanNotFound
	}
	s.plans[p.ID] = p
	return nil
}

// Subscribe 为账户开通订阅，并保存当时的套餐条件快照。
// 每个账户只允许一份有效订阅；从 activatedAt 起接收用量，
// 首月仍收完整月费并提供完整额度。
//
// 终止后可通过本方法重新开通：开通时刻不得早于上次终止时刻，
// 新订阅保存重新开通时的套餐条件快照，当月仍收完整月费并给完整额度。
// 重新开通不重置账户内事件与付款标识，也不清除旧欠费。
// 有尚未终止的订阅时再次开通返回 ErrSubscriptionExists。
func (s *Service) Subscribe(accountID, planID string, activatedAt time.Time) error {
	if accountID == "" {
		return invalidf("account id is empty")
	}
	if planID == "" {
		return invalidf("plan id is empty")
	}
	if activatedAt.IsZero() {
		return invalidf("activated at is zero")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return ErrAccountNotFound
	}
	plan, ok := s.plans[planID]
	if !ok {
		return ErrPlanNotFound
	}
	// 先让已到期的取消生效：若订阅已终止，currentSub 返回 nil，允许重新开通。
	s.applyDueChangesLocked(acc, s.nowUTC())
	if acc.currentSub() != nil {
		return ErrSubscriptionExists
	}
	activatedAt = activatedAt.UTC()
	if last := acc.lastSub(); last != nil {
		if activatedAt.Before(last.terminatedAt) {
			return ErrReactivationTooEarly
		}
	}
	acc.subs = append(acc.subs, &subRecord{
		planID:      planID,
		terms:       termsOf(plan),
		activatedAt: activatedAt,
		termsHistory: []termsEntry{{
			effective: MonthOf(activatedAt),
			terms:     termsOf(plan),
		}},
	})
	return nil
}

// nextMonth 返回 period 的下一个 UTC 自然月。
func nextMonth(period Month) Month {
	return MonthOf(period.End())
}

// applyDueChangesLocked 把已到生效月份月初零点的待生效安排落入条件历史，
// 并在取消时刻到达时终止订阅。
// 到达月初零点即生效，无需先上报用量或生成账单；即使中间若干月没有调用服务，
// 再次进入时也会在此一次性补齐，当前条件仍正确。调用时持有 s.mu。
func (s *Service) applyDueChangesLocked(acc *account, now time.Time) {
	sub := acc.currentSub()
	if sub == nil {
		return
	}
	current := MonthOf(now)
	for sub.pending != nil && !current.Before(sub.pending.EffectivePeriod) {
		entry := termsEntry{
			effective: sub.pending.EffectivePeriod,
			terms:     sub.pending.Terms,
		}
		sub.termsHistory = append(sub.termsHistory, entry)
		sub.planID = sub.pending.TargetPlanID
		sub.terms = sub.pending.Terms
		sub.pending = nil
	}
	// 取消时刻到达即终止订阅：终止时刻不计入订阅期间。
	if !sub.cancelAt.IsZero() && !now.Before(sub.cancelAt) {
		sub.terminatedAt = sub.cancelAt
		sub.pending = nil
		sub.cancelAt = time.Time{}
	}
}

// termsForPeriodLocked 返回指定账期当时适用的套餐条件快照。
// 条件历史按生效月份升序，取不晚于 period 的最后一条；
// 这样历史账单始终按该账期条件计算，不受后续换套餐影响。调用时持有 s.mu。
func termsForPeriodLocked(sub *subRecord, period Month) PlanTerms {
	t := sub.termsHistory[0].terms
	for _, e := range sub.termsHistory {
		if period.Before(e.effective) {
			break
		}
		t = e.terms
	}
	return t
}

// SchedulePlanChange 安排账户从下一个 UTC 自然月起改用另一套餐，升级与降级规则相同。
//
// 成功时返回目标套餐在本次安排时的完整计费条件快照与生效账期（请求被接受时的
// 下一个 UTC 自然月），之后修改套餐定义不影响本安排。当前月仍按旧套餐收取完整
// 月费并提供完整额度，不按天补差，也不迁移本月已发生的用量。
//
// 每个账户至多保留一条尚未生效的安排：再次安排同一目标套餐时返回原安排，
// 不重新取价（Created 为 false）；安排另一目标套餐则替换原安排，生效月份仍为
// 本次请求被接受时的下一月。欠费停用期间也可以安排。
//
// 等待取消期间（已申请取消但尚未到终止时刻）拒绝新换套餐安排。
// 账户不存在、尚未开通订阅、开通时刻尚未到达、目标套餐不存在、目标套餐与当前
// 生效套餐相同或订阅正在取消中时请求失败，原有安排保持不变。
func (s *Service) SchedulePlanChange(accountID, targetPlanID string) (PlanChangeResult, error) {
	if accountID == "" {
		return PlanChangeResult{}, invalidf("account id is empty")
	}
	if targetPlanID == "" {
		return PlanChangeResult{}, invalidf("target plan id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[accountID]
	if !ok {
		return PlanChangeResult{}, ErrAccountNotFound
	}
	now := s.nowUTC()
	// 先让已到期的安排生效：例如跨月后的首次调用，此时“当前套餐”已是新套餐。
	s.applyDueChangesLocked(acc, now)
	sub := acc.currentSub()
	if sub == nil {
		return PlanChangeResult{}, ErrSubscriptionNotFound
	}
	if now.Before(sub.activatedAt) {
		return PlanChangeResult{}, ErrSubscriptionNotActivated
	}
	// 等待取消期间拒绝新换套餐安排。
	if !sub.cancelAt.IsZero() && now.Before(sub.cancelAt) {
		return PlanChangeResult{}, ErrSubscriptionCancelling
	}

	plan, ok := s.plans[targetPlanID]
	if !ok {
		return PlanChangeResult{}, ErrPlanNotFound
	}
	if targetPlanID == sub.planID {
		return PlanChangeResult{}, ErrPlanChangeSamePlan
	}

	// 已有安排且目标相同：原样返回，不重新取价。
	if sub.pending != nil && sub.pending.TargetPlanID == targetPlanID {
		return PlanChangeResult{Created: false, Change: *sub.pending}, nil
	}

	change := PlanChange{
		TargetPlanID:    targetPlanID,
		Terms:           termsOf(plan),
		EffectivePeriod: nextMonth(MonthOf(now)),
	}
	sub.pending = &change
	return PlanChangeResult{Created: true, Change: change}, nil
}

// CancelPlanChange 在安排生效前取消它，取消后账户继续沿用当前套餐。
// 没有待生效安排时取消也成功；已生效的切换不会被撤回。
// 欠费停用期间也可以取消，取消不清除欠费、不解除停用。
// 账户不存在或没有有效订阅时失败。
func (s *Service) CancelPlanChange(accountID string) error {
	if accountID == "" {
		return invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return ErrAccountNotFound
	}
	// 已到生效月份月初零点的安排视为已生效，不可撤回。
	s.applyDueChangesLocked(acc, s.nowUTC())
	sub := acc.currentSub()
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	sub.pending = nil
	return nil
}

// CancelSubscription 为有效订阅申请按月取消。
//
// 订阅已生效时提交取消，以请求时刻的下一个 UTC 自然月月初为终止时刻并返回该时刻；
// 当月照常接收用量，按完整月费和额度计费，不按天退款。
// 欠费停用不妨碍取消，也不免除债务。
// 同一订阅重复取消始终返回原终止时刻，不把期限后移。
// 取消成功时清除尚未生效的换套餐安排；等待取消期间拒绝新换套餐安排。
// 若请求恰在换套餐生效的月初，应先承认该次切换，取消从再下一月生效。
// 账户不存在、没有有效订阅或开通时刻尚未到达时失败。
func (s *Service) CancelSubscription(accountID string) (time.Time, error) {
	if accountID == "" {
		return time.Time{}, invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return time.Time{}, ErrAccountNotFound
	}
	now := s.nowUTC()
	currentMonth := MonthOf(now)
	atMonthStart := now.Equal(currentMonth.Start())

	sub := acc.currentSub()
	if sub == nil {
		return time.Time{}, ErrSubscriptionNotFound
	}
	if now.Before(sub.activatedAt) {
		return time.Time{}, ErrSubscriptionNotActivated
	}

	// 若请求恰在换套餐生效的月初，先承认该次切换，取消从再下一月生效。
	// 换套餐可能已被此前的查询调用落入条件历史，因此同时检查条件历史末条。
	hadChangeDueNow := false
	if sub.pending != nil && currentMonth.Equal(sub.pending.EffectivePeriod) {
		hadChangeDueNow = true
	}
	if len(sub.termsHistory) > 1 && currentMonth.Equal(sub.termsHistory[len(sub.termsHistory)-1].effective) {
		hadChangeDueNow = true
	}
	s.applyDueChangesLocked(acc, now)
	// applyDueChangesLocked 后 sub 指针仍指向当前订阅记录。

	// 同一订阅重复取消始终返回原终止时刻，不把期限后移。
	if !sub.cancelAt.IsZero() {
		return sub.cancelAt, nil
	}

	cancelMonth := nextMonth(currentMonth)
	if atMonthStart && hadChangeDueNow {
		cancelMonth = nextMonth(cancelMonth)
	}
	cancelAt := cancelMonth.Start()

	// 取消成功时清除尚未生效的换套餐安排。
	sub.pending = nil
	sub.cancelAt = cancelAt
	return cancelAt, nil
}

// WithdrawCancellation 撤回尚未生效的取消安排。
//
// 生效前可撤回；有效订阅没有取消安排时撤回也成功；终止后撤回失败。
// 撤回取消不恢复被清除的换套餐安排。
// 账户不存在或没有有效订阅时失败。
func (s *Service) WithdrawCancellation(accountID string) error {
	if accountID == "" {
		return invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return ErrAccountNotFound
	}
	now := s.nowUTC()
	// 先让已到期的取消生效：若已到终止时刻，订阅已终止，撤回失败。
	s.applyDueChangesLocked(acc, now)
	sub := acc.currentSub()
	if sub == nil {
		return ErrSubscriptionNotFound
	}
	// 没有取消安排时撤回也成功。
	if sub.cancelAt.IsZero() {
		return nil
	}
	// 已到终止时刻：撤回失败。
	if !now.Before(sub.cancelAt) {
		return ErrSubscriptionNotFound
	}
	sub.cancelAt = time.Time{}
	return nil
}

// RecordEvent 上报一条用量事件。
//
// 事件按发生时刻归入 UTC 自然月账期；必须属于某段实际订阅期间：
// 开通时刻计入、终止时刻不计入，空档期间拒绝。早于首次开通或晚于当前时刻的
// 事件拒绝。事件标识在同一账户内去重：完全相同的重报返回原结果且不重复累计，
// 相同标识但时刻或数量不同返回 ErrEventConflict。
// 已出账月份拒绝新事件；账户因到期欠费停用时拒绝新事件；
// 但此前已接收事件的完全相同重报始终成功。
// 终止后仍允许补报旧订阅期间且尚未出账的用量，但到期欠费时仍拒绝新增。
func (s *Service) RecordEvent(e Event) (EventResult, error) {
	if e.AccountID == "" {
		return EventResult{}, invalidf("account id is empty")
	}
	if e.EventID == "" {
		return EventResult{}, invalidf("event id is empty")
	}
	if e.Quantity < 0 {
		return EventResult{}, invalidf("event %q has negative quantity %d", e.EventID, e.Quantity)
	}
	if e.At.IsZero() {
		return EventResult{}, invalidf("event %q has zero timestamp", e.EventID)
	}
	at := e.At.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[e.AccountID]
	if !ok {
		return EventResult{}, ErrAccountNotFound
	}
	if len(acc.subs) == 0 {
		return EventResult{}, ErrSubscriptionNotFound
	}

	// 进入即补齐已到期的换套餐安排与取消，保证并发下各操作观察到一致的状态。
	s.applyDueChangesLocked(acc, s.nowUTC())

	// 去重优先：完全相同的重报即使发生在出账后、停用期间或终止后也返回成功。
	if prev, ok := acc.events[e.EventID]; ok {
		if !prev.at.Equal(at) || prev.qty != e.Quantity {
			return EventResult{}, ErrEventConflict
		}
		return EventResult{Accepted: false, Period: prev.period}, nil
	}

	// 事件必须属于某段实际订阅期间：开通时刻计入、终止时刻不计入。
	sub := acc.subCovering(at)
	if sub == nil {
		if at.Before(acc.subs[0].activatedAt) {
			return EventResult{}, ErrEventBeforeSubscription
		}
		return EventResult{}, ErrEventOutsideSubscription
	}
	now := s.nowUTC()
	if at.After(now) {
		return EventResult{}, ErrEventInFuture
	}

	period := MonthOf(at)
	if _, billed := acc.bills[period]; billed {
		return EventResult{}, ErrMonthBilled
	}
	if s.isSuspendedLocked(acc, now) {
		return EventResult{}, ErrSuspended
	}

	total, ok := addNonNeg(acc.usage[period], e.Quantity)
	if !ok {
		return EventResult{}, ErrOverflow
	}
	// 所有校验通过后才落库：失败不消耗事件标识。
	acc.events[e.EventID] = eventRecord{at: at, qty: e.Quantity, period: period}
	acc.usage[period] = total
	return EventResult{Accepted: true, Period: period}, nil
}

// CreateBill 为账户指定账期生成账单（账期不存在则幂等返回同一张）。
// 只能为有订阅覆盖、且已经结束的月份生成账单；完全无订阅的月份出账失败。
// 重复调用得到同一账单；出账后金额与用量明细固定。
// 旧订阅覆盖的已结束月份仍可出账，使用各月当时锁定的条件。
func (s *Service) CreateBill(accountID string, period Month) (Bill, error) {
	if accountID == "" {
		return Bill{}, invalidf("account id is empty")
	}
	if period.Month < time.January || period.Month > time.December {
		return Bill{}, invalidf("invalid billing period %d-%d", period.Year, period.Month)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[accountID]
	if !ok {
		return Bill{}, ErrAccountNotFound
	}
	if len(acc.subs) == 0 {
		return Bill{}, ErrSubscriptionNotFound
	}
	if existing, ok := acc.bills[period]; ok {
		return existing.bill, nil
	}

	// 补齐到期安排（条件历史）与取消，随后按账期取该月当时适用的条件：
	// 即使已经换过套餐或先出新月份账单再补旧月份账单，历史账单也不会被当前套餐覆盖。
	s.applyDueChangesLocked(acc, s.nowUTC())

	sub := acc.subForPeriod(period)
	if sub == nil {
		return Bill{}, ErrBillBeforeSubscription
	}
	now := s.nowUTC()
	if !period.End().Before(now) && !period.End().Equal(now) {
		return Bill{}, ErrBillMonthNotEnded
	}

	terms := termsForPeriodLocked(sub, period)
	bill, err := buildBill(accountID, period, terms, acc.usage[period])
	if err != nil {
		return Bill{}, err
	}
	acc.bills[period] = &billRecord{bill: bill}
	return bill, nil
}

// GetBill 返回指定账期的账单；尚未出账返回 ErrBillNotFound。
func (s *Service) GetBill(accountID string, period Month) (Bill, error) {
	if accountID == "" {
		return Bill{}, invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return Bill{}, ErrAccountNotFound
	}
	rec, ok := acc.bills[period]
	if !ok {
		return Bill{}, ErrBillNotFound
	}
	return rec.bill, nil
}

// RecordPayment 为指定账单登记一笔大于零且不超过余额的本地付款，支持分次登记。
// 付款标识在同一账户内去重：相同账单和金额的重报返回原结果，
// 改成其他账单或金额返回 ErrPaymentConflict；失败不消耗付款标识。
func (s *Service) RecordPayment(accountID, paymentID string, period Month, amount int64) (PaymentResult, error) {
	if accountID == "" {
		return PaymentResult{}, invalidf("account id is empty")
	}
	if paymentID == "" {
		return PaymentResult{}, invalidf("payment id is empty")
	}
	if amount <= 0 {
		return PaymentResult{}, invalidf("payment %q amount must be positive, got %d", paymentID, amount)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[accountID]
	if !ok {
		return PaymentResult{}, ErrAccountNotFound
	}
	// 去重先于账单查找：相同标识改指向其他账单（哪怕不存在）也是冲突。
	if prev, ok := acc.payments[paymentID]; ok {
		if prev.period != period || prev.amount != amount {
			return PaymentResult{}, ErrPaymentConflict
		}
		rec, ok := acc.bills[period]
		if !ok {
			return PaymentResult{}, ErrBillNotFound
		}
		return PaymentResult{
			Registered:  false,
			Payment:     Payment{AccountID: accountID, PaymentID: paymentID, Period: period, Amount: amount},
			BillBalance: rec.bill.Balance,
			Settled:     rec.bill.Settled,
		}, nil
	}

	rec, ok := acc.bills[period]
	if !ok {
		return PaymentResult{}, ErrBillNotFound
	}

	if amount > rec.bill.Balance {
		return PaymentResult{}, ErrPaymentExceedsBalance
	}

	paid, ok := addNonNeg(rec.bill.Paid, amount)
	if !ok {
		return PaymentResult{}, ErrOverflow
	}
	rec.bill.Paid = paid
	rec.bill.Balance -= amount
	if rec.bill.Balance == 0 {
		rec.bill.Settled = true
	}
	acc.payments[paymentID] = paymentRecord{period: period, amount: amount}

	return PaymentResult{
		Registered:  true,
		Payment:     Payment{AccountID: accountID, PaymentID: paymentID, Period: period, Amount: amount},
		BillBalance: rec.bill.Balance,
		Settled:     rec.bill.Settled,
	}, nil
}

// MonthlyUsage 返回账户某账期的累计用量；无任何用量时返回 0。
func (s *Service) MonthlyUsage(accountID string, period Month) (Usage, error) {
	if accountID == "" {
		return Usage{}, invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return Usage{}, ErrAccountNotFound
	}
	// 与其他入口保持一致：顺带让已到期的换套餐安排生效。
	s.applyDueChangesLocked(acc, s.nowUTC())
	return Usage{Period: period, Total: acc.usage[period]}, nil
}

// Status 返回账户状态：各月累计用量、各账单余额以及当前是否因欠费停用。
// 等待取消时展示终止时刻；终止后无有效订阅、无当前套餐及待切换，历史用量与账单继续可查。
func (s *Service) Status(accountID string) (AccountStatus, error) {
	if accountID == "" {
		return AccountStatus{}, invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return AccountStatus{}, ErrAccountNotFound
	}

	now := s.nowUTC()
	// 查询即让已到期安排生效：月初零点之后无需上报用量或出账，当前条件立即更新。
	s.applyDueChangesLocked(acc, now)

	periodSet := make(map[Month]struct{}, len(acc.usage)+len(acc.bills))
	for m := range acc.usage {
		periodSet[m] = struct{}{}
	}
	for m := range acc.bills {
		periodSet[m] = struct{}{}
	}
	periods := make([]Month, 0, len(periodSet))
	for m := range periodSet {
		periods = append(periods, m)
	}
	sortMonths(periods)

	sub := acc.currentSub()
	st := AccountStatus{
		AccountID:    accountID,
		Subscribed:   sub != nil,
		Suspended:    s.isSuspendedLocked(acc, now),
		MonthlyUsage: make([]Usage, 0, len(periods)),
		Bills:        make([]BillSummary, 0, len(acc.bills)),
	}
	if sub != nil {
		st.CurrentTerms = sub.terms
		st.CancelAt = sub.cancelAt
		if sub.pending != nil {
			pending := *sub.pending
			st.PendingChange = &pending
		}
	}
	for _, m := range periods {
		st.MonthlyUsage = append(st.MonthlyUsage, Usage{Period: m, Total: acc.usage[m]})
	}
	for _, m := range periods {
		if rec, ok := acc.bills[m]; ok {
			b := rec.bill
			st.Bills = append(st.Bills, BillSummary{
				Period:   m,
				TotalDue: b.TotalDue,
				Paid:     b.Paid,
				Balance:  b.Balance,
				Settled:  b.Settled,
				DueAt:    b.DueAt,
			})
		}
	}
	return st, nil
}

// isSuspendedLocked 报告账户当前是否停用：
// 存在到达付款截止时刻仍有余额的账单。调用时持有 s.mu。
func (s *Service) isSuspendedLocked(acc *account, now time.Time) bool {
	for _, rec := range acc.bills {
		if !rec.bill.Settled && !now.Before(rec.bill.DueAt) {
			return true
		}
	}
	return false
}

func sortMonths(ms []Month) {
	sort.Slice(ms, func(i, j int) bool { return ms[i].Before(ms[j]) })
}

// addNonNeg 返回两个非负 int64 之和；溢出时 ok=false。
func addNonNeg(a, b int64) (int64, bool) {
	if a > maxInt64-b {
		return 0, false
	}
	return a + b, true
}

// mulNonNeg 返回两个非负 int64 之积；溢出时 ok=false。
func mulNonNeg(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > maxInt64/b {
		return 0, false
	}
	return a * b, true
}

// buildBill 依据套餐快照和账期总用量计算账单，所有金额运算做溢出检查。
func buildBill(accountID string, period Month, t PlanTerms, totalUsage int64) (Bill, error) {
	overageUnits := int64(0)
	if totalUsage > t.IncludedUnits {
		overageUnits = totalUsage - t.IncludedUnits
	}
	overageFee, ok := mulNonNeg(overageUnits, t.OveragePrice)
	if !ok {
		return Bill{}, ErrOverflow
	}
	subtotal, ok := addNonNeg(t.MonthlyFee, overageFee)
	if !ok {
		return Bill{}, ErrOverflow
	}
	tax, ok := rateAmount(subtotal, t.TaxRateBasisPoints)
	if !ok {
		return Bill{}, ErrOverflow
	}
	totalDue, ok := addNonNeg(subtotal, tax)
	if !ok {
		return Bill{}, ErrOverflow
	}
	b := Bill{
		AccountID:     accountID,
		Period:        period,
		Terms:         t,
		TotalUsage:    totalUsage,
		IncludedUnits: t.IncludedUnits,
		OverageUnits:  overageUnits,
		MonthlyFee:    t.MonthlyFee,
		OverageFee:    overageFee,
		Tax:           tax,
		TotalDue:      totalDue,
		Paid:          0,
		Balance:       totalDue,
		Settled:       totalDue == 0,
		DueAt:         period.End().AddDate(0, 0, 7),
	}
	return b, nil
}

// rateAmount 计算 amount*rate/10000 并四舍五入到分（非负数，半数向上）。
// 拆分商和余数计算以避免乘积溢出 int64。
func rateAmount(amount, rate int64) (int64, bool) {
	const den = 10000
	q, r := amount/den, amount%den
	qr, ok := mulNonNeg(q, rate)
	if !ok {
		return 0, false
	}
	rr, ok := mulNonNeg(r, rate)
	if !ok {
		return 0, false
	}
	// 四舍五入：(rr + den/2) / den，rr < den*10001 < 1e8，加 den/2 不溢出。
	add, ok := addNonNeg(qr, (rr+den/2)/den)
	if !ok {
		return 0, false
	}
	return add, true
}
