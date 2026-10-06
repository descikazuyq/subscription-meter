package meter

import (
	"errors"
	"sync"
	"testing"
)

// 本文件回归“结清最后一笔到期欠款”与“上报一条新用量”同时发生时的用量接收
// 结果，防止日后把停用判定、扣款与用量累计拆成多个不同步的步骤而破坏以下
// 不变量：
//
// 规则是按处理新事件那一刻的欠费情况决定是否接收：付款不会自动替用户补录
// 刚被拒绝的事件。因此同一账户上一笔恰好结清到期账单的付款与一条当前账期
// 的新事件并发到达时，终态只能与两种串行顺序之一一致：
//
//   - 用量先于付款裁决：上报返回 ErrSuspended，当前账期累计仍为 0；随后付款
//     成功结清也不能把这次拒绝改写成已接收（不能出现上报失败却累计增加）。
//   - 付款先于用量裁决：上报成功，Accepted 为 true、返回当前账期，累计变为
//     事件数量（不能出现上报成功却没有累计）。
//
// 无论用量走哪条路径：
//
//   - 最后一笔付款都必须是首次登记成功，返回账单余额 0 与已付清；旧账单的
//     应付金额与截止时刻保持出账时的值，付款只改变已付金额与余额。
//   - 结清后账户不再欠费停用，但订阅条件（开通时的套餐快照）不因付款改变。
//   - 原样再次提交该事件：首次被拒的路径本次按首次接收（Accepted=true），
//     首次已成功的路径按重报幂等返回（Accepted=false）；两条路径此后累计都
//     恰好等于事件数量，被拒收不消耗事件标识，成功接收不重复计量。
//
// 时间线（全部 UTC）：
//
//	2026-01-01 00:00 开通套餐 plan1000（月费 1000，无超额、无税），订阅持续
//	           有效：不取消、不换套餐。
//	2026-02-10 00:00 为已结束的 2026-01 出账：应付 1000，截止时刻固定为
//	           2026-02-08 00:00（已到期）。先用付款标识 pay-partial 登记
//	           400 分，账单仍欠 600 分，账户欠费停用；2026-02 尚未出账、
//	           累计用量为 0，账户没有其他欠款。
//	同一时刻并发到达两项合法请求：
//	   - 用新付款标识 pay-rest 登记剩余 600 分；
//	   - 用此前未接收的标识 evt-feb-3 上报发生在当前时刻、数量 3 的事件，
//	     归属于尚未出账的 2026-02。

const (
	finalPayAcct  = "acct-final-payment"
	finalPayID    = "pay-rest"
	partialPayID  = "pay-partial"
	finalEventID  = "evt-feb-3"
	finalBillDue  = 1000
	finalPaidPart = 400
	finalPayRest  = 600
	finalEventQty = 3
)

var (
	finalPayNow   = utc(2026, 2, 10, 0, 0)
	finalDueAt    = utc(2026, 2, 8, 0, 0)
	finalSubStart = utc(2026, 1, 1, 0, 0)
)

// suspendedFinalPaymentFixture 搭好题述起点：订阅持续有效，一月旧账单应付
// 1000、已登记 400、仍欠 600 且已过截止时刻，账户欠费停用；二月未出账、
// 累计用量为 0。结束时时钟停在 2026-02-10 00:00。
func suspendedFinalPaymentFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(finalSubStart)
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, finalPayAcct)
	if err := s.Subscribe(finalPayAcct, "plan1000", finalSubStart); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	clk.t = finalPayNow
	bill, err := s.CreateBill(finalPayAcct, jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	if bill.TotalDue != finalBillDue || bill.Paid != 0 || bill.Balance != finalBillDue || bill.Settled {
		t.Fatalf("jan bill initial state: %+v", bill)
	}
	if !bill.DueAt.Equal(finalDueAt) {
		t.Fatalf("jan bill due at = %v, want %v", bill.DueAt, finalDueAt)
	}

	partial, err := s.RecordPayment(finalPayAcct, partialPayID, jan(2026), finalPaidPart)
	if err != nil || !partial.Registered || partial.BillBalance != finalPayRest || partial.Settled {
		t.Fatalf("partial payment 400: r=%+v err=%v", partial, err)
	}

	// 起点：欠费停用但订阅仍有效，套餐条件保持开通快照；二月无账无用量。
	st, err := s.Status(finalPayAcct)
	if err != nil {
		t.Fatalf("status at fixture: %v", err)
	}
	if !st.Subscribed || !st.Suspended {
		t.Fatalf("fixture flags: subscribed=%v suspended=%v, want true/true", st.Subscribed, st.Suspended)
	}
	if st.CurrentTerms != termsOf(Plan{ID: "plan1000", MonthlyFee: 1000}) {
		t.Fatalf("fixture terms changed: %+v", st.CurrentTerms)
	}
	if u, _ := s.MonthlyUsage(finalPayAcct, feb(2026)); u.Total != 0 {
		t.Fatalf("fixture feb usage = %d, want 0", u.Total)
	}
	if _, err := s.GetBill(finalPayAcct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("fixture feb bill should not exist: %v", err)
	}
	return s, clk
}

// finalPayEvent 返回那条与结清付款竞争的二月事件（数量 3，发生在当前时刻）。
func finalPayEvent() Event {
	return Event{AccountID: finalPayAcct, EventID: finalEventID, At: finalPayNow, Quantity: finalEventQty}
}

// recordRestPayment 登记最后一笔 600 分付款；first 为 true 时断言首次登记，
// 否则断言原样重报（Registered=false，返回首次登记完成时的历史结果 0/已付清）。
func recordRestPayment(t *testing.T, s *Service, first bool) PaymentResult {
	t.Helper()
	r, err := s.RecordPayment(finalPayAcct, finalPayID, jan(2026), finalPayRest)
	if err != nil {
		t.Fatalf("record final payment 600: %v", err)
	}
	want := Payment{AccountID: finalPayAcct, PaymentID: finalPayID, Period: jan(2026), Amount: finalPayRest}
	if r.Payment != want {
		t.Fatalf("final payment content = %+v, want %+v", r.Payment, want)
	}
	if r.Registered != first {
		t.Fatalf("final payment registered = %v, want %v", r.Registered, first)
	}
	if r.BillBalance != 0 || !r.Settled {
		t.Fatalf("final payment result: balance=%d settled=%v, want 0/true", r.BillBalance, r.Settled)
	}
	return r
}

// wantJanBillSettled 校验一月旧账单：已付 1000、余额 0、已付清；应付金额、
// 用量明细与截止时刻保持出账时的固定值，不被任何付款或上报改动。
func wantJanBillSettled(t *testing.T, s *Service) {
	t.Helper()
	b, err := s.GetBill(finalPayAcct, jan(2026))
	if err != nil {
		t.Fatalf("get jan bill: %v", err)
	}
	if b.TotalDue != finalBillDue || b.Paid != finalBillDue || b.Balance != 0 || !b.Settled {
		t.Fatalf("jan bill state: due=%d paid=%d balance=%d settled=%v, want 1000/1000/0/true",
			b.TotalDue, b.Paid, b.Balance, b.Settled)
	}
	if b.MonthlyFee != 1000 || b.OverageFee != 0 || b.Tax != 0 || b.TotalUsage != 0 ||
		b.IncludedUnits != 0 || b.OverageUnits != 0 {
		t.Fatalf("jan bill fixed detail changed: %+v", b)
	}
	if !b.DueAt.Equal(finalDueAt) {
		t.Fatalf("jan bill due at = %v, want %v", b.DueAt, finalDueAt)
	}
}

// wantRestoredSubscription 校验结清后：不再欠费停用、订阅仍生效且套餐条件
// 仍是开通时保存的 plan1000 快照——付款只解除停用，不改变订阅。
func wantRestoredSubscription(t *testing.T, s *Service) AccountStatus {
	t.Helper()
	st, err := s.Status(finalPayAcct)
	if err != nil {
		t.Fatalf("status after payment: %v", err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("flags after payment: subscribed=%v suspended=%v, want true/false",
			st.Subscribed, st.Suspended)
	}
	if st.CurrentTerms != termsOf(Plan{ID: "plan1000", MonthlyFee: 1000}) {
		t.Fatalf("terms changed by payment: %+v", st.CurrentTerms)
	}
	if st.PendingChange != nil || st.ScheduledEnd != nil {
		t.Fatalf("payment fabricated plan change / cancellation: %+v", st)
	}
	return st
}

// wantFebTotal 同时通过 MonthlyUsage 与 Status 校验二月累计用量。二月从未
// 接收过事件且未出账时，Status 的用量列表里本就不包含该账期（与零值等价），
// 因此 want 为 0 时允许其缺省。
func wantFebTotal(t *testing.T, s *Service, want int64) {
	t.Helper()
	u, err := s.MonthlyUsage(finalPayAcct, feb(2026))
	if err != nil {
		t.Fatalf("monthly usage: %v", err)
	}
	if u.Period != feb(2026) || u.Total != want {
		t.Fatalf("feb monthly usage = %+v, want total %d", u, want)
	}
	st, err := s.Status(finalPayAcct)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var found bool
	for _, su := range st.MonthlyUsage {
		if su.Period == feb(2026) {
			found = true
			if su.Total != want {
				t.Fatalf("feb status usage = %d, want %d", su.Total, want)
			}
		}
	}
	if !found && want != 0 {
		t.Fatalf("feb usage missing from status, want total %d: %+v", want, st.MonthlyUsage)
	}
}

// TestUsageBeforeFinalPaymentRejectedThenAccepted 确定性覆盖第一种顺序：
// 新用量在付款结清之前被处理，必须返回 ErrSuspended 且不累计；付款随后
// 成功也不能改写这次拒绝。拒收不消耗事件标识，结清后原样提交按首次接收，
// 再提交才是幂等重报。
func TestUsageBeforeFinalPaymentRejectedThenAccepted(t *testing.T) {
	s, clk := suspendedFinalPaymentFixture(t)
	ev := finalPayEvent()

	// 停用期间上报：ErrSuspended，当前账期累计仍为 0。
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrSuspended) {
		t.Fatalf("event while suspended: r err=%v, want ErrSuspended", err)
	}
	wantFebTotal(t, s, 0)

	// 付款随后成功结清：首次登记，返回余额 0、已付清。
	recordRestPayment(t, s, true)
	// 旧账单只更新已付与余额；应付与截止时刻保持出账时的值。
	wantJanBillSettled(t, s)
	// 不再欠费停用，订阅条件不因付款改变。
	wantRestoredSubscription(t, s)
	// 付款成功不替用户补录刚被拒绝的事件：二月累计仍为 0。
	wantFebTotal(t, s, 0)

	// 原样提交刚才被拒的事件：拒收没有占用标识，本次作为首次接收。
	r, err := s.RecordEvent(ev)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("resubmit rejected event: r=%+v err=%v, want accepted into 2026-02", r, err)
	}
	wantFebTotal(t, s, finalEventQty)

	// 再次原样提交：此时才是已接收事件的重报，Accepted=false、不重复计量。
	if r, err := s.RecordEvent(ev); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("identical replay after acceptance: r=%+v err=%v", r, err)
	}
	wantFebTotal(t, s, finalEventQty)

	// 最后一笔付款原样重报：不再次登记，返回首次登记完成时的历史结果
	// （余额 0、已付清），账单当前状态不变。
	recordRestPayment(t, s, false)
	wantJanBillSettled(t, s)
	wantFebTotal(t, s, finalEventQty)

	// 时钟未被任何操作推动；二月始终未出账。
	if !clk.t.Equal(finalPayNow) {
		t.Fatalf("clock moved unexpectedly: %v", clk.t)
	}
	if _, err := s.GetBill(finalPayAcct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("feb bill should still not exist: %v", err)
	}
}

// TestPaymentBeforeUsageEventAcceptedThenReplayed 确定性覆盖第二种顺序：
// 付款先完成、账户恢复，上报成功并累计 3；再次原样提交为重报（Accepted
// =false），不重复计量。
func TestPaymentBeforeUsageEventAcceptedThenReplayed(t *testing.T) {
	s, clk := suspendedFinalPaymentFixture(t)
	ev := finalPayEvent()

	// 付款先完成：首次登记，余额 0、已付清，账户立即解除停用。
	recordRestPayment(t, s, true)
	wantJanBillSettled(t, s)
	wantRestoredSubscription(t, s)

	// 上报新用量：首次接收成功，归入尚未出账的 2026-02，累计变为 3。
	r, err := s.RecordEvent(ev)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("event after payment: r=%+v err=%v, want accepted into 2026-02", r, err)
	}
	wantFebTotal(t, s, finalEventQty)

	// 原样重报：Accepted=false、账期仍为 2026-02，不再次累计。
	if r, err := s.RecordEvent(ev); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("identical replay: r=%+v err=%v", r, err)
	}
	wantFebTotal(t, s, finalEventQty)

	// 付款重报仍返回首次登记时的历史结果（0/已付清），不再次增加已付金额。
	recordRestPayment(t, s, false)
	wantJanBillSettled(t, s)
	wantFebTotal(t, s, finalEventQty)

	if !clk.t.Equal(finalPayNow) {
		t.Fatalf("clock moved unexpectedly: %v", clk.t)
	}
	if _, err := s.GetBill(finalPayAcct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("feb bill should still not exist: %v", err)
	}
}

// paymentEventOutcome 收集并发竞争中一方的原始返回，供按顺序分类断言。
type paymentEventOutcome struct {
	payR   PaymentResult
	payErr error
	evR    EventResult
	evErr  error
}

// TestFinalPaymentConcurrentWithNewUsage 真并发版本：用启动屏障让“登记剩余
// 600”与“上报数量 3 的二月事件”在同一时刻争用同一账户。实现把两者的
// 全部裁决都放在同一把互斥锁的一个临界区内，因此终态必须且只能与两种
// 串行顺序之一一致：
//
//   - 事件先裁决：事件 ErrSuspended、累计为 0；
//   - 付款先裁决：事件 Accepted=true、累计为 3。
//
// 两种顺序下付款都必须首次登记成功（余额 0、已付清）。并发结束后再原样
// 提交事件：被拒路径首次接收（Accepted=true），已接收路径幂等重报
// （Accepted=false），两条路径最终累计都恰为 3，不允许失败却累计或成功
// 却不累计。
func TestFinalPaymentConcurrentWithNewUsage(t *testing.T) {
	const rounds = 200
	for iter := 0; iter < rounds; iter++ {
		s, _ := suspendedFinalPaymentFixture(t)
		ev := finalPayEvent()

		var oc paymentEventOutcome
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			oc.payR, oc.payErr = s.RecordPayment(finalPayAcct, finalPayID, jan(2026), finalPayRest)
		}()
		go func() {
			defer wg.Done()
			<-start
			oc.evR, oc.evErr = s.RecordEvent(ev)
		}()
		close(start)
		wg.Wait()

		// 付款与用量先后无关：最后一笔付款都首次登记成功，恰好结清账单。
		if oc.payErr != nil || !oc.payR.Registered ||
			oc.payR.Payment != (Payment{AccountID: finalPayAcct, PaymentID: finalPayID, Period: jan(2026), Amount: finalPayRest}) ||
			oc.payR.BillBalance != 0 || !oc.payR.Settled {
			t.Fatalf("iter %d concurrent payment: r=%+v err=%v", iter, oc.payR, oc.payErr)
		}

		// 事件只能是“被停用拒绝”或“首次接收成功”二者之一；并发首报绝不能
		// 命中重报（Accepted=false 且无错误），也不能返回任何其他错误。
		eventRejected := false
		switch {
		case errors.Is(oc.evErr, ErrSuspended):
			eventRejected = true
		case oc.evErr == nil && oc.evR.Accepted && oc.evR.Period == feb(2026):
			// 付款先裁决：首次接收。
		default:
			t.Fatalf("iter %d concurrent event: r=%+v err=%v", iter, oc.evR, oc.evErr)
		}

		// 自洽性：返回结果必须与累计用量一致——失败却累计增加、成功却没有
		// 累计都不允许。
		u, err := s.MonthlyUsage(finalPayAcct, feb(2026))
		if err != nil {
			t.Fatalf("iter %d monthly usage: %v", iter, err)
		}
		if eventRejected {
			if u.Total != 0 {
				t.Fatalf("iter %d rejected event accumulated %d", iter, u.Total)
			}
		} else if u.Total != finalEventQty {
			t.Fatalf("iter %d accepted event missing accumulation, total=%d", iter, u.Total)
		}

		// 无论哪种顺序，账单已结清、固定字段不变，账户恢复且订阅条件不变。
		wantJanBillSettled(t, s)
		wantRestoredSubscription(t, s)

		// 并发结束后原样再提交同一事件：被拒路径本次首次接收，成功路径本次
		// 幂等重报；两条路径此刻起累计都恰为 3。
		r2, err := s.RecordEvent(ev)
		if err != nil {
			t.Fatalf("iter %d second submit: %v", iter, err)
		}
		if r2.Accepted != eventRejected {
			t.Fatalf("iter %d second submit accepted=%v, eventRejected=%v", iter, r2.Accepted, eventRejected)
		}
		if r2.Period != feb(2026) {
			t.Fatalf("iter %d second submit period=%s", iter, r2.Period)
		}
		wantFebTotal(t, s, finalEventQty)

		// 第三次提交无论哪条路径都是重报：Accepted=false，累计不再变化。
		if r3, err := s.RecordEvent(ev); err != nil || r3.Accepted || r3.Period != feb(2026) {
			t.Fatalf("iter %d third submit: r=%+v err=%v", iter, r3, err)
		}
		wantFebTotal(t, s, finalEventQty)

		// 付款重报不再次登记；二月始终未出账。
		recordRestPayment(t, s, false)
		if _, err := s.GetBill(finalPayAcct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("iter %d feb bill should not exist: %v", iter, err)
		}
	}
}
