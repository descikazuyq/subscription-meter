package meter

import (
	"fmt"
	"sync"
	"testing"
)

// 本文件回归“登记最后一笔付款”与“账户状态查询”并发时的快照原子性：
// Status 组装账单付款摘要（已付、余额、付清标志）与账户欠费停用标志必须取自
// 同一个真实的付款状态，不能把付款前后的字段拼成一份自相矛盾的结果。
//
// 场景（全部 UTC）：
//
//	2026-01-01 00:00 开通套餐 plan-taxed（月费 1000、包含 10 单位、超额单价
//	           100、税率 10%），订阅一直有效：不取消、不换套餐。
//	2026-01-15 接收 15 单位用量。
//	2026-02-10 00:00（当前时刻，固定不动）为已结束的 2026-01 出正式账单：
//	           超额 5 单位 × 100 = 500，税前 1500，税 150，应付 1650；
//	           截止时刻固定为 2026-02-08 00:00（已到期）。此前已用标识
//	           pay-partial-400 登记 400 分部分付款：已付 400、余额 1250、
//	           未付清，账户因这张账单欠费停用，但订阅仍有效；没有其他欠款。
//	同一时刻并发到达：
//	   - 用尚未使用过的标识 pay-rest-1250 登记剩余 1250 分（恰好结清）；
//	   - 多个账户状态查询。
//
// 每份成功的查询结果都只能完整表示两种真实状态之一：
//
//   - 付款尚未登记：摘要 400 / 1250 / 未付清，停用标志为真；
//   - 付款已登记：摘要 1650 / 0 / 已付清，停用标志为假。
//
// 已付与余额之和始终等于应付 1650；不允许余额已归零却未付清、账单已结清
// 却仍停用（或账单未结清却未停用）这类拼接结果。查询与付款谁先裁决都可以，
// 既不要求查询一定看到付款后状态，也不要求所有查询看到同一种状态。最后一笔
// 付款成功返回之后再查询，必须显示结清并解除停用。付款只改变收款与欠费状态：
// 同一张一月账单的应付、截止时刻、历史累计用量与订阅套餐条件始终保持原值。

const (
	overdueStatusAcct = "acct-overdue-status"
	overdueStatusPlan = "plan-taxed"
	overduePartialID  = "pay-partial-400"
	overdueRestID     = "pay-rest-1250"
	overdueEventID    = "evt-jan-15"

	overdueBillDue    = int64(1650)
	overduePartialAmt = int64(400)
	overdueRestAmt    = int64(1250)
	overdueUsage      = int64(15)
)

var (
	overdueSubStart = utc(2026, 1, 1, 0, 0)
	overdueEventAt  = utc(2026, 1, 15, 0, 0)
	overdueNow      = utc(2026, 2, 10, 0, 0)
	overdueDueAt    = utc(2026, 2, 8, 0, 0)
)

// overduePlan 返回题述套餐：月费 1000、额度 10、超额单价 100、税率 10%。
func overduePlan() Plan {
	return Plan{
		ID:                 overdueStatusPlan,
		MonthlyFee:         1000,
		IncludedUnits:      10,
		OveragePrice:       100,
		TaxRateBasisPoints: 1000,
	}
}

// overdueStatusFixture 搭好题述起点并停在 2026-02-10 00:00：一月正式账单
// 应付 1650、已付 400、余额 1250、未付清，账户因这张已到期账单停用但订阅
// 仍有效；账户没有其他账单或欠款。
func overdueStatusFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(overdueSubStart)
	mustPlan(t, s, overduePlan())
	mustAccount(t, s, overdueStatusAcct)
	mustSubscribe(t, s, overdueStatusAcct, overdueStatusPlan, overdueSubStart)

	clk.t = overdueEventAt
	mustRecordEvent(t, s, Event{
		AccountID: overdueStatusAcct,
		EventID:   overdueEventID,
		At:        overdueEventAt,
		Quantity:  overdueUsage,
	})

	clk.t = overdueNow
	bill, err := s.CreateBill(overdueStatusAcct, jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	// 正式账单应付 1650：5 单位超额 × 100 = 500，(1000+500)×10% = 150。
	if bill.MonthlyFee != 1000 || bill.IncludedUnits != 10 || bill.OverageUnits != 5 ||
		bill.OverageFee != 500 || bill.Tax != 150 || bill.TotalDue != overdueBillDue ||
		bill.TotalUsage != overdueUsage {
		t.Fatalf("jan bill amounts: %+v, want fee=1000 overage=5x100 tax=150 due=1650 usage=15", bill)
	}
	if !bill.DueAt.Equal(overdueDueAt) {
		t.Fatalf("jan bill due at = %v, want %v", bill.DueAt, overdueDueAt)
	}

	partial, err := s.RecordPayment(overdueStatusAcct, overduePartialID, jan(2026), overduePartialAmt)
	if err != nil || !partial.Registered || partial.BillBalance != overdueRestAmt || partial.Settled {
		t.Fatalf("partial payment 400: r=%+v err=%v", partial, err)
	}

	// 付款前起点：400 / 1250 / 未付清，因到期账单停用，订阅仍有效。
	st, err := s.Status(overdueStatusAcct)
	if err != nil {
		t.Fatalf("fixture status: %v", err)
	}
	if settled := assertOverdueStatusCoherent(t, st, "fixture"); settled {
		t.Fatalf("fixture status unexpectedly shows final payment already registered: %+v", st)
	}
	return s, clk
}

// recordOverdueRest 登记最后一笔 1250 分付款；first 为 true 时断言首次登记，
// 否则断言原样重报（Registered=false，返回首次登记完成时的历史结果 0/已付清）。
func recordOverdueRest(t *testing.T, s *Service, first bool) PaymentResult {
	t.Helper()
	r, err := s.RecordPayment(overdueStatusAcct, overdueRestID, jan(2026), overdueRestAmt)
	if err != nil {
		t.Fatalf("record final payment 1250 (first=%v): %v", first, err)
	}
	want := Payment{
		AccountID: overdueStatusAcct,
		PaymentID: overdueRestID,
		Period:    jan(2026),
		Amount:    overdueRestAmt,
	}
	if r.Payment != want || r.Registered != first || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("final payment (first=%v) = %+v, want %+v registered=%v balance=0 settled=true",
			first, r, want, first)
	}
	return r
}

// assertOverdueStatusCoherent 校验一份付款期间取得的状态只能完整表示
// 付款前或付款后两种真实状态之一，返回 true 表示付款已登记（已结清）。
// 它同时锁住：已付+余额恒等于应付、余额归零与付清标志一致、账单结清状态与
// 账户停用标志互为反面，以及账单/用量/套餐条件等不随付款改变的固定字段。
func assertOverdueStatusCoherent(t *testing.T, st AccountStatus, label string) bool {
	t.Helper()
	if st.AccountID != overdueStatusAcct {
		t.Fatalf("%s: account id = %q, want %q", label, st.AccountID, overdueStatusAcct)
	}
	// 订阅一直有效：付款只改变收款与欠费状态，不取消、不换套餐。
	if !st.Subscribed {
		t.Fatalf("%s: Subscribed=false, want true: %+v", label, st)
	}
	if st.PendingChange != nil || st.ScheduledEnd != nil {
		t.Fatalf("%s: payment fabricated pending change or cancellation: %+v", label, st)
	}
	if want := termsOf(overduePlan()); st.CurrentTerms != want {
		t.Fatalf("%s: current terms = %+v, want %+v", label, st.CurrentTerms, want)
	}
	// 历史累计用量仍是一月 15：付款不重新计费，也不生成二月账期。
	if len(st.MonthlyUsage) != 1 ||
		st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: overdueUsage}) {
		t.Fatalf("%s: monthly usage = %+v, want only %s total %d",
			label, st.MonthlyUsage, jan(2026), overdueUsage)
	}
	// 账户始终只有这一张一月账单。
	if len(st.Bills) != 1 {
		t.Fatalf("%s: want exactly one bill summary, got %+v", label, st.Bills)
	}
	bs := st.Bills[0]
	if bs.Period != jan(2026) || bs.TotalDue != overdueBillDue || !bs.DueAt.Equal(overdueDueAt) {
		t.Fatalf("%s: bill period/due/drifted: %+v, want %s due=%d dueAt=%v",
			label, bs, jan(2026), overdueBillDue, overdueDueAt)
	}
	// 已付与余额之和始终等于应付 1650，且不允许负值。
	if bs.Paid < 0 || bs.Balance < 0 || bs.Paid+bs.Balance != overdueBillDue {
		t.Fatalf("%s: paid=%d balance=%d, want non-negative and sum %d",
			label, bs.Paid, bs.Balance, overdueBillDue)
	}
	// 不能出现余额已经归零但仍未付清，或仍有余额却标记付清。
	if (bs.Balance == 0) != bs.Settled {
		t.Fatalf("%s: balance=%d but settled=%v: %+v", label, bs.Balance, bs.Settled, bs)
	}
	// 账户只有这一张到期账单：停用标志必须与“账单未结清”严格一致，
	// 不能出现账单已结清却仍停用，或账单未结清却未停用。
	if st.Suspended == bs.Settled {
		t.Fatalf("%s: suspended=%v but bill settled=%v: summary=%+v",
			label, st.Suspended, bs.Settled, bs)
	}
	if !bs.Settled {
		// 付款尚未登记的完整快照：400 / 1250 / 未付清，停用。
		if bs.Paid != overduePartialAmt || bs.Balance != overdueRestAmt {
			t.Fatalf("%s: pre-payment summary = %+v, want paid=%d balance=%d",
				label, bs, overduePartialAmt, overdueRestAmt)
		}
		return false
	}
	// 付款已登记的完整快照：1650 / 0 / 已付清，解除停用。
	if bs.Paid != overdueBillDue || bs.Balance != 0 {
		t.Fatalf("%s: post-payment summary = %+v, want paid=%d balance=0",
			label, bs, overdueBillDue)
	}
	return true
}

// assertJanBillPostPayment 校验一月账单付款后只更新了收款状态：1650 已付、
// 零余额、已付清；套餐条件快照、用量、各项金额与截止时刻保持出账时的原值。
func assertJanBillPostPayment(t *testing.T, s *Service) {
	t.Helper()
	b, err := s.GetBill(overdueStatusAcct, jan(2026))
	if err != nil {
		t.Fatalf("get jan bill: %v", err)
	}
	if b.Paid != overdueBillDue || b.Balance != 0 || !b.Settled {
		t.Fatalf("jan bill payment state: paid=%d balance=%d settled=%v, want 1650/0/true",
			b.Paid, b.Balance, b.Settled)
	}
	if b.Terms != termsOf(overduePlan()) ||
		b.TotalUsage != overdueUsage || b.IncludedUnits != 10 || b.OverageUnits != 5 ||
		b.MonthlyFee != 1000 || b.OverageFee != 500 || b.Tax != 150 || b.TotalDue != overdueBillDue {
		t.Fatalf("jan bill fixed fields changed by payment: %+v", b)
	}
	if !b.DueAt.Equal(overdueDueAt) {
		t.Fatalf("jan bill due at = %v, want %v", b.DueAt, overdueDueAt)
	}
}

// TestStatusSnapshotsBeforeAndAfterFinalPayment 确定性地锁住两种合法快照本身：
// 付款前的查询完整显示 400/1250/未付清与停用；付款成功后的查询完整显示
// 1650/0/已付清与解除停用；付款重报与时钟推移都不改变这些结果。
func TestStatusSnapshotsBeforeAndAfterFinalPayment(t *testing.T) {
	s, clk := overdueStatusFixture(t)

	before, err := s.Status(overdueStatusAcct)
	if err != nil {
		t.Fatalf("status before payment: %v", err)
	}
	if assertOverdueStatusCoherent(t, before, "before payment") {
		t.Fatalf("status before payment unexpectedly settled: %+v", before)
	}

	recordOverdueRest(t, s, true)

	after, err := s.Status(overdueStatusAcct)
	if err != nil {
		t.Fatalf("status after payment: %v", err)
	}
	if !assertOverdueStatusCoherent(t, after, "after payment") {
		t.Fatalf("status after payment unexpectedly unpaid: %+v", after)
	}
	assertJanBillPostPayment(t, s)

	// 最后一笔付款的原样重报：不再次登记，返回首次登记时的历史结果 0/已付清，
	// 账单与账户状态保持结清、解除停用。
	recordOverdueRest(t, s, false)
	replayed, err := s.Status(overdueStatusAcct)
	if err != nil {
		t.Fatalf("status after replay: %v", err)
	}
	if !assertOverdueStatusCoherent(t, replayed, "after replay") {
		t.Fatalf("status after replay unexpectedly unpaid: %+v", replayed)
	}
	assertJanBillPostPayment(t, s)

	// 时钟始终固定在 2026-02-10，整个过程没有产生二月账单。
	if !clk.t.Equal(overdueNow) {
		t.Fatalf("clock moved unexpectedly: %v", clk.t)
	}
}

// statusGrab 收集一个并发查询的原始返回。
type statusGrab struct {
	st  AccountStatus
	err error
}

// TestStatusConcurrentWithFinalPaymentCoherent 真并发版本：用启动屏障让多份
// 状态查询与“登记剩余 1250”在同一时刻争用同一账户。实现把付款落库（已付、
// 余额、付清标志）、停用状态重算与状态组装全部放在同一把互斥锁的一个临界
// 区内，因此每份查询都必须取自某个真实的付款瞬间——要么完整付款前，要么
// 完整付款后；查询与付款谁先裁决都可以，不要求各查询看到同一种状态。
//
// 最后一笔付款必须首次登记成功并恰好结清；它成功返回之后再查询必须是付款后
// 状态；付款重报不改变任何状态；一月账单的固定字段全程不变。
func TestStatusConcurrentWithFinalPaymentCoherent(t *testing.T) {
	const rounds = 200
	const readers = 8
	for iter := 0; iter < rounds; iter++ {
		s, _ := overdueStatusFixture(t)

		grabs := make([]statusGrab, readers)
		var (
			payR   PaymentResult
			payErr error
		)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(readers + 1)
		for i := 0; i < readers; i++ {
			go func(i int) {
				defer wg.Done()
				<-start
				grabs[i].st, grabs[i].err = s.Status(overdueStatusAcct)
			}(i)
		}
		go func() {
			defer wg.Done()
			<-start
			payR, payErr = s.RecordPayment(overdueStatusAcct, overdueRestID, jan(2026), overdueRestAmt)
		}()
		close(start)
		wg.Wait()

		// 付款期间的查询都必须正常返回，且每份结果只完整表示两种状态之一。
		// 不要求两种状态都出现，也不要求全部查询相同；只禁止自相矛盾的拼接。
		for i, g := range grabs {
			if g.err != nil {
				t.Fatalf("iter %d reader %d status failed during payment: %v", iter, i, g.err)
			}
			assertOverdueStatusCoherent(t, g.st, fmt.Sprintf("iter %d reader %d", iter, i))
		}

		// 与查询并发的最后一笔付款必须首次登记成功，返回余额 0、已付清。
		if payErr != nil || !payR.Registered ||
			payR.Payment != (Payment{AccountID: overdueStatusAcct, PaymentID: overdueRestID, Period: jan(2026), Amount: overdueRestAmt}) ||
			payR.BillBalance != 0 || !payR.Settled {
			t.Fatalf("iter %d concurrent final payment: r=%+v err=%v", iter, payR, payErr)
		}

		// 付款成功返回之后再查询：必须显示结清并解除停用。
		final, err := s.Status(overdueStatusAcct)
		if err != nil {
			t.Fatalf("iter %d final status: %v", iter, err)
		}
		if !assertOverdueStatusCoherent(t, final, fmt.Sprintf("iter %d final", iter)) {
			t.Fatalf("iter %d status after successful payment still unpaid: %+v", iter, final)
		}
		assertJanBillPostPayment(t, s)

		// 原样重报最后一笔付款：不再次登记，状态仍停留在付款后快照。
		recordOverdueRest(t, s, false)
		postReplay, err := s.Status(overdueStatusAcct)
		if err != nil {
			t.Fatalf("iter %d status after replay: %v", iter, err)
		}
		if !assertOverdueStatusCoherent(t, postReplay, fmt.Sprintf("iter %d post-replay", iter)) {
			t.Fatalf("iter %d status regressed after replay: %+v", iter, postReplay)
		}
	}
}
