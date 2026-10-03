package meter

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件保障“迟到用量与月度出账在同一账期边界竞争”时的行为：
// 一月已结束、一月账单尚未生成时，补报一月末用量与生成一月账单两项操作
// 通过现有入口发起，允许任一先完成，但事件上报结果、累计用量与账单必须
// 对应同一个结果——不能出现用量已接收却未计费，或上报失败却增加了账单金额。
//
// 公共场景（全部 UTC）：
//
//	2026-01-01 00:00:00  开通套餐：月费 1000 分、包含 10 单位、
//	                     超额单价 7 分、税率万分之 500。
//	一月内               已成功接收 8 单位用量，一月未出账。
//	2026-02-01 00:00:00  当前时刻。新事件发生于 2026-01-31 23:59:59、
//	                     数量 5，使用账户从未接收过的事件标识，应归入一月。
//
// 两种合法的确定结果：
//
//	事件先被接收：上报成功，一月累计 13；账单总用量 13、超额 3 单位、
//	超额费用 21 分、税额 51 分、应付 1072 分。
//	账单先生成：事件返回 ErrMonthBilled，一月累计保持 8；账单总用量 8、
//	超额费用 0、税额 50 分、应付 1050 分。

// lateEventPlan 是本场景共用的套餐条件。
var lateEventPlan = Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 7, TaxRateBasisPoints: 500}

// lateEvent 返回那条在一月末发生、二月上报的迟到事件（每次调用构造相同内容）。
func lateEvent() Event {
	return Event{
		AccountID: "u",
		EventID:   "late-jan",
		At:        utc(2026, 1, 31, 23, 59).Add(59 * time.Second), // 2026-01-31 23:59:59 UTC
		Quantity:  5,
	}
}

// newLateEventScenario 搭好公共场景并返回时钟停在 2026-02-01 00:00:00 的服务。
func newLateEventScenario(t *testing.T) *Service {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, lateEventPlan)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "p", utc(2026, 1, 1, 0, 0))
	// 一月内接收 8 单位用量。
	clk.t = utc(2026, 1, 15, 12, 0)
	if r, err := s.RecordEvent(Event{AccountID: "u", EventID: "jan-base", At: utc(2026, 1, 15, 0, 0), Quantity: 8}); err != nil ||
		!r.Accepted || r.Period != jan(2026) {
		t.Fatalf("base jan usage: %+v %v", r, err)
	}
	// 进入二月：一月已结束且未出账，账户无其他账单或欠费。
	clk.t = utc(2026, 2, 1, 0, 0)
	return s
}

// wantLateEventBill 断言一月账单的用量明细与全部金额字段。
func wantLateEventBill(t *testing.T, b Bill, totalUsage, overageUnits, overageFee, tax, totalDue int64) {
	t.Helper()
	if b.AccountID != "u" || b.Period != jan(2026) {
		t.Fatalf("bill identity = %s/%s, want u/%s", b.AccountID, b.Period, jan(2026))
	}
	if b.Terms != termsOf(lateEventPlan) {
		t.Fatalf("bill terms = %+v, want %+v", b.Terms, termsOf(lateEventPlan))
	}
	if b.TotalUsage != totalUsage || b.IncludedUnits != 10 || b.OverageUnits != overageUnits {
		t.Fatalf("bill usage detail = total %d included %d overage %d, want total %d included 10 overage %d",
			b.TotalUsage, b.IncludedUnits, b.OverageUnits, totalUsage, overageUnits)
	}
	if b.MonthlyFee != 1000 || b.OverageFee != overageFee || b.Tax != tax || b.TotalDue != totalDue {
		t.Fatalf("bill amounts = fee %d overageFee %d tax %d due %d, want 1000/%d/%d/%d",
			b.MonthlyFee, b.OverageFee, b.Tax, b.TotalDue, overageFee, tax, totalDue)
	}
	if b.Paid != 0 || b.Balance != totalDue || b.Settled {
		t.Fatalf("bill payment state = paid %d balance %d settled %v, want 0/%d/false",
			b.Paid, b.Balance, b.Settled, totalDue)
	}
	if !b.DueAt.Equal(jan(2026).End().AddDate(0, 0, 7)) {
		t.Fatalf("bill due at = %v, want %v", b.DueAt, jan(2026).End().AddDate(0, 0, 7))
	}
}

// wantConsistentViews 断言操作结束后用量查询、账单查询与账户状态展示同一组数据，
// 且再次为一月出账沿用已生成的金额与用量。
func wantConsistentViews(t *testing.T, s *Service, usageTotal, totalDue int64) {
	t.Helper()
	if u, err := s.MonthlyUsage("u", jan(2026)); err != nil || u.Total != usageTotal {
		t.Fatalf("monthly usage = %+v %v, want total %d", u, err, usageTotal)
	}
	b, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatalf("get jan bill: %v", err)
	}
	if b.TotalUsage != usageTotal || b.TotalDue != totalDue {
		t.Fatalf("bill = usage %d due %d, want usage %d due %d", b.TotalUsage, b.TotalDue, usageTotal, totalDue)
	}
	// 重复出账得到同一张账单：金额与用量不因再次出账或此前的上报而变化。
	again, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("re-create jan bill: %v", err)
	}
	if again != b {
		t.Fatalf("jan bill not idempotent:\nfirst=%+v\nagain=%+v", b, again)
	}
	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("status = subscribed %v suspended %v, want true/false", st.Subscribed, st.Suspended)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) || st.MonthlyUsage[0].Total != usageTotal {
		t.Fatalf("status usage = %+v, want jan total %d", st.MonthlyUsage, usageTotal)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) || st.Bills[0].TotalDue != totalDue ||
		st.Bills[0].Balance != totalDue || st.Bills[0].Settled {
		t.Fatalf("status bills = %+v, want jan due %d unpaid", st.Bills, totalDue)
	}
}

// TestLateEventAcceptedBeforeJanuaryBill 覆盖“事件先被接收”的确定结果：
// 上报成功并计入一月，随后出账按 13 单位计费；出账后原样重报仍成功但不再累计。
func TestLateEventAcceptedBeforeJanuaryBill(t *testing.T) {
	s := newLateEventScenario(t)
	ev := lateEvent()

	// 先补报一月末用量：本次新增成功，归入一月，一月累计 8+5=13。
	r, err := s.RecordEvent(ev)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("late event = %+v %v, want accepted into %s", r, err, jan(2026))
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 13 {
		t.Fatalf("jan usage after late event = %d, want 13", u.Total)
	}

	// 再生成一月账单：总用量 13、超额 3 单位、超额费用 3*7=21 分、
	// 税额 (1000+21)*5% = 51.05 → 51 分、应付 1072 分。
	bill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	wantLateEventBill(t, bill, 13, 3, 21, 51, 1072)

	// 出账后原样重报该事件：仍成功，但表示没有再次累计。
	replay, err := s.RecordEvent(ev)
	if err != nil || replay.Accepted || replay.Period != jan(2026) {
		t.Fatalf("replay after billed = %+v %v, want non-accumulating success", replay, err)
	}

	// 用量、账单与账户状态展示相符的数据；再次出账沿用已生成结果。
	wantConsistentViews(t, s, 13, 1072)
}

// TestLateEventRejectedAfterJanuaryBill 覆盖“一月账单先生成”的确定结果：
// 事件返回已出账错误，用量与账单保持出账时的 8 单位；此后原样再报仍失败，
// 不能变成已接收事件的重报，也不能改动任何用量或金额。
func TestLateEventRejectedAfterJanuaryBill(t *testing.T) {
	s := newLateEventScenario(t)
	ev := lateEvent()

	// 先生成一月账单：总用量 8、未超额、税额 1000*5% = 50 分、应付 1050 分。
	bill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	wantLateEventBill(t, bill, 8, 0, 0, 50, 1050)

	// 再补报一月末用量：一月已出账，返回现有的已出账错误。
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("late event after billed: %v, want ErrMonthBilled", err)
	}
	// 上报失败不增加用量，账单金额不变。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 8 {
		t.Fatalf("jan usage after rejected event = %d, want 8", u.Total)
	}
	if b, _ := s.GetBill("u", jan(2026)); b != bill {
		t.Fatalf("bill changed after rejected event:\nwas=%+v\nnow=%+v", bill, b)
	}

	// 因出账被拒绝的事件原样再报仍应失败：不能变成已接收事件的重报。
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("replay of rejected event: %v, want ErrMonthBilled", err)
	}

	// 用量、账单与账户状态展示相符的数据；再次出账沿用已生成结果。
	wantConsistentViews(t, s, 8, 1050)
}

// TestConcurrentLateEventAndJanuaryBill 让迟到事件上报与一月出账并发进行：
// 不规定哪项操作先成功，但最终状态必须是两种合法结果之一——
// 事件被接收则账单按 13 单位计 1072 分，事件被拒绝则账单保持 8 单位 1050 分；
// 不允许出现用量已接收却未计费，或上报失败却增加了账单金额。
func TestConcurrentLateEventAndJanuaryBill(t *testing.T) {
	for round := 0; round < 32; round++ {
		s := newLateEventScenario(t)
		ev := lateEvent()

		var wg sync.WaitGroup
		var evResult EventResult
		var evErr error
		var bill Bill
		var billErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			evResult, evErr = s.RecordEvent(ev)
		}()
		go func() {
			defer wg.Done()
			bill, billErr = s.CreateBill("u", jan(2026))
		}()
		wg.Wait()

		if billErr != nil {
			t.Fatalf("round %d: create bill: %v", round, billErr)
		}
		switch {
		case evErr == nil:
			// 事件被接收：必须本次新增成功，且账单按 13 单位计费。
			if !evResult.Accepted || evResult.Period != jan(2026) {
				t.Fatalf("round %d: event result = %+v, want accepted into %s", round, evResult, jan(2026))
			}
			if bill.TotalUsage != 13 || bill.TotalDue != 1072 {
				t.Fatalf("round %d: accepted event not billed: usage %d due %d, want 13/1072",
					round, bill.TotalUsage, bill.TotalDue)
			}
			wantConsistentViews(t, s, 13, 1072)
		case errors.Is(evErr, ErrMonthBilled):
			// 账单先生成：事件被拒绝，账单必须保持 8 单位。
			if bill.TotalUsage != 8 || bill.TotalDue != 1050 {
				t.Fatalf("round %d: rejected event still billed: usage %d due %d, want 8/1050",
					round, bill.TotalUsage, bill.TotalDue)
			}
			wantConsistentViews(t, s, 8, 1050)
		default:
			t.Fatalf("round %d: unexpected event error: %v", round, evErr)
		}
	}
}
