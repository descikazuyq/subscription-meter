package meter

import (
	"fmt"
	"sync"
	"testing"
)

// 本文件回归“为账户安排下月换套餐”与“修改目标套餐定义”同时发生时，
// 安排保存的套餐条件快照必须完整一致：月费、包含额度、超额单价、税率
// 四项必须同属安排被接受那一刻的同一份完整乙定义，不能各字段取自不同
// 版本，目标套餐标识与生效账期也必须保持一致。
//
// 场景（全部 UTC，时钟由测试注入，结果不依赖运行当天日期）：
//
//	账户自 2026-01-01 00:00 起使用甲套餐；2026-01-15 12:00 时尚无待生效
//	安排，也没有任何账单与欠费。
//	乙套餐原定义：月费 2000、包含 10、超额单价 100、税率 1000（10%）；
//	修改后定义：月费 3000、包含 20、超额单价 200、税率 600（6%）。
//	此时“安排二月改用乙”与“修改乙的上述四项条件”同时提交：
//	两项合法请求都必须成功，换套餐请求返回新建安排（Created=true），
//	目标仍为乙、生效账期为 2026-02，不能被推到三月。安排先被接受时
//	保存乙的原定义，修改先被接受时保存乙的新定义；允许任一种结果，
//	但四项条件不允许混用。
//
//	一月查询仍显示甲的完整条件，待生效安排与换套餐请求首次返回的内容
//	逐条一致。两项请求完成后，把乙再改成第三组四项全不相同的定义
//	（月费 4000、包含 30、超额单价 300、税率 800），并在二月到来前
//	再次选择乙：必须返回原安排（Created=false），不重新取价。
//
//	2026-02-01 00:00 直接查询：显示首次安排保存的整份乙条件，待生效
//	安排消失，后来修改的定义不能混入。二月累计 25 单位用量，二月结束
//	后出账：
//	  - 保存原定义：包含 10、超额 15、超额费 1500、税 350、应付 3850；
//	  - 保存新定义：包含 20、超额 5、超额费 1000、税 240、应付 4240。
//	账单采用的套餐条件必须与该账户的安排返回、二月实际生效条件三处
//	完全一致。整个过程账户没有到期欠款，二月用量正常接收。
//
// 实现上，SchedulePlanChange 在同一把互斥锁的一个临界区内读取套餐并以
// termsOf(plan) 整体落库为安排锁定快照（见 service.go 的
// SchedulePlanChange），UpdatePlan 对同一把锁整体替换套餐定义，两项
// 操作必然串行裁决，不存在字段级交错；生效月初 settleLocked 再把这份
// 完整快照原样追加进条件时间线，Status 与 CreateBill 都只经
// termsForPeriod 取同一份条件。本测试把这三层（安排返回、账户生效、
// 最终账单）的同源保证固定下来。并发用例不依赖调度顺序，另外用两个
// 确定顺序的用例分别锁住两种先后关系。

// changeRaceDefs 汇总本次争用涉及的各份定义：甲（一月当前套餐）、
// 乙的原定义、与安排同时提交的修改后定义、争用结束后的第三次定义、
// 以及出账后的第四次定义（后两份四项均与前两份全不相同，用于证明
// 任何后来的修改都不会混入）。
type changeRaceDefs struct {
	planA                             Plan
	oldB, updatedB, laterB, postBillB Plan
}

func newChangeRaceDefs() changeRaceDefs {
	return changeRaceDefs{
		planA:     planDef("a", 1000, 10, 100, 1000),
		oldB:      planDef("b", 2000, 10, 100, 1000),
		updatedB:  planDef("b", 3000, 20, 200, 600),
		laterB:    planDef("b", 4000, 30, 300, 800),
		postBillB: planDef("b", 5000, 40, 400, 500),
	}
}

// newChangeRaceService 在 2026-01-15 12:00 UTC（一月中旬）构造服务：
// 建好甲与按原定义存在的乙，账户自 2026-01-01 起使用甲，无待生效
// 安排、无账单、无欠费。
func newChangeRaceService(t *testing.T, acct string, d changeRaceDefs) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, d.planA)
	mustPlan(t, s, d.oldB)
	mustAccount(t, s, acct)
	mustSubscribe(t, s, acct, "a", utc(2026, 1, 1, 0, 0))
	return s, clk
}

// assertJanuaryBeforeContention 固定争用前的一月状态：订阅已生效、未停用、
// 当前是甲的完整条件、没有待生效安排、没有任何账单（因此不存在欠费）。
func assertJanuaryBeforeContention(t *testing.T, s *Service, acct string, d changeRaceDefs) {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q before contention: %v", acct, err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("%q subscribed=%v suspended=%v before contention", acct, st.Subscribed, st.Suspended)
	}
	if st.CurrentTerms != termsOf(d.planA) {
		t.Fatalf("%q current terms before contention = %+v, want whole plan-a %+v",
			acct, st.CurrentTerms, termsOf(d.planA))
	}
	if st.PendingChange != nil {
		t.Fatalf("%q unexpected pending change before contention: %+v", acct, st.PendingChange)
	}
	if len(st.Bills) != 0 {
		t.Fatalf("%q unexpected bills before contention: %+v", acct, st.Bills)
	}
}

// assertScheduledChangeIsCoherent 校验“安排二月改用乙”的成功返回本身：
// 新建安排、目标乙、生效账期为二月（不允许推到三月），四项条件整份等于
// 乙的原定义或修改后定义之一，不允许字段级混搭。返回安排保存的完整快照。
func assertScheduledChangeIsCoherent(t *testing.T, acct string, r PlanChangeResult, d changeRaceDefs) PlanTerms {
	t.Helper()
	oldTerms, updatedTerms := termsOf(d.oldB), termsOf(d.updatedB)
	if !r.Created {
		t.Fatalf("%q schedule must create a new arrangement, got %+v", acct, r)
	}
	if r.Change.TargetPlanID != "b" {
		t.Fatalf("%q target plan = %q, want b", acct, r.Change.TargetPlanID)
	}
	if r.Change.EffectivePeriod != feb(2026) {
		t.Fatalf("%q effective period = %s, want 2026-02 (must not slip to March)",
			acct, r.Change.EffectivePeriod)
	}
	got := r.Change.Terms
	if got.PlanID != "b" {
		t.Fatalf("%q terms plan id = %q, want b", acct, got.PlanID)
	}
	if got != oldTerms && got != updatedTerms {
		t.Fatalf("%q arrangement saved mixed terms %+v, want whole old %+v or whole updated %+v",
			acct, got, oldTerms, updatedTerms)
	}
	// 以月费为锚点逐项再核一遍，使字段级混搭在失败信息中无处遁形：
	// 月费取自哪一份定义，其余三项（含税率）也必须全部来自同一份。
	if got.MonthlyFee == oldTerms.MonthlyFee {
		if got != oldTerms {
			t.Fatalf("%q monthly fee from old definition but other fields not: %+v", acct, got)
		}
	} else if got != updatedTerms {
		t.Fatalf("%q terms neither fully old nor fully updated: %+v", acct, got)
	}
	return got
}

// assertJanuaryStateMatchesFirstReturn 校验一月查询与安排首次返回一致：
// 当前仍是甲的完整条件；待生效安排存在且与首次返回的安排逐字段相等；
// 重复查询结果稳定；账户未停用。
func assertJanuaryStateMatchesFirstReturn(t *testing.T, s *Service, acct string, first PlanChangeResult, d changeRaceDefs) {
	t.Helper()
	for i := 0; i < 2; i++ {
		st, err := s.Status(acct)
		if err != nil {
			t.Fatalf("status %q in January (check %d): %v", acct, i, err)
		}
		if !st.Subscribed || st.Suspended {
			t.Fatalf("%q subscribed=%v suspended=%v in January", acct, st.Subscribed, st.Suspended)
		}
		if st.CurrentTerms != termsOf(d.planA) {
			t.Fatalf("%q January current terms = %+v, want whole plan-a %+v",
				acct, st.CurrentTerms, termsOf(d.planA))
		}
		if st.PendingChange == nil {
			t.Fatalf("%q pending arrangement disappeared in January", acct)
		}
		if *st.PendingChange != first.Change {
			t.Fatalf("%q pending arrangement %+v != first schedule return %+v",
				acct, *st.PendingChange, first.Change)
		}
	}
}

// assertLaterUpdateAndReselectKeepsOriginal 覆盖：两项请求完成后再次修改
// 乙的四项计费条件，并在二月到来前再次选择乙——必须返回原安排
// （Created=false）、不重新取价，待生效安排与一月当前条件都不变。
func assertLaterUpdateAndReselectKeepsOriginal(t *testing.T, s *Service, acct string, first PlanChangeResult, d changeRaceDefs) {
	t.Helper()
	if err := s.UpdatePlan(d.laterB); err != nil {
		t.Fatalf("%q second plan update: %v", acct, err)
	}
	if got := s.Status2("b"); got != d.laterB {
		t.Fatalf("plan b current definition = %+v, want later %+v", got, d.laterB)
	}
	r := mustSchedule(t, s, acct, "b")
	if r.Created {
		t.Fatalf("%q reselecting same pending target created a new arrangement: %+v", acct, r)
	}
	if r.Change != first.Change {
		t.Fatalf("%q reselect repriced or rewrote arrangement:\ngot  %+v\nwant %+v",
			acct, r.Change, first.Change)
	}
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q after reselect: %v", acct, err)
	}
	if st.PendingChange == nil || *st.PendingChange != first.Change {
		t.Fatalf("%q pending after reselect = %+v, want %+v",
			acct, st.PendingChange, first.Change)
	}
	if st.CurrentTerms != termsOf(d.planA) {
		t.Fatalf("%q January terms changed while waiting: %+v", acct, st.CurrentTerms)
	}
}

// expectedFebruaryBill 按保存的整份乙定义给出二月 25 单位用量的期望账单。
type expectedFebruaryBill struct {
	includedUnits, overageUnits           int64
	monthlyFee, overageFee, tax, totalDue int64
}

func expectFebruaryBill(t *testing.T, acct string, saved PlanTerms, d changeRaceDefs) expectedFebruaryBill {
	t.Helper()
	switch saved {
	case termsOf(d.oldB):
		// 包含 10、超额 15×100=1500、(2000+1500)×10%=350、合计 3850。
		return expectedFebruaryBill{10, 15, 2000, 1500, 350, 3850}
	case termsOf(d.updatedB):
		// 包含 20、超额 5×200=1000、(3000+1000)×6%=240、合计 4240。
		return expectedFebruaryBill{20, 5, 3000, 1000, 240, 4240}
	default:
		t.Fatalf("%q saved terms %+v are neither the old nor the updated b definition", acct, saved)
		return expectedFebruaryBill{}
	}
}

// assertFebruaryGoesLiveAndBillMatches 覆盖三层一致性中的后两层与最终账单：
// 二月零点直接查询显示首次安排保存的整份乙条件、待生效安排消失（第三份
// 定义不混入）；二月接收 25 单位用量；二月结束后账单的条件、额度、月费、
// 超额单价、税率与保存的安排逐字段同源，金额恰为该份定义下的期望值；
// 出账后再改一次乙定义，账单与账户当前条件都不被改写。
func assertFebruaryGoesLiveAndBillMatches(t *testing.T, s *Service, clk *fakeClock, acct string, saved PlanTerms, d changeRaceDefs) {
	t.Helper()
	want := expectFebruaryBill(t, acct, saved, d)

	// 2026-02-01 00:00 UTC：无需先上报用量或出账，直接查询即生效。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q at February boundary: %v", acct, err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("%q subscribed=%v suspended=%v at February boundary", acct, st.Subscribed, st.Suspended)
	}
	if st.CurrentTerms != saved {
		t.Fatalf("%q effective February terms = %+v, want saved arrangement snapshot %+v",
			acct, st.CurrentTerms, saved)
	}
	if st.PendingChange != nil {
		t.Fatalf("%q pending arrangement must vanish in February, got %+v", acct, st.PendingChange)
	}
	// 乙的当前定义已是第三份，但账户生效条件必须仍是安排锁定的那一份。
	if got := s.Status2("b"); got != d.laterB {
		t.Fatalf("plan b current definition = %+v, want later definition %+v", got, d.laterB)
	}

	// 二月累计 25 单位用量：账户无到期欠款，事件必须正常接收。
	clk.t = utc(2026, 2, 10, 12, 0)
	r, err := s.RecordEvent(Event{
		AccountID: acct, EventID: "usage-feb",
		At: utc(2026, 2, 10, 0, 0), Quantity: 25,
	})
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("%q february usage: r=%+v err=%v", acct, r, err)
	}
	if u, err := s.MonthlyUsage(acct, feb(2026)); err != nil || u.Total != 25 {
		t.Fatalf("%q february usage = %+v err=%v, want total 25", acct, u, err)
	}

	// 二月账期结束后出账：条件必须与安排及二月生效条件为同一份完整定义。
	clk.t = utc(2026, 3, 1, 0, 0)
	bill := mustBill(t, s, acct, feb(2026))
	if bill.AccountID != acct || bill.Period != feb(2026) {
		t.Fatalf("%q bill identity = %s/%s", acct, bill.AccountID, bill.Period)
	}
	if bill.Terms != saved {
		t.Fatalf("%q bill terms = %+v, want saved snapshot %+v", acct, bill.Terms, saved)
	}
	if bill.TotalUsage != 25 ||
		bill.IncludedUnits != want.includedUnits ||
		bill.OverageUnits != want.overageUnits ||
		bill.MonthlyFee != want.monthlyFee ||
		bill.OverageFee != want.overageFee ||
		bill.Tax != want.tax ||
		bill.TotalDue != want.totalDue {
		t.Fatalf("%q bill numbers = usage %d included %d overageUnits %d fee %d overageFee %d tax %d totalDue %d, want %+v",
			acct, bill.TotalUsage, bill.IncludedUnits, bill.OverageUnits,
			bill.MonthlyFee, bill.OverageFee, bill.Tax, bill.TotalDue, want)
	}
	// 超额单价必须来自保存的快照：与超额费交叉印证，防止单价字段混搭。
	if bill.OverageFee != bill.OverageUnits*saved.OveragePrice {
		t.Fatalf("%q overage fee %d != units %d * saved overage price %d",
			acct, bill.OverageFee, bill.OverageUnits, saved.OveragePrice)
	}
	// 税率必须来自保存的快照：用账单月费与超额费重算税额交叉印证。
	if recomputed, ok := rateAmount(bill.MonthlyFee+bill.OverageFee, saved.TaxRateBasisPoints); !ok || recomputed != bill.Tax {
		t.Fatalf("%q tax %d != (fee %d + overageFee %d) at saved rate %d bps (got %d, ok=%v)",
			acct, bill.Tax, bill.MonthlyFee, bill.OverageFee, saved.TaxRateBasisPoints, recomputed, ok)
	}
	if bill.Balance != want.totalDue || bill.Paid != 0 || bill.Settled != (want.totalDue == 0) {
		t.Fatalf("%q bill payment state: paid=%d balance=%d settled=%v, want totalDue %d unpaid",
			acct, bill.Paid, bill.Balance, bill.Settled, want.totalDue)
	}
	if !bill.DueAt.Equal(feb(2026).End().AddDate(0, 0, 7)) {
		t.Fatalf("%q bill due at = %v, want 2026-03-08T00:00:00Z", acct, bill.DueAt)
	}

	// 出账后查询：账单固定不变；状态中的当前条件与账单条件同源；
	// 账单摘要进入账户状态且金额一致。
	got, err := s.GetBill(acct, feb(2026))
	if err != nil {
		t.Fatalf("get bill %q: %v", acct, err)
	}
	if got != bill {
		t.Fatalf("%q bill changed between create and get:\ncreate=%+v\nget   =%+v", acct, bill, got)
	}
	st, _ = s.Status(acct)
	if !st.Subscribed || st.CurrentTerms != saved {
		t.Fatalf("%q current terms after billing = %+v, want %+v", acct, st.CurrentTerms, saved)
	}
	var summary *BillSummary
	for i := range st.Bills {
		if st.Bills[i].Period == feb(2026) {
			summary = &st.Bills[i]
		}
	}
	if summary == nil {
		t.Fatalf("%q february bill missing from status summaries: %+v", acct, st.Bills)
	}
	if summary.TotalDue != want.totalDue || summary.Balance != want.totalDue || summary.Settled != false {
		t.Fatalf("%q status bill summary = %+v, want totalDue/balance %d unsettled",
			acct, summary, want.totalDue)
	}

	// 出账后再改一次乙的四项条件：已固定的账单与账户生效条件都不被改写。
	if err := s.UpdatePlan(d.postBillB); err != nil {
		t.Fatalf("%q post-billing plan update: %v", acct, err)
	}
	again, err := s.GetBill(acct, feb(2026))
	if err != nil {
		t.Fatalf("get bill after update %q: %v", acct, err)
	}
	if again != bill {
		t.Fatalf("%q bill changed after later plan update:\nbefore=%+v\nafter =%+v", acct, bill, again)
	}
	st, _ = s.Status(acct)
	if st.CurrentTerms != saved {
		t.Fatalf("%q effective terms rewritten by post-billing update: %+v, want %+v",
			acct, st.CurrentTerms, saved)
	}
}

// contendScheduleWithPlanUpdate 用启动屏障让“安排二月换乙”与“修改乙定义”
// 真正并发地（而非顺序提交）首次提交，返回安排结果与两项操作各自的错误。
func contendScheduleWithPlanUpdate(s *Service, acct string, updated Plan) (PlanChangeResult, error, error) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	var sched PlanChangeResult
	var schedErr, updErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		sched, schedErr = s.SchedulePlanChange(acct, updated.ID)
	}()
	go func() {
		defer wg.Done()
		<-start
		updErr = s.UpdatePlan(updated)
	}()
	close(start)
	wg.Wait()
	return sched, schedErr, updErr
}

// runFullLifecycle 从争用结束点开始，跑完一月一致性、再次改价后重选、
// 二月生效与二月出账的全部下游保障。
func runFullLifecycle(t *testing.T, s *Service, clk *fakeClock, acct string, first PlanChangeResult, d changeRaceDefs) PlanTerms {
	t.Helper()
	saved := assertScheduledChangeIsCoherent(t, acct, first, d)
	assertJanuaryStateMatchesFirstReturn(t, s, acct, first, d)
	assertLaterUpdateAndReselectKeepsOriginal(t, s, acct, first, d)
	assertFebruaryGoesLiveAndBillMatches(t, s, clk, acct, saved, d)
	return saved
}

// TestConcurrentSchedulePlanChangeAndPlanUpdateKeepsOneCompleteDefinition
// 并发提交“安排二月换乙”与“修改乙的四项条件”：每轮两项合法操作都必须
// 成功；安排保存的条件必须整份属于原定义或修改后定义；随后第三次改价、
// 二月前重选、二月零点生效、25 单位用量与三月一日出账全程沿用该份快照，
// 安排返回、账户生效条件与最终账单三处同源。并发调度不保证某种先后，
// 因此多轮运行，并由确定性顺序用例分别锁住两种结果。
func TestConcurrentSchedulePlanChangeAndPlanUpdateKeepsOneCompleteDefinition(t *testing.T) {
	const rounds = 100
	sawOld, sawUpdated := false, false
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("race-acct-%d", iter)
		d := newChangeRaceDefs()
		s, clk := newChangeRaceService(t, acct, d)
		assertJanuaryBeforeContention(t, s, acct, d)

		first, schedErr, updErr := contendScheduleWithPlanUpdate(s, acct, d.updatedB)
		if schedErr != nil {
			t.Fatalf("iter %d schedule plan change failed: %v", iter, schedErr)
		}
		if updErr != nil {
			t.Fatalf("iter %d plan update failed: %v", iter, updErr)
		}
		// 修改总是成功：争用结束后乙的当前定义必为修改后的定义。
		if got := s.Status2("b"); got != d.updatedB {
			t.Fatalf("iter %d plan b definition = %+v, want updated %+v", iter, got, d.updatedB)
		}

		saved := runFullLifecycle(t, s, clk, acct, first, d)
		switch saved {
		case termsOf(d.oldB):
			sawOld = true
		case termsOf(d.updatedB):
			sawUpdated = true
		}
	}
	// 提示性要求：若调度始终只给出一种顺序，确定性用例仍分别保障两种结果。
	if !sawOld || !sawUpdated {
		t.Logf("scheduler only exercised one order this run (old=%v updated=%v); deterministic subtests cover both",
			sawOld, sawUpdated)
	}
}

// TestSchedulePlanChangeAndPlanUpdateBothOrdersDeterministic 以确定顺序
// 锁住两种先后关系：安排先被接受时保存乙的原定义（二月账单 3850）；
// 修改先被接受时安排保存乙的新定义（二月账单 4240）。两种顺序下第三份
// 定义都不能混入，生效月份都必须仍是二月。
func TestSchedulePlanChangeAndPlanUpdateBothOrdersDeterministic(t *testing.T) {
	t.Run("schedule-accepted-first-keeps-old-definition", func(t *testing.T) {
		acct := "det-schedule-first"
		d := newChangeRaceDefs()
		s, clk := newChangeRaceService(t, acct, d)
		assertJanuaryBeforeContention(t, s, acct, d)

		// 顺序一：安排先被接受，当场锁定乙的原定义。
		first := mustSchedule(t, s, acct, "b")
		if !first.Created || first.Change.Terms != termsOf(d.oldB) {
			t.Fatalf("schedule first: %+v, want whole old b terms", first)
		}
		// 修改随后被接受：只影响之后新快照，已接受安排不被改写。
		if err := s.UpdatePlan(d.updatedB); err != nil {
			t.Fatalf("update after schedule: %v", err)
		}
		if got := s.Status2("b"); got != d.updatedB {
			t.Fatalf("plan b definition = %+v, want updated", got)
		}

		saved := runFullLifecycle(t, s, clk, acct, first, d)
		if saved != termsOf(d.oldB) {
			t.Fatalf("schedule-first must keep old definition, got %+v", saved)
		}
	})

	t.Run("update-accepted-first-keeps-updated-definition", func(t *testing.T) {
		acct := "det-update-first"
		d := newChangeRaceDefs()
		s, clk := newChangeRaceService(t, acct, d)
		assertJanuaryBeforeContention(t, s, acct, d)

		// 顺序二：修改先被接受，乙的定义已换成新值。
		if err := s.UpdatePlan(d.updatedB); err != nil {
			t.Fatalf("update before schedule: %v", err)
		}
		if got := s.Status2("b"); got != d.updatedB {
			t.Fatalf("plan b definition = %+v, want updated", got)
		}
		// 安排随后被接受：必须按修改后的完整定义锁定快照，生效月仍为二月。
		first := mustSchedule(t, s, acct, "b")

		saved := runFullLifecycle(t, s, clk, acct, first, d)
		if saved != termsOf(d.updatedB) {
			t.Fatalf("update-first must keep updated definition, got %+v", saved)
		}
	})
}
