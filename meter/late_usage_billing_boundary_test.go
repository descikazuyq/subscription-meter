package meter

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归“迟到用量与月度出账同时发生”的账期边界：
//
//	账户于 2026-01-01 00:00:00 UTC 开通，套餐月费 1000 分、包含 10 单位、
//	超额单价 7 分、税率万分之 500（5%）。一月已接收用量 8 单位且尚未出账。
//	当前时刻为 2026-02-01 00:00:00 UTC：一月已经结束，此时用户一边补报
//	一条发生在 2026-01-31 23:59:59 UTC、数量 5 单位的新事件（事件应归入
//	一月），一边为一月生成账单。
//
// 两种先后顺序都符合现有规则，测试不规定并发时必须哪项先成功，但要求
// 事件结果、累计用量与账单始终对应同一个确定结果：
//
//   - 事件先被接收：Accepted=true，一月累计 13，账单总用量 13、超额 3、
//     超额费用 21、税额 51、应付 1072。
//   - 账单先生成：事件返回现有 ErrMonthBilled，一月累计保持 8，账单总用量
//     8、无超额、税额 50、应付 1050。
//
// 既不能“用量已接收却未计费”，也不能“上报失败却增加了账单金额”。

var boundaryTerms = PlanTerms{
	PlanID:             "boundary-plan",
	MonthlyFee:         1000,
	IncludedUnits:      10,
	OveragePrice:       7,
	TaxRateBasisPoints: 500,
}

// newLateUsageScenario 搭建共同的初始状态：时钟停在 2026-02-01 00:00:00 UTC，
// 一月已接收 8 单位用量、尚未出账，账户无其他账单与欠费。
func newLateUsageScenario(t *testing.T) *Service {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, Plan{
		ID:                 boundaryTerms.PlanID,
		MonthlyFee:         boundaryTerms.MonthlyFee,
		IncludedUnits:      boundaryTerms.IncludedUnits,
		OveragePrice:       boundaryTerms.OveragePrice,
		TaxRateBasisPoints: boundaryTerms.TaxRateBasisPoints,
	})
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", boundaryTerms.PlanID, utc(2026, 1, 1, 0, 0))
	if r, err := s.RecordEvent(Event{
		AccountID: "u",
		EventID:   "jan-usage-8",
		At:        utc(2026, 1, 20, 0, 0),
		Quantity:  8,
	}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("seed january usage: %+v %v", r, err)
	}
	// 进入二月零点：一月已结束，补报事件与出账都在此时发起。
	clk.t = utc(2026, 2, 1, 0, 0)
	return s
}

// lateJanuaryEvent 是一月末发生、二月补报的新事件：账户此前未接收过该标识。
func lateJanuaryEvent() Event {
	return Event{
		AccountID: "u",
		EventID:   "late-jan-31-5",
		At:        time.Date(2026, time.January, 31, 23, 59, 59, 0, time.UTC),
		Quantity:  5,
	}
}

// wantJanuaryBoundaryBill 按“事件先接收 / 账单先生成”两种结果之一断言
// 一月账单的完整金额、用量与付款状态。
func wantJanuaryBoundaryBill(t *testing.T, b Bill, accepted bool) {
	t.Helper()
	want := struct {
		totalUsage, overageUnits, overageFee, tax, due int64
	}{8, 0, 0, 50, 1050}
	if accepted {
		// (1000 + 3*7) = 1021；1021*5% = 51.05 → 四舍五入 51；应付 1072。
		want = struct {
			totalUsage, overageUnits, overageFee, tax, due int64
		}{13, 3, 21, 51, 1072}
	}
	if b.AccountID != "u" || b.Period != jan(2026) {
		t.Fatalf("bill identity = %s/%s, want u/2026-01", b.AccountID, b.Period)
	}
	if b.Terms != boundaryTerms || b.IncludedUnits != boundaryTerms.IncludedUnits {
		t.Fatalf("bill terms = %+v included=%d, want snapshot %+v", b.Terms, b.IncludedUnits, boundaryTerms)
	}
	if b.TotalUsage != want.totalUsage || b.OverageUnits != want.overageUnits ||
		b.OverageFee != want.overageFee || b.MonthlyFee != 1000 ||
		b.Tax != want.tax || b.TotalDue != want.due {
		t.Fatalf("accepted=%v bill amounts/usage = usage %d overUnits %d fee %d overFee %d tax %d due %d, "+
			"want usage %d overUnits %d fee 1000 overFee %d tax %d due %d",
			accepted,
			b.TotalUsage, b.OverageUnits, b.MonthlyFee, b.OverageFee, b.Tax, b.TotalDue,
			want.totalUsage, want.overageUnits, want.overageFee, want.tax, want.due)
	}
	if b.Paid != 0 || b.Balance != want.due || b.Settled {
		t.Fatalf("bill payment state = paid %d balance %d settled %v, want 0/%d/false",
			b.Paid, b.Balance, b.Settled, want.due)
	}
	if !b.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("due at = %v, want 2026-02-08", b.DueAt)
	}
}

// assertJanuaryBoundaryState 断言查询用量、查询账单与账户状态展示相符数据，
// 并校验一月账单的幂等再出账沿用既有金额与用量。
func assertJanuaryBoundaryState(t *testing.T, s *Service, accepted bool) {
	t.Helper()
	wantUsage := int64(8)
	if accepted {
		wantUsage = 13
	}
	if u, err := s.MonthlyUsage("u", jan(2026)); err != nil || u.Total != wantUsage {
		t.Fatalf("january usage = %d (%v), want %d", u.Total, err, wantUsage)
	}
	got, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatalf("get january bill: %v", err)
	}
	wantJanuaryBoundaryBill(t, got, accepted)

	// 再次为一月出账：沿用已经生成的金额与用量，返回同一张账单。
	again, err := s.CreateBill("u", jan(2026))
	if err != nil || again != got {
		t.Fatalf("re-bill january not identical: %+v vs %+v err=%v", again, got, err)
	}

	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// 订阅不受出账影响；二月零点距一月账单截止（2/8）尚早，不停用。
	if !st.Subscribed || st.Suspended || st.CurrentTerms != boundaryTerms {
		t.Fatalf("account status = sub %v suspended %v terms %+v, want subscribed/not suspended/boundary terms",
			st.Subscribed, st.Suspended, st.CurrentTerms)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) ||
		st.MonthlyUsage[0].Total != wantUsage {
		t.Fatalf("status monthly usage = %+v, want january %d", st.MonthlyUsage, wantUsage)
	}
	if len(st.Bills) != 1 {
		t.Fatalf("status bills = %+v, want exactly one january bill", st.Bills)
	}
	sum := st.Bills[0]
	wantDue := int64(1050)
	if accepted {
		wantDue = 1072
	}
	if sum.Period != jan(2026) || sum.TotalDue != wantDue || sum.Paid != 0 ||
		sum.Balance != wantDue || sum.Settled {
		t.Fatalf("status bill summary = %+v, want january due %d unpaid", sum, wantDue)
	}
	if !sum.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("status bill due at = %v, want 2026-02-08", sum.DueAt)
	}
}

// TestLateUsageAcceptedBeforeJanuaryBill：补报事件先被接收，随后一月出账。
// 事件按发生时刻归入一月：本次新增成功，一月累计与账单总用量都为 13。
func TestLateUsageAcceptedBeforeJanuaryBill(t *testing.T) {
	s := newLateUsageScenario(t)
	ev := lateJanuaryEvent()

	r, err := s.RecordEvent(ev)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("late event before bill = %+v %v, want accepted into january", r, err)
	}

	b, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create january bill: %v", err)
	}
	wantJanuaryBoundaryBill(t, b, true)
	assertJanuaryBoundaryState(t, s, true)

	// 已接收事件在出账后原样重报：仍成功，但表示没有再次累计。
	replay, err := s.RecordEvent(ev)
	if err != nil || replay.Accepted || replay.Period != jan(2026) {
		t.Fatalf("identical replay after billed = %+v %v, want success not re-accumulated", replay, err)
	}
	assertJanuaryBoundaryState(t, s, true)
}

// TestJanuaryBillCreatedBeforeLateUsage：一月账单先生成，补报事件随后被拒。
// 返回现有的已出账错误，用量与账单金额均保持为出账时的确定结果。
func TestJanuaryBillCreatedBeforeLateUsage(t *testing.T) {
	s := newLateUsageScenario(t)
	ev := lateJanuaryEvent()

	b, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create january bill: %v", err)
	}
	wantJanuaryBoundaryBill(t, b, false)

	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("late event after bill = %v, want ErrMonthBilled", err)
	}
	assertJanuaryBoundaryState(t, s, false)

	// 被出账拒绝的事件原样再报仍失败：它不能变成“已接收事件的重报”
	// （重报会成功且 Accepted=false），也不得改动任何用量或金额。
	r, err := s.RecordEvent(ev)
	if !errors.Is(err, ErrMonthBilled) || r != (EventResult{}) {
		t.Fatalf("replay rejected event = %+v %v, want ErrMonthBilled with zero result", r, err)
	}
	assertJanuaryBoundaryState(t, s, false)
}

// TestLateUsageRacesJanuaryBill：两项操作经现有服务入口真正并发发起。
// 允许实际处理顺序任意，但每轮结束后世界必须恰好处于两种自洽结果之一：
// 要么“事件先成功 → 用量 13 / 账单 1072”，要么“账单先成功 → 事件被拒 /
// 用量 8 / 账单 1050”；绝不允许接收与计费互相错位。
func TestLateUsageRacesJanuaryBill(t *testing.T) {
	const iterations = 200
	var sawAccepted, sawRejected bool
	for i := 0; i < iterations; i++ {
		s := newLateUsageScenario(t)
		ev := lateJanuaryEvent()

		start := make(chan struct{})
		var wg sync.WaitGroup
		var (
			mu      sync.Mutex
			evRes   EventResult
			evErr   error
			billRes Bill
			billErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r, err := s.RecordEvent(ev)
			mu.Lock()
			evRes, evErr = r, err
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			<-start
			b, err := s.CreateBill("u", jan(2026))
			mu.Lock()
			billRes, billErr = b, err
			mu.Unlock()
		}()
		close(start)
		wg.Wait()

		if billErr != nil {
			t.Fatalf("iteration %d: create bill: %v", i, billErr)
		}
		switch {
		case evErr == nil && evRes.Accepted && evRes.Period == jan(2026):
			// 事件先被接收：账单必须为 13/1072，不能接收了却按 8 计费。
			sawAccepted = true
			wantJanuaryBoundaryBill(t, billRes, true)
			assertJanuaryBoundaryState(t, s, true)
			// 出账后的原样重报不再累计，账单金额不变。
			if r, err := s.RecordEvent(ev); err != nil || r.Accepted {
				t.Fatalf("iteration %d: replay accepted event = %+v %v, want dedup no-op", i, r, err)
			}
			assertJanuaryBoundaryState(t, s, true)
		case errors.Is(evErr, ErrMonthBilled) && evRes == (EventResult{}):
			// 账单先生成：事件被拒，账单必须为 8/1050，不能拒绝了却计入金额。
			sawRejected = true
			wantJanuaryBoundaryBill(t, billRes, false)
			assertJanuaryBoundaryState(t, s, false)
			// 被拒事件原样再报仍是已出账错误，不会变成成功的去重重报。
			if r, err := s.RecordEvent(ev); !errors.Is(err, ErrMonthBilled) || r != (EventResult{}) {
				t.Fatalf("iteration %d: replay rejected event = %+v %v, want ErrMonthBilled", i, r, err)
			}
			assertJanuaryBoundaryState(t, s, false)
		default:
			t.Fatalf("iteration %d: inconsistent race outcome event=%+v err=%v bill=%+v",
				i, evRes, evErr, billRes)
		}
	}
	// 并发顺序本身不可规定；两个确定性测试分别覆盖两条分支。
	// 若运行期调度恰好观察到两种顺序，记录下来便于判断本用例的实际覆盖面。
	if !sawAccepted || !sawRejected {
		t.Logf("race iterations observed accepted=%v rejected=%v; both branches are "+
			"also covered deterministically by the sequential tests", sawAccepted, sawRejected)
	}
}
