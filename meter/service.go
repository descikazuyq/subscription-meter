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
	activatedAt time.Time
	// terms 是本订阅段套餐条件的唯一事实来源，按生效月份升序保存，
	// 首条为开通当月适用的开通快照，之后每条对应一次已生效的换套餐。
	// 当前套餐标识、当前完整条件与任意历史账期的条件都统一由
	// termsForPeriod 从中取出，开通与换套餐生效时只需在这里追加一份快照。
	terms []termsEntry
	// pending 尚未生效的换套餐安排，每账户至多一条；nil 表示无安排。
	pending *PlanChange
	// cancelEnd 已安排的终止时刻（取消请求时刻的下一个 UTC 自然月月初）。
	// 非 nil 且尚未到达时为等待取消状态；到达后本订阅段终止并移入 history。
	cancelEnd *time.Time
	// endedAt 已终止订阅段的实际终止时刻；当前有效段为 nil。
	endedAt *time.Time
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
	// balance 与 settled 是本笔付款首次成功登记完成时账单的余额与付清状态。
	// 完全相同的重报原样返回该历史结果，不随后续付款或账单当前状态变化。
	balance int64
	settled bool
}

type account struct {
	id       string
	sub      *subRecord
	history  []*subRecord
	events   map[string]eventRecord
	usage    map[Month]int64
	bills    map[Month]*billRecord
	payments map[string]paymentRecord
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

// UpdatePlan 更新套餐当前条件。只影响之后新保存的条件快照：之后开通的
// 订阅与之后新接受的换套餐安排按修改后的定义取得条件。已开通订阅继续
// 使用开通时保存的快照，已接受的换套餐安排继续使用安排时锁定的快照，
// 均不被改写。
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
// activatedAt 可以晚于当前时刻，即提前登记一份尚未生效的订阅：登记会被保留，
// 但在实际开通时刻到达前 Status 的 Subscribed 为 false、CurrentTerms 为零值，
// 也没有待换套餐安排与待取消终止时刻；此期间再次开通仍返回
// ErrSubscriptionExists，安排换套餐、登记取消返回 ErrSubscriptionNotActivated。
// 判断以实际开通时刻为界（按同一瞬间比较，带时区的开通时间结果一致），不按
// 开通月份提前生效；到达开通时刻后查询即显示有效订阅与登记时保存的完整套餐
// 条件快照，等待期间修改套餐定义不改变该快照。
//
// 订阅终止（按月取消到达终止时刻）后可用本入口重新开通：activatedAt 不得早于
// 上一段订阅的终止时刻；重新开通保存当时的套餐条件，当月仍收完整月费、给完整
// 额度。重新开通不重置账户内事件与付款标识，也不清除旧欠费。同样允许登记未来
// 的重新开通时刻：旧订阅已结束、新订阅尚未开始的间隔中查询不显示任何生效套餐，
// 历史用量与账单继续可查。
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
	now := s.nowUTC()
	// 与其他入口保持一致：先让已到期的换套餐/取消落入历史。
	s.settleLocked(acc, now)
	plan, ok := s.plans[planID]
	if !ok {
		return ErrPlanNotFound
	}
	if acc.sub != nil {
		return ErrSubscriptionExists
	}
	activated := activatedAt.UTC()
	// 重新开通时刻不得早于上一段订阅的终止时刻。
	if n := len(acc.history); n > 0 {
		if activated.Before(*acc.history[n-1].endedAt) {
			return ErrResubscribeBeforeEnd
		}
	}
	acc.sub = &subRecord{
		activatedAt: activated,
		// 开通快照即条件时间线首条：当前条件与历史出账都从这里取。
		terms: []termsEntry{{
			effective: MonthOf(activated),
			terms:     termsOf(plan),
		}},
	}
	return nil
}

// nextMonth 返回 period 的下一个 UTC 自然月。
func nextMonth(period Month) Month {
	return MonthOf(period.End())
}

// settleLocked 让当前订阅段中已到生效时刻的换套餐安排落入条件历史，
// 并在到达取消终止时刻时把当前段标记终止、移入 acc.history。
// 到达月初零点（或终止时刻）即生效，无需先上报用量或生成账单；即使中间若干月
// 没有调用服务，再次进入时也会在此一次性补齐。调用时持有 s.mu。
//
// 开通时刻尚未到达的订阅段只是提前登记：它尚未生效，既没有已生效的套餐条件，
// 也不可能有待换套餐安排或待取消终止时刻，因此在到达开通时刻前不做任何处理，
// 尤其不能按开通月份让条件“提前生效”。
func (s *Service) settleLocked(acc *account, now time.Time) {
	sub := acc.sub
	if sub == nil || now.Before(sub.activatedAt) {
		return
	}
	current := MonthOf(now)
	for sub.pending != nil && !current.Before(sub.pending.EffectivePeriod) {
		// 换套餐生效只需向条件时间线追加一份锁定快照；
		// 当前套餐标识与当前条件都由该时间线统一导出，无需另行更新。
		sub.terms = append(sub.terms, termsEntry{
			effective: sub.pending.EffectivePeriod,
			terms:     sub.pending.Terms,
		})
		sub.pending = nil
	}
	if sub.cancelEnd != nil && !now.Before(*sub.cancelEnd) {
		end := *sub.cancelEnd
		sub.cancelEnd = nil
		sub.pending = nil
		sub.endedAt = &end
		acc.history = append(acc.history, sub)
		acc.sub = nil
	}
}

// coversInstant 报告订阅段是否覆盖时刻 at：半开区间 [activatedAt, end)，
// 开通时刻计入、终止时刻不计入。尚未生效的当前段（提前登记、开通时刻未到）
// 不覆盖任何时刻。订阅段的有效上界统一由 segmentEnd 给出：当前段已安排取消
// 时为登记的终止时刻（未到也一样，等待取消期间的事件必须落在终止之前），
// 已终止段为实际终止时刻，其余为无限远。所有时刻按同一瞬间比较，时区表达
// 不影响结果。
func coversInstant(sub *subRecord, at time.Time) bool {
	if at.Before(sub.activatedAt) {
		return false
	}
	if end, ended := segmentEnd(sub); ended && !at.Before(end) {
		return false
	}
	return true
}

// segmentEnd 返回订阅段的终止时刻（不含）：当前段已登记取消时是等待中的
// 终止时刻，已终止段是实际终止时刻；没有上界时 ended 为 false。
func segmentEnd(sub *subRecord) (time.Time, bool) {
	if sub.endedAt != nil {
		return *sub.endedAt, true
	}
	if sub.cancelEnd != nil {
		return *sub.cancelEnd, true
	}
	return time.Time{}, false
}

// coversMonth 报告订阅段是否让整个账期 period 成为可出账月份：
// 开通所在月计入（月中开通也按完整月出账），终止所在月不计入该段。
// 它是整月覆盖判断，不能由“月初那一刻是否被覆盖”代替——月中开通的月份
// 月初并不在订阅期间内，但整月仍归该订阅段出账。
func coversMonth(sub *subRecord, period Month) bool {
	if period.Before(MonthOf(sub.activatedAt)) {
		return false
	}
	if end, ended := segmentEnd(sub); ended && !period.Before(MonthOf(end)) {
		return false
	}
	return true
}

// termsForPeriod 在订阅段 sub 中返回指定账期当时适用的套餐条件快照。
// 条件时间线按生效月份升序，取不晚于 period 的最后一条；当前条件、
// 是否与当前套餐相同、历史账单出账全部经过这里，因此同一账期永远得到
// 同一份完整条件（月费、额度、超额单价、税率同源），不受之后的换套餐
// 或套餐定义修改影响。调用方须保证 sub.terms 非空（订阅段创建时即有首条）。
func termsForPeriod(sub *subRecord, period Month) PlanTerms {
	t := sub.terms[0].terms
	for _, e := range sub.terms {
		if period.Before(e.effective) {
			break
		}
		t = e.terms
	}
	return t
}

// segmentsLocked 按订阅发生的先后返回账户的全部订阅段（已终止的历史段，
// 以及当前段）。订阅归属的唯一事实来源就是这些订阅段：每段的开通时刻与
// 终止时刻由 settleLocked 维护，时刻与整月两种覆盖判断都只经 coversInstant
// 与 coversMonth 访问它们，用量接收与账单出账因此对同一账户的订阅期间和
// 空档保持一致的依据。调用时持有 s.mu。
func segmentsLocked(acc *account) []*subRecord {
	segs := make([]*subRecord, 0, len(acc.history)+1)
	segs = append(segs, acc.history...)
	if acc.sub != nil {
		segs = append(segs, acc.sub)
	}
	return segs
}

// segmentCoveringInstantLocked 返回覆盖时刻 at 的订阅段（coversInstant，
// 开通计入、终止不计入），没有任何段覆盖（空档、开通前、未来登记生效前）
// 时返回 nil。倒序检查使仍在延续的当前段优先命中。调用时持有 s.mu。
func segmentCoveringInstantLocked(acc *account, at time.Time) *subRecord {
	segs := segmentsLocked(acc)
	for i := len(segs) - 1; i >= 0; i-- {
		if coversInstant(segs[i], at) {
			return segs[i]
		}
	}
	return nil
}

// segmentCoveringMonthLocked 返回整月账期 period 出账应使用的订阅段
// （coversMonth：开通当月计入、终止当月不计入）。整月无任何段覆盖（早于
// 首段开通月，或处于两段之间的空档月）时返回 nil；月中重新开通的月份由
// 新订阅段覆盖，不沿用旧段条件。倒序检查使后来的段优先。调用时持有 s.mu。
func segmentCoveringMonthLocked(acc *account, period Month) *subRecord {
	segs := segmentsLocked(acc)
	for i := len(segs) - 1; i >= 0; i-- {
		if coversMonth(segs[i], period) {
			return segs[i]
		}
	}
	return nil
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
// 账户不存在、尚未开通订阅、开通时刻尚未到达、目标套餐不存在、目标套餐与当前
// 生效套餐相同时请求失败，原有安排保持不变。登记取消订阅会清除尚未生效的安排，
// 等待取消期间安排换套餐失败（ErrPlanChangeWhileCancelling）。
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
	// 先让已到期的安排/取消生效：例如跨月后的首次调用，此时“当前套餐”已是新套餐；
	// 恰在换套餐生效的月初登记取消，先承认该次切换，取消从再下一月生效。
	s.settleLocked(acc, now)
	if acc.sub == nil {
		return PlanChangeResult{}, ErrSubscriptionNotFound
	}
	if now.Before(acc.sub.activatedAt) {
		return PlanChangeResult{}, ErrSubscriptionNotActivated
	}
	if acc.sub.cancelEnd != nil {
		return PlanChangeResult{}, ErrPlanChangeWhileCancelling
	}

	plan, ok := s.plans[targetPlanID]
	if !ok {
		return PlanChangeResult{}, ErrPlanNotFound
	}
	// 与当前生效套餐的比较同样取当前账期适用的完整条件，
	// 保证“当前套餐”的判断与状态查询、出账同源。
	if targetPlanID == termsForPeriod(acc.sub, MonthOf(now)).PlanID {
		return PlanChangeResult{}, ErrPlanChangeSamePlan
	}

	// 已有安排且目标相同：原样返回，不重新取价。
	if acc.sub.pending != nil && acc.sub.pending.TargetPlanID == targetPlanID {
		return PlanChangeResult{Created: false, Change: *acc.sub.pending}, nil
	}

	change := PlanChange{
		TargetPlanID:    targetPlanID,
		Terms:           termsOf(plan),
		EffectivePeriod: nextMonth(MonthOf(now)),
	}
	acc.sub.pending = &change
	return PlanChangeResult{Created: true, Change: change}, nil
}

// CancelPlanChange 在安排生效前取消它，取消后账户继续沿用当前套餐。
// 没有待生效安排时取消也成功；已生效的切换不会被撤回。
// 欠费停用期间也可以取消，取消不清除欠费、不解除停用。
// 账户不存在、订阅已终止（含已到终止时刻）时失败。
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
	// 已到生效月份月初零点的安排视为已生效，不可撤回；已终止订阅段无当前订阅。
	s.settleLocked(acc, s.nowUTC())
	if acc.sub == nil {
		return ErrSubscriptionNotFound
	}
	acc.sub.pending = nil
	return nil
}

// CancelSubscription 为当前有效订阅登记按月取消。
//
// 仅当订阅已生效（当前时刻不早于开通时刻）时可取消；欠费停用不妨碍取消，也不
// 免除债务。终止时刻为请求时刻的下一个 UTC 自然月月初零点（UTC）：当月照常
// 接收用量并按完整月费与额度计费，不按天退款。重复取消始终返回原终止时刻，
// 不把期限后移（Cancelled 为 false）。
//
// 成功时清除尚未生效的换套餐安排；若请求恰在某次换套餐生效的月初，先承认该次
// 切换，取消从再下一月生效。等待取消期间拒绝新的换套餐安排。
//
// 账户不存在、从未开通或当前无有效订阅、开通时刻尚未到达时请求失败。
func (s *Service) CancelSubscription(accountID string) (CancelSubscriptionResult, error) {
	if accountID == "" {
		return CancelSubscriptionResult{}, invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[accountID]
	if !ok {
		return CancelSubscriptionResult{}, ErrAccountNotFound
	}
	now := s.nowUTC()
	// 先补齐到期状态：恰在月初时先承认到期换套餐，终止时刻取再下一月。
	s.settleLocked(acc, now)
	if acc.sub == nil {
		return CancelSubscriptionResult{}, ErrSubscriptionNotFound
	}
	if now.Before(acc.sub.activatedAt) {
		return CancelSubscriptionResult{}, ErrSubscriptionNotActivated
	}

	// 重复取消：原样返回原终止时刻，不后移期限。
	if acc.sub.cancelEnd != nil {
		return CancelSubscriptionResult{
			Cancelled:    false,
			Cancellation: SubscriptionCancellation{EndAt: *acc.sub.cancelEnd},
		}, nil
	}

	// 登记取消清除尚未生效的换套餐安排（撤回取消也不恢复它）。
	acc.sub.pending = nil
	end := nextMonth(MonthOf(now)).Start()
	acc.sub.cancelEnd = &end
	return CancelSubscriptionResult{
		Cancelled:    true,
		Cancellation: SubscriptionCancellation{EndAt: end},
	}, nil
}

// UndoCancelSubscription 在取消生效前撤回按月取消安排，撤回后订阅继续有效。
// 有效订阅没有取消安排时撤回也成功；撤回不恢复登记取消时被清除的换套餐安排。
// 终止时刻已到（订阅已终止）后撤回失败（ErrSubscriptionNotFound）；
// 账户不存在或从未开通时同样失败。
func (s *Service) UndoCancelSubscription(accountID string) error {
	if accountID == "" {
		return invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.accounts[accountID]
	if !ok {
		return ErrAccountNotFound
	}
	// 到达终止时刻后订阅已终止：撤回失败。
	s.settleLocked(acc, s.nowUTC())
	if acc.sub == nil {
		return ErrSubscriptionNotFound
	}
	acc.sub.cancelEnd = nil
	return nil
}

// RecordEvent 上报一条用量事件。
//
// 事件按发生时刻归入 UTC 自然月账期；事件发生时刻必须属于某段实际订阅期间
// （开通时刻计入、终止时刻不计入），空档期间与晚于当前时刻的事件被拒绝。
// 订阅终止后仍可补报旧订阅期间且尚未出账的用量。
// 事件标识在同一账户内去重：完全相同的重报返回原结果且不重复累计——
// 即使发生在取消、重新开通之后也成功；相同标识但时刻或数量不同返回
// ErrEventConflict。
// 已出账月份拒绝新事件；账户因到期欠费停用时拒绝新增事件；
// 但此前已接收事件的完全相同重报始终成功。
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

	now := s.nowUTC()
	// 进入即补齐已到期的换套餐/取消，保证并发下各操作观察到一致的分段状态。
	s.settleLocked(acc, now)

	// 从未开通订阅的账户没有任何订阅段。
	if acc.sub == nil && len(acc.history) == 0 {
		return EventResult{}, ErrSubscriptionNotFound
	}

	// 去重优先：完全相同的重报即使发生在出账后、停用期间、取消或重新开通
	// 之后也返回成功。
	if prev, ok := acc.events[e.EventID]; ok {
		if !prev.at.Equal(at) || prev.qty != e.Quantity {
			return EventResult{}, ErrEventConflict
		}
		return EventResult{Accepted: false, Period: prev.period}, nil
	}

	if at.After(now) {
		return EventResult{}, ErrEventInFuture
	}
	// 事件发生时刻必须落在某段实际订阅期间：[activatedAt, end)。
	// 与出账同源于 coversInstant/coversMonth，但这里按实际瞬间判定：
	// 月中重新开通当月虽可整月出账，开通前的事件仍被拒绝。
	if segmentCoveringInstantLocked(acc, at) == nil {
		return EventResult{}, ErrEventBeforeSubscription
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
// 只能为某段订阅实际覆盖的、且已经结束的月份生成账单：开通当月计入、终止
// 当月（终止时刻所在月）不计入该段，完全无订阅的空档月份出账失败。
// 取消订阅不自动出账；终止后旧订阅覆盖的已结束月份仍可出账，使用各月当时
// 锁定的条件。重复调用得到同一账单；出账后金额与用量明细固定。
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
	if existing, ok := acc.bills[period]; ok {
		return existing.bill, nil
	}

	now := s.nowUTC()
	// 补齐到期换套餐/取消（条件历史与分段），随后按账期取覆盖该月的订阅段中
	// 当时适用的条件：即使已重新开通或先出新月份账单再补旧月份账单，历史账单
	// 也不会被后来的订阅覆盖。
	s.settleLocked(acc, now)

	if acc.sub == nil && len(acc.history) == 0 {
		return Bill{}, ErrSubscriptionNotFound
	}
	if period.End().After(now) {
		return Bill{}, ErrBillMonthNotEnded
	}

	sub := segmentCoveringMonthLocked(acc, period)
	if sub == nil {
		// 早于首段开通，或处于两段订阅之间的空档。
		return Bill{}, ErrBillBeforeSubscription
	}

	terms := termsForPeriod(sub, period)
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

// EstimateCurrentBill 预估当前账期（查询时刻所在的 UTC 自然月）截至查询时刻的
// 应付金额：订阅已实际生效的账户在月份尚未结束时即可查看，不必先出账。
// 只计算已接收的用量；没有任何用量时也可查询，累计与超额用量为零，
// 月费与税额照常计算。
//
// 计价规则与正式出账相同：使用订阅保存的当月套餐条件快照，收取完整月费并
// 提供完整额度（月中开通不按天折算），超额部分按快照单价计算，税额以月费与
// 超额费用之和为基数按万分比四舍五入到分。尚未生效的换套餐安排不混入本月
// 预估；到达安排生效月月初后，查询直接使用安排接受时锁定的条件，无需先上报
// 用量。已登记按月取消但尚未到终止时刻的账户当月仍完整计费；因欠费停用但
// 订阅仍生效的账户也可查询，查询本身不解除停用。
//
// 查询不产生任何副作用：不保存正式账单、不关闭当月、不产生欠费，也不改变
// 已有账单的金额与付款状态；查询之后同月合法的新用量仍按原规则接收，再次
// 查询反映新增累计量。正式出账仍只处理已经结束的月份。
//
// 超额费用、税前合计或预计应付金额超出非负 int64 范围时返回 ErrOverflow，
// 不返回部分金额。账户不存在返回 ErrAccountNotFound；从未开通或订阅已终止
// 返回 ErrSubscriptionNotFound；提前登记但开通时刻尚未到达返回
// ErrSubscriptionNotActivated；这些情况下不输出预估。
func (s *Service) EstimateCurrentBill(accountID string) (BillEstimate, error) {
	if accountID == "" {
		return BillEstimate{}, invalidf("account id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	acc, ok := s.accounts[accountID]
	if !ok {
		return BillEstimate{}, ErrAccountNotFound
	}
	now := s.nowUTC()
	// 与其他入口保持一致：先让已到期的换套餐/取消落入历史。到达换套餐
	// 生效月月初后，这里即把安排锁定的条件追加进条件时间线，下面的预估
	// 因此直接使用锁定快照，无需先上报用量或出账。
	s.settleLocked(acc, now)
	if acc.sub == nil {
		// 从未开通，或订阅已到达终止时刻（段已移入历史）。
		return BillEstimate{}, ErrSubscriptionNotFound
	}
	if now.Before(acc.sub.activatedAt) {
		// 提前登记、开通时刻尚未到达：不输出预估。
		return BillEstimate{}, ErrSubscriptionNotActivated
	}

	period := MonthOf(now)
	terms := termsForPeriod(acc.sub, period)
	usage := acc.usage[period]
	overageUnits, overageFee, tax, totalDue, err := chargeAmounts(terms, usage)
	if err != nil {
		return BillEstimate{}, err
	}
	return BillEstimate{
		AccountID:     accountID,
		Period:        period,
		Estimated:     true,
		Terms:         terms,
		TotalUsage:    usage,
		IncludedUnits: terms.IncludedUnits,
		OverageUnits:  overageUnits,
		MonthlyFee:    terms.MonthlyFee,
		OverageFee:    overageFee,
		Tax:           tax,
		TotalDue:      totalDue,
	}, nil
}

// RecordPayment 为指定账单登记一笔大于零且不超过余额的本地付款，支持分次登记。
// 付款标识在同一账户内去重：相同账单和金额的重报仍成功，但本次不再登记
// （Registered 为 false），也不再次增加已付金额；返回的付款内容、账单余额与
// 付清状态均为该笔付款首次成功登记完成时的历史结果，不随后续付款或账单当前
// 状态变化（例如账单后来已结清，重报早先的部分付款仍返回当时的未清余额与
// 未付清）。账单查询与账户状态中的余额始终反映当前状态，不受重报影响。
// 相同标识但账期或金额不同返回 ErrPaymentConflict：即使原账单已结清，或改指
// 向的账期还没有账单，也按冲突处理而非当作新付款。失败不消耗付款标识。
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
		// 完全相同的重报仍成功，但不再次登记、不改变账单：
		// 返回这笔付款首次成功登记完成时的余额与付清状态，
		// 不受账单后续收款、当前余额或停用/恢复状态影响。
		return PaymentResult{
			Registered:  false,
			Payment:     Payment{AccountID: accountID, PaymentID: paymentID, Period: period, Amount: amount},
			BillBalance: prev.balance,
			Settled:     prev.settled,
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
	// 保存首次登记完成时的历史结果，供完全相同重报原样返回。
	acc.payments[paymentID] = paymentRecord{
		period:  period,
		amount:  amount,
		balance: rec.bill.Balance,
		settled: rec.bill.Settled,
	}

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
	// 与其他入口保持一致：顺带让已到期的换套餐/取消生效。
	s.settleLocked(acc, s.nowUTC())
	return Usage{Period: period, Total: acc.usage[period]}, nil
}

// Status 返回账户状态：各月累计用量、各账单余额以及当前是否因欠费停用。
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
	// 查询即让已到期安排/取消生效：月初零点或终止时刻之后无需上报用量或出账，
	// 当前条件与订阅状态立即更新。
	s.settleLocked(acc, now)

	// 订阅是否生效以实际开通时刻为界：提前登记、开通时刻尚未到达的订阅仍
	// 保存在 acc.sub 中（再次开通、安排换套餐、登记取消均按已有订阅的规则
	// 处理），但查询时刻不显示为有效订阅。到达开通时刻即生效，不按开通月份
	// 提前生效；时刻按同一瞬间比较，使用带时区的时间结果一致。
	active := acc.sub != nil && !now.Before(acc.sub.activatedAt)

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

	st := AccountStatus{
		AccountID:    accountID,
		Subscribed:   active,
		Suspended:    s.isSuspendedLocked(acc, now),
		MonthlyUsage: make([]Usage, 0, len(periods)),
		Bills:        make([]BillSummary, 0, len(acc.bills)),
	}
	// 仅实际生效的订阅才展示当前套餐条件、待换套餐安排与已安排终止时刻；
	// 等待开通期间这些字段均为零值（查询不删除也不提前激活已登记订阅）。
	if active {
		// 当前条件与历史出账取自同一份条件时间线：月初零点 settleLocked
		// 追加新快照后，这里直接拿到安排时锁定的完整条件，无需另行同步。
		st.CurrentTerms = termsForPeriod(acc.sub, MonthOf(now))
		if acc.sub.pending != nil {
			pending := *acc.sub.pending
			st.PendingChange = &pending
		}
		if acc.sub.cancelEnd != nil {
			end := *acc.sub.cancelEnd
			st.ScheduledEnd = &end
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

// chargeAmounts 按套餐条件快照与账期累计用量计算超额用量与各项金额，
// 所有金额运算做溢出检查。正式出账与当前账期预估共用这一份计价规则：
// 完整月费、完整额度、超额按快照单价、税额以月费与超额费用之和为基数
// 按万分比四舍五入到分。
func chargeAmounts(t PlanTerms, totalUsage int64) (overageUnits, overageFee, tax, totalDue int64, err error) {
	if totalUsage > t.IncludedUnits {
		overageUnits = totalUsage - t.IncludedUnits
	}
	var ok bool
	if overageFee, ok = mulNonNeg(overageUnits, t.OveragePrice); !ok {
		return 0, 0, 0, 0, ErrOverflow
	}
	subtotal, ok := addNonNeg(t.MonthlyFee, overageFee)
	if !ok {
		return 0, 0, 0, 0, ErrOverflow
	}
	if tax, ok = rateAmount(subtotal, t.TaxRateBasisPoints); !ok {
		return 0, 0, 0, 0, ErrOverflow
	}
	if totalDue, ok = addNonNeg(subtotal, tax); !ok {
		return 0, 0, 0, 0, ErrOverflow
	}
	return overageUnits, overageFee, tax, totalDue, nil
}

// buildBill 依据套餐快照和账期总用量计算账单，所有金额运算做溢出检查。
func buildBill(accountID string, period Month, t PlanTerms, totalUsage int64) (Bill, error) {
	overageUnits, overageFee, tax, totalDue, err := chargeAmounts(t, totalUsage)
	if err != nil {
		return Bill{}, err
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
