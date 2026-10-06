package meter

import (
	"fmt"
	"sync"
	"testing"
)

// 本文件回归“为账户安排下月换套餐与修改目标套餐定义同时提交”时，安排保存的
// 套餐条件快照必须完整一致：月费、包含额度、超额单价、税率四项必须同属安排
// 被接受那一刻的同一份完整乙定义，不能各字段取自不同版本，目标套餐标识也
// 必须保持一致。回归保障贯穿三处可见条件，并要求它们始终同源：
//   - 安排返回（以及等待期 Status 中的待生效安排）；
//   - 二月月初零点后账户实际生效的当前条件；
//   - 二月结束后生成的账单所采用的计费条件与金额。
//
// 场景（全部 UTC，不依赖运行当天日期，时钟由测试显式推进）：
//
//	账户自 2026-01-01 00:00 起使用甲套餐（月费 1000、包含 10、超额单价
//	100、无税）；2026-01-15 时尚无待生效安排，也没有任何到期欠款。
//	乙套餐原定义：月费 2000、包含 10、超额单价 100、税率 1000（10%）；
//	修改后定义：月费 3000、包含 20、超额单价 200、税率 600（6%）。
//	此时“为账户安排二月改用乙”与“修改乙的上述四项条件”同时提交：
//	两项合法请求都必须成功，换套餐请求必须返回新建安排（Created=true），
//	目标仍为乙，生效账期为二月（不得推到三月）；安排先被接受时保存乙的
//	原定义，修改先被接受时保存乙的新定义，允许任一种结果，但四项条件
//	不允许混用。
//
//	两项请求完成后再次修改乙的四项条件（4000/40/400/8%），并在二月到来
//	前再次选择乙：必须返回原安排（Created=false），不重新取价。到达
//	2026-02-01 00:00 UTC 直接查询，应显示首次安排保存的整份乙条件，
//	待生效安排消失，后来修改的定义不能混入。
//
//	账户二月累计 25 单位用量，二月结束后出账：
//	  - 保存原定义：包含 10、超额 15、超额费 1500、税 350、应付 3850；
//	  - 保存新定义：包含 20、超额 5、超额费 1000、税 240、应付 4240。
//	账单采用的套餐条件必须与该账户的安排及二月生效条件为同一份完整定义。
//
// 实现上，SchedulePlanChange 在同一把互斥锁的一个临界区内读取套餐并以
// termsOf(plan) 整体落库为安排快照（见 service.go 的 SchedulePlanChange），
// UpdatePlan 对同一把锁整体替换套餐定义，两项操作必然串行裁决，不存在
// 字段级交错；生效时 settleLocked 再把该快照整体追加进条件时间线，
// Status 与 CreateBill 都只经 termsForPeriod 取条件。本测试把这一保证
// 固定下来。并发用例不依赖调度顺序，另外用两个确定顺序的子用例分别
// 锁住两种先后关系。

// planChangeRaceDefs 汇总一次“安排换套餐与改价争用”涉及的各份定义：
// 甲（一月当前套餐）、乙的原定义、争用同时提交的修改后定义、争用结束后
// 的第三次定义（四项与前两份乙定义全不相同）。
type planChangeRaceDefs struct {
	jiaDef                       Plan
	oldDef, updatedDef, laterDef Plan
}

func newPlanChangeRaceDefs(jia, yi string) planChangeRaceDefs {
	return planChangeRaceDefs{
		jiaDef:     planDef(jia, 1000, 10, 100, 0),
		oldDef:     planDef(yi, 2000, 10, 100, 1000),
		updatedDef: planDef(yi, 3000, 20, 200, 600),
		laterDef:   planDef(yi, 4000, 40, 400, 800),
	}
}

// newPlanChangeRaceService 在 2026-01-15 12:00 UTC 构造服务：按原定义
// 建好甲、乙两个套餐，账户自 2026-01-01 起使用甲；断言前提——一月中旬
// 已生效、无待生效安排、无账单无欠费、当前条件是完整的甲定义。
func newPlanChangeRaceService(t *testing.T, acct string, d planChangeRaceDefs) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, d.jiaDef)
	mustPlan(t, s, d.oldDef)
	mustAccount(t, s, acct)
	mustSubscribe(t, s, acct, d.jiaDef.ID, utc(2026, 1, 1, 0, 0))

	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("baseline status %q: %v", acct, err)
	}
	if !st.Subscribed || st.Suspended || st.PendingChange != nil || len(st.Bills) != 0 {
		t.Fatalf("%q baseline state: %+v", acct, st)
	}
	if st.CurrentTerms != termsOf(d.jiaDef) {
		t.Fatalf("%q baseline current terms = %+v, want whole jia %+v",
			acct, st.CurrentTerms, termsOf(d.jiaDef))
	}
	return s, clk
}

// contendScheduleWithPlanUpdate 用启动屏障让“安排二月改用乙”与“修改乙
// 定义”真正并发地（而非顺序提交）首次提交，返回安排结果与两项操作的错误。
func contendScheduleWithPlanUpdate(s *Service, acct string, updated Plan) (PlanChangeResult, error, error) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	var r PlanChangeResult
	var schedErr, updErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		r, schedErr = s.SchedulePlanChange(acct, updated.ID)
	}()
	go func() {
		defer wg.Done()
		<-start
		updErr = s.UpdatePlan(updated)
	}()
	close(start)
	wg.Wait()
	return r, schedErr, updErr
}

// assertScheduleCoherent 核对换套餐请求的首次返回与一月查询状态：
// 必须是新建安排，目标仍为乙，生效账期为二月（不得推到三月）；安排锁定
// 的四项条件必须整份等于乙的原定义或修改后定义，不允许字段级混搭；
// 一月查询仍显示甲的完整条件，待生效安排与首次返回逐字段一致，账户无欠费。
// 返回安排保存的那一份完整乙条件。
func assertScheduleCoherent(t *testing.T, s *Service, acct string, r PlanChangeResult, d planChangeRaceDefs) PlanTerms {
	t.Helper()
	oldTerms, updatedTerms := termsOf(d.oldDef), termsOf(d.updatedDef)

	if !r.Created {
		t.Fatalf("%q schedule must create a new change: %+v", acct, r)
	}
	if r.Change.TargetPlanID != d.oldDef.ID {
		t.Fatalf("%q target plan = %q, want %q", acct, r.Change.TargetPlanID, d.oldDef.ID)
	}
	if r.Change.EffectivePeriod != feb(2026) {
		t.Fatalf("%q effective period = %s, want 2026-02 (must not slip to March)",
			acct, r.Change.EffectivePeriod)
	}
	saved := r.Change.Terms
	if saved.PlanID != d.oldDef.ID {
		t.Fatalf("%q locked terms plan id = %q, want %q", acct, saved.PlanID, d.oldDef.ID)
	}
	if saved != oldTerms && saved != updatedTerms {
		t.Fatalf("%q schedule locked mixed terms %+v, want whole old %+v or whole updated %+v",
			acct, saved, oldTerms, updatedTerms)
	}
	// 逐项再核一遍，让字段级混搭在失败信息中无处遁形：任一字段取自哪一版，
	// 其余三项（含税率）也必须来自同一版完整定义。
	if saved.MonthlyFee == oldTerms.MonthlyFee {
		if saved != oldTerms {
			t.Fatalf("%q monthly fee from old definition but other fields not all old: %+v",
				acct, saved)
		}
	} else if saved != updatedTerms {
		t.Fatalf("%q terms neither fully old nor fully updated: %+v", acct, saved)
	}

	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q after contention: %v", acct, err)
	}
	if !st.Subscribed {
		t.Fatalf("%q subscription not active in January: %+v", acct, st)
	}
	if st.Suspended {
		t.Fatalf("%q suspended without any overdue debt: %+v", acct, st)
	}
	// 一月查询仍显示甲的完整条件：四项都不能被乙的任一版定义替换。
	if st.CurrentTerms != termsOf(d.jiaDef) {
		t.Fatalf("%q january current terms = %+v, want whole jia %+v",
			acct, st.CurrentTerms, termsOf(d.jiaDef))
	}
	// 待生效安排与换套餐请求的首次返回完全一致。
	if st.PendingChange == nil {
		t.Fatalf("%q pending change missing after schedule", acct)
	}
	if *st.PendingChange != r.Change {
		t.Fatalf("%q pending change = %+v, want first schedule result %+v",
			acct, *st.PendingChange, r.Change)
	}
	return saved
}

// assertLaterUpdateAndReselectKeepsChange 覆盖“两项请求完成后”的后续约定：
// 再次修改乙的四项计费条件，并在二月到来前再次选择乙——必须返回原安排，
// Created=false，不重新取价，生效月份仍为二月；一月当前条件仍是甲。
func assertLaterUpdateAndReselectKeepsChange(t *testing.T, s *Service, acct string, first PlanChange, d planChangeRaceDefs) {
	t.Helper()
	if err := s.UpdatePlan(d.laterDef); err != nil {
		t.Fatalf("%q second yi update: %v", acct, err)
	}
	if got := s.Status2(d.oldDef.ID); got != d.laterDef {
		t.Fatalf("%q yi current definition = %+v, want later %+v", acct, got, d.laterDef)
	}

	// 仍在一月（2026-01-15），二月到来前再次选择乙：重报原安排。
	r2, err := s.SchedulePlanChange(acct, d.oldDef.ID)
	if err != nil {
		t.Fatalf("%q reselect yi before February: %v", acct, err)
	}
	if r2.Created || r2.Change != first {
		t.Fatalf("%q reselect re-priced the change: created=%v change=%+v, want original %+v",
			acct, r2.Created, r2.Change, first)
	}
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q after reselect: %v", acct, err)
	}
	if st.PendingChange == nil || *st.PendingChange != first {
		t.Fatalf("%q pending change rewritten by reselect: %+v, want %+v",
			acct, st.PendingChange, first)
	}
	if st.CurrentTerms != termsOf(d.jiaDef) || st.Suspended {
		t.Fatalf("%q january state after reselect = %+v, want whole jia and no suspension",
			acct, st)
	}
}

// assertFebruaryActivationAndBill 覆盖二月生效与最终账单：2026-02-01 零点
// UTC 直接查询必须显示首次安排保存的整份乙条件、待生效安排消失；账户始终
// 无到期欠款，二月 25 单位用量正常接收；二月结束后账单的条件与金额必须
// 与安排及二月生效条件同源（原定义 3850 / 新定义 4240），此后再改价也
// 不改变已生效条件与已出账单。
func assertFebruaryActivationAndBill(t *testing.T, s *Service, clk *fakeClock, acct string, first PlanChange, d planChangeRaceDefs) {
	t.Helper()
	saved := first.Terms
	oldTerms, updatedTerms, laterTerms := termsOf(d.oldDef), termsOf(d.updatedDef), termsOf(d.laterDef)

	// 到达 2026-02-01 00:00 UTC：直接查询即生效，无需先上报用量或出账。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q at February boundary: %v", acct, err)
	}
	if !st.Subscribed || st.PendingChange != nil {
		t.Fatalf("%q february activation state: %+v", acct, st)
	}
	if st.CurrentTerms != saved {
		t.Fatalf("%q february effective terms = %+v, want saved snapshot %+v",
			acct, st.CurrentTerms, saved)
	}
	if st.CurrentTerms == laterTerms {
		t.Fatalf("%q february terms leaked the later definition: %+v", acct, st.CurrentTerms)
	}

	// 二月累计 25 单位用量：账户没有任何到期欠款，用量必须正常接收。
	clk.t = utc(2026, 2, 10, 12, 0)
	if st, _ = s.Status(acct); st.Suspended {
		t.Fatalf("%q suspended despite no overdue debt: %+v", acct, st)
	}
	ev, err := s.RecordEvent(Event{
		AccountID: acct, EventID: "usage-feb",
		At: utc(2026, 2, 10, 0, 0), Quantity: 25,
	})
	if err != nil || !ev.Accepted || ev.Period != feb(2026) {
		t.Fatalf("%q february usage: ev=%+v err=%v", acct, ev, err)
	}
	if u, err := s.MonthlyUsage(acct, feb(2026)); err != nil || u.Total != 25 {
		t.Fatalf("%q february total usage = %+v err=%v, want 25", acct, u, err)
	}

	// 二月账期结束后生成账单。
	clk.t = utc(2026, 3, 1, 0, 0)
	bill := mustBill(t, s, acct, feb(2026))

	type wantFebBill struct {
		included, overageUnits, overageFee, tax, totalDue int64
	}
	var want wantFebBill
	switch saved {
	case oldTerms:
		want = wantFebBill{included: 10, overageUnits: 15, overageFee: 1500, tax: 350, totalDue: 3850}
	case updatedTerms:
		want = wantFebBill{included: 20, overageUnits: 5, overageFee: 1000, tax: 240, totalDue: 4240}
	default:
		t.Fatalf("%q saved terms %+v are neither the old nor the updated definition", acct, saved)
	}

	// 三方一致：账单条件 == 安排锁定条件 == 账户二月实际生效条件。
	if bill.Terms != first.Terms {
		t.Fatalf("%q bill terms %+v != scheduled change terms %+v", acct, bill.Terms, first.Terms)
	}
	st, _ = s.Status(acct)
	if st.CurrentTerms != bill.Terms {
		t.Fatalf("%q effective terms %+v != bill terms %+v", acct, st.CurrentTerms, bill.Terms)
	}
	if bill.AccountID != acct || bill.Period != feb(2026) || bill.TotalUsage != 25 {
		t.Fatalf("%q bill identity or usage: %+v", acct, bill)
	}
	if bill.IncludedUnits != want.included || bill.OverageUnits != want.overageUnits ||
		bill.MonthlyFee != saved.MonthlyFee || bill.OverageFee != want.overageFee ||
		bill.Tax != want.tax || bill.TotalDue != want.totalDue ||
		bill.Paid != 0 || bill.Balance != want.totalDue || bill.Settled {
		t.Fatalf("%q february bill amounts = included %d overageUnits %d monthlyFee %d overageFee %d tax %d totalDue %d paid %d balance %d settled=%v, want %+v",
			acct, bill.IncludedUnits, bill.OverageUnits, bill.MonthlyFee, bill.OverageFee,
			bill.Tax, bill.TotalDue, bill.Paid, bill.Balance, bill.Settled, want)
	}
	// 超额单价交叉印证：必须与账单其他字段来自同一份保存定义。
	if bill.OverageFee != bill.OverageUnits*saved.OveragePrice {
		t.Fatalf("%q overage fee %d != %d units * saved overage price %d",
			acct, bill.OverageFee, bill.OverageUnits, saved.OveragePrice)
	}
	// 税率交叉印证：(月费+超额费) 按保存的税率四舍五入必须恰为账单税额。
	if tax, ok := rateAmount(bill.MonthlyFee+bill.OverageFee, saved.TaxRateBasisPoints); !ok || tax != bill.Tax {
		t.Fatalf("%q tax %d not computed from saved tax rate %d bp",
			acct, bill.Tax, saved.TaxRateBasisPoints)
	}
	if !bill.DueAt.Equal(feb(2026).End().AddDate(0, 0, 7)) {
		t.Fatalf("%q bill due at %v, want 2026-03-08T00:00:00Z", acct, bill.DueAt)
	}

	// 账单一经生成即固定：重复取得同一张。
	if got, err := s.GetBill(acct, feb(2026)); err != nil || got != bill {
		t.Fatalf("%q bill not fixed after creation: got=%+v err=%v", acct, got, err)
	}

	// 出账后再改一次乙的定义（第四组，四项仍全不同）：已追加到时间线的
	// 生效条件与已生成的账单都不被改写。
	if err := s.UpdatePlan(planDef(d.oldDef.ID, 5000, 50, 500, 900)); err != nil {
		t.Fatalf("%q post-billing yi update: %v", acct, err)
	}
	if got, _ := s.GetBill(acct, feb(2026)); got != bill {
		t.Fatalf("%q bill rewritten by later plan update:\nbefore=%+v\nafter =%+v", acct, bill, got)
	}
	st, _ = s.Status(acct)
	if st.CurrentTerms != saved || st.PendingChange != nil {
		t.Fatalf("%q effective terms rewritten after billing: current=%+v pending=%+v, want %+v",
			acct, st.CurrentTerms, st.PendingChange, saved)
	}
}

// TestConcurrentSchedulePlanChangeAndPlanUpdateKeepsOneCompleteDefinition
// 并发提交“安排二月改用乙”与“修改乙定义”：每轮两项操作都必须成功；
// 安排必须是新建的二月生效安排，锁定的四项条件整份属于原定义或修改后
// 定义；随后第三次改价、二月前重选乙不重新取价、二月零点生效、25 单位
// 用量与三月一日出账全程沿用该份快照。并发调度不保证某种先后，因此多轮
// 运行，并由确定性顺序子用例分别锁住两种结果。
func TestConcurrentSchedulePlanChangeAndPlanUpdateKeepsOneCompleteDefinition(t *testing.T) {
	const rounds = 100
	sawOld, sawUpdated := false, false
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("race-acct-%d", iter)
		d := newPlanChangeRaceDefs(fmt.Sprintf("race-jia-%d", iter), fmt.Sprintf("race-yi-%d", iter))
		s, clk := newPlanChangeRaceService(t, acct, d)

		r, schedErr, updErr := contendScheduleWithPlanUpdate(s, acct, d.updatedDef)
		if schedErr != nil {
			t.Fatalf("iter %d schedule failed: %v", iter, schedErr)
		}
		if updErr != nil {
			t.Fatalf("iter %d plan update failed: %v", iter, updErr)
		}
		// 修改总是成功：争用结束后乙的当前定义必为修改后的定义。
		if got := s.Status2(d.oldDef.ID); got != d.updatedDef {
			t.Fatalf("iter %d yi current definition = %+v, want updated %+v", iter, got, d.updatedDef)
		}

		saved := assertScheduleCoherent(t, s, acct, r, d)
		switch saved {
		case termsOf(d.oldDef):
			sawOld = true
		case termsOf(d.updatedDef):
			sawUpdated = true
		}

		assertLaterUpdateAndReselectKeepsChange(t, s, acct, r.Change, d)
		assertFebruaryActivationAndBill(t, s, clk, acct, r.Change, d)
	}
	// 提示性要求：若调度始终只给出一种顺序，确定顺序子用例仍分别保障两种结果。
	if !sawOld || !sawUpdated {
		t.Logf("scheduler only exercised one order this run (old=%v updated=%v); deterministic subtests cover both",
			sawOld, sawUpdated)
	}
}

// TestSchedulePlanChangeAndPlanUpdateBothOrdersDeterministic 以确定顺序
// 锁住两种先后关系：安排先被接受时保存乙的原定义（二月账单 3850）；
// 修改先被接受时安排保存乙的新定义（二月账单 4240）。两种顺序下第三次
// 改价都不能混入，重选乙都不重新取价。
func TestSchedulePlanChangeAndPlanUpdateBothOrdersDeterministic(t *testing.T) {
	t.Run("schedule-accepted-first-keeps-old-definition", func(t *testing.T) {
		acct := "det-sched-first"
		d := newPlanChangeRaceDefs("det-sched-first-jia", "det-sched-first-yi")
		s, clk := newPlanChangeRaceService(t, acct, d)

		// 顺序一：安排先被接受，当场锁定乙的原定义（2000/10/100/10%）。
		r := mustSchedule(t, s, acct, d.oldDef.ID)
		if !r.Created || r.Change.TargetPlanID != d.oldDef.ID ||
			r.Change.EffectivePeriod != feb(2026) || r.Change.Terms != termsOf(d.oldDef) {
			t.Fatalf("schedule accepted first: %+v", r)
		}
		// 修改随后被接受：只影响之后新快照，已接受安排保持原定义。
		if err := s.UpdatePlan(d.updatedDef); err != nil {
			t.Fatalf("update after schedule: %v", err)
		}
		if got := s.Status2(d.oldDef.ID); got != d.updatedDef {
			t.Fatalf("yi definition = %+v, want updated", got)
		}
		saved := assertScheduleCoherent(t, s, acct, r, d)
		if saved != termsOf(d.oldDef) {
			t.Fatalf("schedule-first must keep old definition, got %+v", saved)
		}

		assertLaterUpdateAndReselectKeepsChange(t, s, acct, r.Change, d)
		assertFebruaryActivationAndBill(t, s, clk, acct, r.Change, d)
	})

	t.Run("update-accepted-first-keeps-updated-definition", func(t *testing.T) {
		acct := "det-upd-first"
		d := newPlanChangeRaceDefs("det-upd-first-jia", "det-upd-first-yi")
		s, clk := newPlanChangeRaceService(t, acct, d)

		// 顺序二：修改先被接受，乙的定义已整体换成新值。
		if err := s.UpdatePlan(d.updatedDef); err != nil {
			t.Fatalf("update before schedule: %v", err)
		}
		if got := s.Status2(d.oldDef.ID); got != d.updatedDef {
			t.Fatalf("yi definition = %+v, want updated", got)
		}
		// 安排随后被接受：必须按修改后的完整定义（3000/20/200/6%）锁定。
		r := mustSchedule(t, s, acct, d.oldDef.ID)
		if !r.Created || r.Change.TargetPlanID != d.oldDef.ID ||
			r.Change.EffectivePeriod != feb(2026) || r.Change.Terms != termsOf(d.updatedDef) {
			t.Fatalf("schedule accepted after update: %+v", r)
		}
		saved := assertScheduleCoherent(t, s, acct, r, d)
		if saved != termsOf(d.updatedDef) {
			t.Fatalf("update-first must keep updated definition, got %+v", saved)
		}

		assertLaterUpdateAndReselectKeepsChange(t, s, acct, r.Change, d)
		assertFebruaryActivationAndBill(t, s, clk, acct, r.Change, d)
	})
}
