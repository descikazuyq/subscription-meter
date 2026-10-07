package meter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// 本文件回归“登记最后一笔付款的同时查询账户状态”。
//
// 场景：账户只有一张已经到期的一月账单（应付 1650 分、付款截止 2026-02-08、
// 当前时刻 2026-02-10），此前已登记 400 分部分付款，账户因这张账单停用。
// 此时用新的付款标识登记剩余 1250 分，并在付款进行期间并发查询账户状态。
//
// 实现把 RecordPayment 的扣款落库与 Status 的快照组装各自放在同一把互斥锁的
// 一个临界区内，因此任何一份成功返回的查询结果都完整落在两种真实状态之一：
//   - 付款尚未登记：摘要已付 400、余额 1250、未付清，停用标志为真；
//   - 付款已经登记：摘要已付 1650、余额 0、已付清，停用标志为假。
//
// 这些测试锁住该行为，防止日后把查询拆成分步读取（或把付款拆成分步落库）而
// 拼出互相矛盾的结果：余额已归零但仍未付清、账单已结清但账户仍因此停用、
// 已付与余额之和不等于应付等。查询与付款谁先完成都可以，不要求并发的查询
// 看到同一种状态；但付款成功返回之后发起的查询必须显示结清与解除停用。
// 付款只改变收款与欠费状态：应付金额、付款截止、历史累计用量与套餐条件
// 保持原值，不重新计费。

// statusConcurrentTerms 是本文件场景使用的套餐条件快照：
// 月费 1000 分、包含 10 单位、超额单价 100 分、税率 10%。
var statusConcurrentTerms = PlanTerms{
	PlanID:             "plan-status-concurrent",
	MonthlyFee:         1000,
	IncludedUnits:      10,
	OveragePrice:       100,
	TaxRateBasisPoints: 1000,
}

// setupStatusConcurrentFixture 建立场景账户：2026-01-01 零点 UTC 开通且一直
// 有效的订阅，一月接收 15 单位用量，出一张应付 1650 分、截止 2026-02-08 的
// 账单，已登记 400 分部分付款；结束时时钟停在 2026-02-10（账单已到期，
// 账户因这张账单停用，订阅仍然有效）。
func setupStatusConcurrentFixture(t *testing.T, s *Service, clk *fakeClock, acct string) {
	t.Helper()
	mustAccount(t, s, acct)
	mustSubscribe(t, s, acct, statusConcurrentTerms.PlanID, utc(2026, 1, 1, 0, 0))
	clk.t = utc(2026, 1, 20, 0, 0)
	mustRecordEvent(t, s, Event{AccountID: acct, EventID: "jan-usage", At: utc(2026, 1, 15, 12, 0), Quantity: 15})

	clk.t = utc(2026, 2, 10, 0, 0)
	b, err := s.CreateBill(acct, jan(2026))
	if err != nil {
		t.Fatalf("create bill %q: %v", acct, err)
	}
	// 月费 1000 + 超额 5×100 = 1500，税 10% 计 150，应付合计 1650。
	if b.TotalDue != 1650 || b.MonthlyFee != 1000 || b.OverageFee != 500 || b.Tax != 150 ||
		b.TotalUsage != 15 || b.Paid != 0 || b.Balance != 1650 || b.Settled {
		t.Fatalf("bill %q initial state: %+v", acct, b)
	}
	if !b.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("bill %q due at %v, want 2026-02-08T00:00Z", acct, b.DueAt)
	}

	part, err := s.RecordPayment(acct, "pay-part-400", jan(2026), 400)
	if err != nil || !part.Registered || part.BillBalance != 1250 || part.Settled {
		t.Fatalf("partial payment %q: r=%+v err=%v, want balance=1250 settled=false", acct, part, err)
	}
}

// checkStatusSnapshot 校验一份并发期间取得的状态查询结果：不变字段保持原值，
// 且付款相关字段完整落在“付款前”或“付款后”两种真实状态之一。
// 返回空串表示合法，否则返回违规描述。
func checkStatusSnapshot(st AccountStatus) string {
	// 订阅自 2026-01-01 起一直有效，付款不影响订阅与套餐条件。
	if !st.Subscribed {
		return fmt.Sprintf("Subscribed=false, subscription should stay active: %+v", st)
	}
	if st.CurrentTerms != statusConcurrentTerms {
		return fmt.Sprintf("CurrentTerms=%+v, want %+v", st.CurrentTerms, statusConcurrentTerms)
	}
	if st.PendingChange != nil || st.ScheduledEnd != nil {
		return fmt.Sprintf("unexpected pending change or scheduled end: %+v", st)
	}
	// 历史累计用量保持一月的 15 单位，付款不重新计费。
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) || st.MonthlyUsage[0].Total != 15 {
		return fmt.Sprintf("MonthlyUsage=%+v, want single Jan entry of 15", st.MonthlyUsage)
	}
	// 结果仍应包含同一张一月账单，应付金额与付款截止保持原值。
	if len(st.Bills) != 1 {
		return fmt.Sprintf("Bills=%+v, want exactly the Jan bill", st.Bills)
	}
	bs := st.Bills[0]
	if bs.Period != jan(2026) || bs.TotalDue != 1650 || !bs.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		return fmt.Sprintf("bill immutable fields changed: %+v", bs)
	}
	// 已付与余额之和始终等于应付 1650。
	if bs.Paid+bs.Balance != 1650 {
		return fmt.Sprintf("paid %d + balance %d != 1650: %+v", bs.Paid, bs.Balance, bs)
	}
	// 付清标志与余额、停用标志与付清状态必须互相一致，且整体恰好落在
	// 两种真实状态之一：付款前（400/1250/未付清/停用）或
	// 付款后（1650/0/已付清/未停用）。
	before := bs.Paid == 400 && bs.Balance == 1250 && !bs.Settled && st.Suspended
	after := bs.Paid == 1650 && bs.Balance == 0 && bs.Settled && !st.Suspended
	if !before && !after {
		return fmt.Sprintf("contradictory snapshot: paid=%d balance=%d settled=%v suspended=%v",
			bs.Paid, bs.Balance, bs.Settled, st.Suspended)
	}
	return ""
}

// TestStatusDuringFinalPaymentConsistentSnapshot 并发登记最后一笔 1250 分付款
// 并同时查询账户状态：每份成功查询结果都必须是付款前或付款后的完整一致快照，
// 不允许把付款前后的字段拼在一起。付款成功返回后再发起的查询必须显示结清
// 与解除停用。
func TestStatusDuringFinalPaymentConsistentSnapshot(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{
		ID:                 statusConcurrentTerms.PlanID,
		MonthlyFee:         statusConcurrentTerms.MonthlyFee,
		IncludedUnits:      statusConcurrentTerms.IncludedUnits,
		OveragePrice:       statusConcurrentTerms.OveragePrice,
		TaxRateBasisPoints: statusConcurrentTerms.TaxRateBasisPoints,
	})

	const rounds = 50
	const queriers = 4
	var totalQueries atomic.Int64

	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("acct-%d", iter)
		setupStatusConcurrentFixture(t, s, clk, acct)

		// 付款前的查询：已付 400、余额 1250、未付清，账户因这张账单停用。
		st, err := s.Status(acct)
		if err != nil {
			t.Fatalf("iter %d pre status: %v", iter, err)
		}
		if msg := checkStatusSnapshot(st); msg != "" {
			t.Fatalf("iter %d pre status: %s", iter, msg)
		}
		if bs := statusBillSummary(t, st, jan(2026)); bs.Paid != 400 || bs.Balance != 1250 || bs.Settled || !st.Suspended {
			t.Fatalf("iter %d pre status should be the not-yet-paid state: %+v", iter, st)
		}

		// 启动屏障让付款与查询真正并发：一个 goroutine 用尚未使用过的付款
		// 标识登记剩余 1250 分，若干 goroutine 在付款完成前持续查询状态。
		start := make(chan struct{})
		done := make(chan struct{})
		violations := make([][]string, queriers)
		var wg sync.WaitGroup
		for q := 0; q < queriers; q++ {
			wg.Add(1)
			go func(q int) {
				defer wg.Done()
				<-start
				for {
					select {
					case <-done:
						return
					default:
					}
					totalQueries.Add(1)
					st, err := s.Status(acct)
					if err != nil {
						violations[q] = append(violations[q], fmt.Sprintf("status error: %v", err))
						return
					}
					if msg := checkStatusSnapshot(st); msg != "" {
						violations[q] = append(violations[q], msg)
						return
					}
				}
			}(q)
		}
		var payRes PaymentResult
		var payErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			payRes, payErr = s.RecordPayment(acct, "pay-final-1250", jan(2026), 1250)
			close(done)
		}()
		close(start)
		wg.Wait()

		for q := 0; q < queriers; q++ {
			for _, msg := range violations[q] {
				t.Fatalf("iter %d querier %d observed inconsistent snapshot: %s", iter, q, msg)
			}
		}

		// 最后一笔付款应成功登记并结清账单。
		if payErr != nil || !payRes.Registered || payRes.BillBalance != 0 || !payRes.Settled {
			t.Fatalf("iter %d final payment: r=%+v err=%v, want registered balance=0 settled=true",
				iter, payRes, payErr)
		}

		// 付款成功返回之后发起的查询必须显示结清与解除停用。
		st, err = s.Status(acct)
		if err != nil {
			t.Fatalf("iter %d post status: %v", iter, err)
		}
		if msg := checkStatusSnapshot(st); msg != "" {
			t.Fatalf("iter %d post status: %s", iter, msg)
		}
		if bs := statusBillSummary(t, st, jan(2026)); bs.Paid != 1650 || bs.Balance != 0 || !bs.Settled || st.Suspended {
			t.Fatalf("iter %d post status should be the settled state: %+v", iter, st)
		}
	}

	if totalQueries.Load() == 0 {
		t.Fatal("no status query overlapped the final payment window")
	}
}

// TestFinalPaymentSettlesBillAndLiftsSuspension 以确定顺序锁住同一场景：
// 付款前查询显示部分付款与停用，最后一笔付款成功登记后查询显示结清与
// 解除停用；付款只改变收款与欠费状态，账单金额、付款截止、用量与套餐
// 条件保持原值，重报此前的部分付款仍返回其历史结果且不改变当前状态。
func TestFinalPaymentSettlesBillAndLiftsSuspension(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{
		ID:                 statusConcurrentTerms.PlanID,
		MonthlyFee:         statusConcurrentTerms.MonthlyFee,
		IncludedUnits:      statusConcurrentTerms.IncludedUnits,
		OveragePrice:       statusConcurrentTerms.OveragePrice,
		TaxRateBasisPoints: statusConcurrentTerms.TaxRateBasisPoints,
	})
	setupStatusConcurrentFixture(t, s, clk, "solo")

	// 付款前：摘要 400/1250/未付清，停用为真，订阅仍有效。
	st, err := s.Status("solo")
	if err != nil {
		t.Fatalf("pre status: %v", err)
	}
	if bs := statusBillSummary(t, st, jan(2026)); bs.Paid != 400 || bs.Balance != 1250 || bs.Settled ||
		!st.Suspended || !st.Subscribed {
		t.Fatalf("pre status: %+v", st)
	}

	// 用尚未使用过的付款标识登记剩余 1250 分：成功登记并结清。
	pay, err := s.RecordPayment("solo", "pay-final-1250", jan(2026), 1250)
	if err != nil || !pay.Registered || pay.BillBalance != 0 || !pay.Settled {
		t.Fatalf("final payment: r=%+v err=%v", pay, err)
	}

	// 付款后：摘要 1650/0/已付清，解除停用；账单金额明细、付款截止、
	// 历史用量与套餐条件保持原值。
	st, err = s.Status("solo")
	if err != nil {
		t.Fatalf("post status: %v", err)
	}
	if msg := checkStatusSnapshot(st); msg != "" {
		t.Fatalf("post status: %s", msg)
	}
	if bs := statusBillSummary(t, st, jan(2026)); bs.Paid != 1650 || bs.Balance != 0 || !bs.Settled || st.Suspended {
		t.Fatalf("post status should be settled and not suspended: %+v", st)
	}
	b, err := s.GetBill("solo", jan(2026))
	if err != nil {
		t.Fatalf("get bill: %v", err)
	}
	if b.Paid != 1650 || b.Balance != 0 || !b.Settled ||
		b.TotalDue != 1650 || b.MonthlyFee != 1000 || b.OverageFee != 500 || b.Tax != 150 ||
		b.TotalUsage != 15 || !b.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("bill after final payment: %+v", b)
	}

	// 重报此前的 400 分部分付款：返回首次登记时的历史结果（余额 1250、
	// 未付清），不再次扣款，账单与状态保持结清后的当前状态。
	replay, err := s.RecordPayment("solo", "pay-part-400", jan(2026), 400)
	if err != nil || replay.Registered || replay.BillBalance != 1250 || replay.Settled {
		t.Fatalf("replay partial payment: r=%+v err=%v, want historical 1250/false", replay, err)
	}
	if b, _ := s.GetBill("solo", jan(2026)); b.Paid != 1650 || b.Balance != 0 || !b.Settled {
		t.Fatalf("bill after replay: %+v", b)
	}
	if st, _ := s.Status("solo"); st.Suspended {
		t.Fatalf("status after replay should stay unsuspended: %+v", st)
	}
}
