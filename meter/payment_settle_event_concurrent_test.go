package meter

import (
	"errors"
	"sync"
	"testing"
)

// 本文件为“欠费停用后的用量接收”补充回归保障，重点保护结清最后一笔到期
// 欠款的付款与一条新用量上报同时发生时的裁决。
//
// 既有规则：是否接收一条新事件，只取决于处理该事件那一刻账户的欠费状态；
// 付款不会自动替用户补录刚被拒绝的事件，被拒收的事件也不消耗事件标识。
// 付款只改变账单的已付金额与余额：账单金额、用量明细与截止时刻出账即固定，
// 订阅是否有效、适用什么套餐条件都不因付款改变。
//
// 场景（账户只有一张已到期的旧账单，没有其他欠款）：
//   - 账户自 2026-01-01 00:00 UTC 起持续订阅套餐 plan1000（月费 1000 分，
//     超额单价与税率均为零）。
//   - 2026-01 账单应付 1000 分，截止时刻 2026-02-08 00:00 UTC；此前已登记
//     400 分，仍欠 600 分。时钟停在截止时刻本身（账单已到期，账户欠费停用，
//     但订阅仍有效）。
//   - 当前账期 2026-02 尚未出账、累计用量为 0。
//   - 同时发生两项合法请求：用新付款标识登记剩余 600 分；用此前未接收过的
//     事件标识上报发生在 2026-02-07（有效订阅期间、不晚于当前时刻）的 3 单位。
//
// 两项请求由同一把互斥锁串行裁决，只允许与某一种先后顺序一致的结果：
//   - 事件先于付款处理：上报返回 ErrSuspended，二月累计仍为 0；随后付款成功
//     也不能把这次拒绝改成已接收。
//   - 付款先于事件处理：上报成功，Accepted=true，归入 2026-02，累计变为 3。
// 不允许“上报失败却累计增加”或“上报成功却没有累计”的撕裂结果。
//
// 无论哪种顺序，最后那笔付款都首次登记成功并返回余额 0、已付清；旧账单
// 已付 1000、余额 0，应付金额与截止时刻保持不变；账户不再欠费停用，订阅
// 条件不变。此后原样再提交一次该事件：首次被拒的路径本次按首次接收处理
// （Accepted=true），首次已成功的路径则是重报（Accepted=false）；两条路径
// 此时累计都恰为 3，不消耗被拒绝事件的标识，也不重复计量。

const (
	settleRacyAcct    = "acct-final-payment-race"
	settleRacyPlanID  = "plan1000"
	settleRacyPartial = "pay-partial-400"
	settleRacyFinal   = "pay-final-600"
	settleRacyEventID = "evt-feb-unseen"
)

var (
	settleRacySubscribeAt = utc(2026, 1, 1, 0, 0)
	settleRacyDueAt       = utc(2026, 2, 8, 0, 0)
	settleRacyEventAt     = utc(2026, 2, 7, 12, 0)
)

// settleRacyEvent 是与结清付款并发上报的那条二月新用量（3 单位）。
func settleRacyEvent() Event {
	return Event{
		AccountID: settleRacyAcct,
		EventID:   settleRacyEventID,
		At:        settleRacyEventAt,
		Quantity:  3,
	}
}

// newSettleRacyFixture 建立题述起点：一月账单 1000 分已到期、已付 400、
// 欠 600，账户欠费停用但订阅持续有效；二月未出账、无用量。
func newSettleRacyFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(settleRacySubscribeAt)
	mustPlan(t, s, Plan{ID: settleRacyPlanID, MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, settleRacyAcct)
	if err := s.Subscribe(settleRacyAcct, settleRacyPlanID, settleRacySubscribeAt); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	clk.t = settleRacyDueAt
	b, err := s.CreateBill(settleRacyAcct, jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	if b.TotalDue != 1000 || b.Paid != 0 || b.Balance != 1000 || b.Settled {
		t.Fatalf("jan bill initial state: %+v", b)
	}
	if !b.DueAt.Equal(settleRacyDueAt) {
		t.Fatalf("jan bill due at = %v, want %v", b.DueAt, settleRacyDueAt)
	}

	// 此前已登记 400 分：账单仍欠 600、未付清。
	pr, err := s.RecordPayment(settleRacyAcct, settleRacyPartial, jan(2026), 400)
	if err != nil || !pr.Registered || pr.BillBalance != 600 || pr.Settled {
		t.Fatalf("partial payment: r=%+v err=%v", pr, err)
	}

	// 起点校验：到期欠费停用；订阅仍有效且套餐条件不变；二月无用量、未出账。
	wantSettleRacyState(t, s, true)
	wantSettleRacyBill(t, s, 400, 600, false)
	wantSettleRacyFebUsage(t, s, 0)
	if _, err := s.GetBill(settleRacyAcct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("feb bill should not exist at start: %v", err)
	}
	return s, clk
}

// wantSettleRacyState 校验停用标志与订阅状态彼此独立：付款只解除欠费停用，
// 订阅始终有效、当前套餐条件保持开通快照不变。
func wantSettleRacyState(t *testing.T, s *Service, suspended bool) {
	t.Helper()
	st, err := s.Status(settleRacyAcct)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Suspended != suspended {
		t.Fatalf("suspended = %v, want %v; bills=%+v", st.Suspended, suspended, st.Bills)
	}
	if !st.Subscribed {
		t.Fatalf("subscription must remain active regardless of payment: %+v", st)
	}
	if st.CurrentTerms.PlanID != settleRacyPlanID || st.CurrentTerms.MonthlyFee != 1000 {
		t.Fatalf("current terms changed after payment: %+v", st.CurrentTerms)
	}
}

// wantSettleRacyBill 校验一月账单的付款状态，以及出账时即固定的应付金额、
// 用量明细与截止时刻不被付款或上报改写。
func wantSettleRacyBill(t *testing.T, s *Service, paid, balance int64, settled bool) {
	t.Helper()
	b, err := s.GetBill(settleRacyAcct, jan(2026))
	if err != nil {
		t.Fatalf("get jan bill: %v", err)
	}
	if b.TotalDue != 1000 || b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("jan bill: due=%d paid=%d balance=%d settled=%v, want paid=%d balance=%d settled=%v",
			b.TotalDue, b.Paid, b.Balance, b.Settled, paid, balance, settled)
	}
	if b.MonthlyFee != 1000 || b.OverageFee != 0 || b.Tax != 0 || b.TotalUsage != 0 {
		t.Fatalf("jan bill fixed detail changed: %+v", b)
	}
	if !b.DueAt.Equal(settleRacyDueAt) {
		t.Fatalf("jan bill due at = %v, want %v", b.DueAt, settleRacyDueAt)
	}
}

// wantSettleRacyFebUsage 校验当前账期（二月）的累计用量。
func wantSettleRacyFebUsage(t *testing.T, s *Service, total int64) {
	t.Helper()
	u, err := s.MonthlyUsage(settleRacyAcct, feb(2026))
	if err != nil {
		t.Fatalf("monthly usage: %v", err)
	}
	if u.Period != feb(2026) || u.Total != total {
		t.Fatalf("feb usage = %+v, want total %d", u, total)
	}
}

// wantSettleRacyFinalPayment 校验结清欠款的最后一笔付款：首次登记成功、
// 余额 0、已付清；原样重报则不再登记（Registered=false），历史结果仍是
// 余额 0、已付清，也不再次增加已付金额。
func wantSettleRacyFinalPayment(t *testing.T, s *Service, first bool) {
	t.Helper()
	r, err := s.RecordPayment(settleRacyAcct, settleRacyFinal, jan(2026), 600)
	if err != nil {
		t.Fatalf("final payment: %v", err)
	}
	if r.Registered != first || r.BillBalance != 0 || !r.Settled ||
		r.Payment.PaymentID != settleRacyFinal || r.Payment.Amount != 600 {
		t.Fatalf("final payment first=%v: r=%+v", first, r)
	}
}

// TestEventBeforeFinalPaymentRejectedThenAcceptedAfterSettle 锁定顺序一：
// 新用量在付款结清前被处理，必须返回 ErrSuspended 且不累计；付款随后成功
// 也不能把拒绝改成接收。结清后原样再报，作为首次接收成功；再报则是重报。
func TestEventBeforeFinalPaymentRejectedThenAcceptedAfterSettle(t *testing.T) {
	s, clk := newSettleRacyFixture(t)
	ev := settleRacyEvent()

	// 事件先处理：账户仍欠费停用。
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrSuspended) {
		t.Fatalf("event before payment must be ErrSuspended: %v", err)
	}
	// 拒收不累计、不改变停用状态、不产生二月账单。
	wantSettleRacyFebUsage(t, s, 0)
	wantSettleRacyState(t, s, true)
	wantSettleRacyBill(t, s, 400, 600, false)

	// 付款随后成功：不能补录刚才被拒的事件。
	wantSettleRacyFinalPayment(t, s, true)
	wantSettleRacyFebUsage(t, s, 0)
	wantSettleRacyState(t, s, false)
	wantSettleRacyBill(t, s, 1000, 0, true)

	// 拒收没有消耗事件标识：原样再报按首次接收处理。
	r, err := s.RecordEvent(ev)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("event resubmitted after settle must be first accepted: r=%+v err=%v", r, err)
	}
	wantSettleRacyFebUsage(t, s, 3)

	// 再原样提交：命中接收记录的重报，不重复计量。
	if r, err := s.RecordEvent(ev); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("identical replay must deduplicate: r=%+v err=%v", r, err)
	}
	wantSettleRacyFebUsage(t, s, 3)

	// 付款重报仍返回首次登记的历史结果，不再扣款；账单与订阅保持终态。
	wantSettleRacyFinalPayment(t, s, false)
	wantSettleRacyBill(t, s, 1000, 0, true)
	wantSettleRacyState(t, s, false)
	if _, err := s.GetBill(settleRacyAcct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("feb bill must never be auto-created: %v", err)
	}
	if !clk.t.Equal(settleRacyDueAt) {
		t.Fatalf("clock moved unexpectedly: %v", clk.t)
	}
}

// TestFinalPaymentBeforeEventAcceptedThenReplayDeduplicated 锁定顺序二：
// 付款先完成、停用解除，随后上报必须首次接收成功（Accepted=true，归入
// 2026-02，累计 3）；原样再报则是重报（Accepted=false），累计不变。
func TestFinalPaymentBeforeEventAcceptedThenReplayDeduplicated(t *testing.T) {
	s, clk := newSettleRacyFixture(t)
	ev := settleRacyEvent()

	// 付款先处理：首次登记成功并结清。
	wantSettleRacyFinalPayment(t, s, true)
	wantSettleRacyBill(t, s, 1000, 0, true)
	wantSettleRacyState(t, s, false)

	// 事件后处理：停用已解除，作为首次上报接收。
	r, err := s.RecordEvent(ev)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("event after payment must be first accepted: r=%+v err=%v", r, err)
	}
	wantSettleRacyFebUsage(t, s, 3)

	// 原样再报：重报，不重复计量。
	if r, err := s.RecordEvent(ev); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("identical replay must deduplicate: r=%+v err=%v", r, err)
	}
	wantSettleRacyFebUsage(t, s, 3)

	// 付款重报返回历史结果，不改账单；终态与顺序一完全一致。
	wantSettleRacyFinalPayment(t, s, false)
	wantSettleRacyBill(t, s, 1000, 0, true)
	wantSettleRacyState(t, s, false)
	if _, err := s.GetBill(settleRacyAcct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("feb bill must never be auto-created: %v", err)
	}
	if !clk.t.Equal(settleRacyDueAt) {
		t.Fatalf("clock moved unexpectedly: %v", clk.t)
	}
}

// TestFinalPaymentAndNewEventConcurrent 并发版本：最后一笔结清付款与一条新
// 用量在同一瞬间到达。实现把两项操作各自的“判定 → 落库”都放在同一把互斥
// 锁的一个临界区内，因此每一轮的结果必须与某一种确定顺序完全一致：
//   - 事件赢：ErrSuspended 且二月累计为 0；
//   - 付款赢：Accepted=true 且二月累计为 3。
//
// 不允许撕裂结果（失败却累计、成功却没累计）。并发结束后再按实际走向验证：
// 被拒收的事件可在结清后以原标识首次接收；已接收的重报只幂等返回；两条
// 路径最终累计都恰为 3。
func TestFinalPaymentAndNewEventConcurrent(t *testing.T) {
	const rounds = 200
	for iter := 0; iter < rounds; iter++ {
		s, _ := newSettleRacyFixture(t)

		type eventOutcome struct {
			r   EventResult
			err error
		}
		payCh := make(chan PaymentResult, 1)
		payErrCh := make(chan error, 1)
		evtCh := make(chan eventOutcome, 1)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r, err := s.RecordPayment(settleRacyAcct, settleRacyFinal, jan(2026), 600)
			payCh <- r
			payErrCh <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			r, err := s.RecordEvent(settleRacyEvent())
			evtCh <- eventOutcome{r, err}
		}()
		close(start)
		wg.Wait()
		pay := <-payCh
		payErr := <-payErrCh
		eo := <-evtCh

		// 付款无论先后都首次登记成功：金额恰好等于结清前余额，必然结清。
		if payErr != nil || !pay.Registered || pay.BillBalance != 0 || !pay.Settled {
			t.Fatalf("iter %d final payment: r=%+v err=%v", iter, pay, payErr)
		}

		// 事件结果只能是两种顺序之一：被停用拒绝（未累计）或首次接收（已累计）。
		eventRejected := errors.Is(eo.err, ErrSuspended)
		eventAccepted := eo.err == nil && eo.r.Accepted && eo.r.Period == feb(2026)
		if eventRejected == eventAccepted {
			t.Fatalf("iter %d event outcome must match exactly one order: r=%+v err=%v", iter, eo.r, eo.err)
		}

		// 结果与累计必须自洽：拒绝即 0、接收即 3，杜绝撕裂。
		if eventRejected {
			wantSettleRacyFebUsage(t, s, 0)
		} else {
			wantSettleRacyFebUsage(t, s, 3)
		}

		// 并发结束后的共同终态：账单已结清、账户不再停用、订阅条件不变、
		// 二月仍未出账。
		wantSettleRacyBill(t, s, 1000, 0, true)
		wantSettleRacyState(t, s, false)
		if _, err := s.GetBill(settleRacyAcct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("iter %d feb bill must not exist: %v", iter, err)
		}

		ev := settleRacyEvent()
		if eventRejected {
			// 付款不会补录被拒事件；拒收也不占标识：此刻原样再报必须首次接收。
			r, err := s.RecordEvent(ev)
			if err != nil || !r.Accepted || r.Period != feb(2026) {
				t.Fatalf("iter %d rejected event must accept after settle: r=%+v err=%v", iter, r, err)
			}
			wantSettleRacyFebUsage(t, s, 3)
		}

		// 无论哪条路径，此刻原样再报都应是重报，累计恰为 3，不重复计量。
		r, err := s.RecordEvent(ev)
		if err != nil || r.Accepted || r.Period != feb(2026) {
			t.Fatalf("iter %d final replay must deduplicate: r=%+v err=%v", iter, r, err)
		}
		wantSettleRacyFebUsage(t, s, 3)

		// 付款重报仍是首次登记时的历史结果（余额 0、已付清），不再登记。
		wantSettleRacyFinalPayment(t, s, false)
		wantSettleRacyBill(t, s, 1000, 0, true)
		wantSettleRacyState(t, s, false)
	}
}
