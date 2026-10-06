package meter

import (
	"fmt"
	"sync"
	"testing"
)

// 本文件回归“修改套餐定义与为账户开通订阅同时提交”时，账户保存的套餐条件
// 快照必须完整一致：月费、包含额度、超额单价、税率四项必须同属开通被接受
// 那一刻的同一份完整定义，不能各字段取自不同版本，套餐标识也必须保持一致。
//
// 场景（全部 UTC）：
//
//	套餐 p 原定义：月费 1000、包含 10、超额单价 100、税率 1000（10%）。
//	修改后定义：月费 2000、包含 20、超额单价 200、税率 600（6%）。
//	账户已建立、无账单无欠费、尚未开通订阅；2026 年 1 月中旬登记自
//	2026-01-01 00:00 起开通（登记时订阅已到实际开通时刻，查询即生效）。
//	“修改 p 的定义”与“为该账户开通 p 的订阅”同时提交：两项操作都必须
//	成功，开通先被接受时账户保存原定义，修改先被接受时账户保存修改后的
//	定义；允许任一种顺序，不要求某一项必然先成功，但四项条件必须整份
//	同源。
//
//	随后把 p 再改成另一组四项全不相同的有效定义（月费 3000、包含 30、
//	超额单价 300、税率 800），账户查询与一月出账都必须沿用开通取得的
//	整份快照，后来的改价不能混入。一月接收 25 单位用量，2026-02-01
//	00:00 为一月出账：
//	  - 保存原定义：超额 15、超额费 1500、税 250、应付 2750；
//	  - 保存修改后定义：超额 5、超额费 1000、税 180、应付 3180。
//
// 实现上，Subscribe 在同一把互斥锁的一个临界区内读取套餐并以
// termsOf(plan) 整体落库为开通快照（见 service.go 的 Subscribe），
// UpdatePlan 对同一把锁整体替换套餐定义，两项操作必然串行裁决，不存在
// 字段级交错；本测试把这一保证固定下来。并发用例不依赖调度顺序，
// 另外用两个确定顺序的用例分别锁住两种先后关系。

// activationDefs 汇总一次“开通与改价争用”涉及的三份定义：原定义、
// 同时提交的修改后定义、争用结束后的第三次定义（四项与前两份全不相同）。
type activationDefs struct {
	planID                       string
	oldDef, updatedDef, laterDef Plan
}

func newActivationDefs(planID string) activationDefs {
	return activationDefs{
		planID:     planID,
		oldDef:     planDef(planID, 1000, 10, 100, 1000),
		updatedDef: planDef(planID, 2000, 20, 200, 600),
		laterDef:   planDef(planID, 3000, 30, 300, 800),
	}
}

// newActivationRaceService 在 2026-01-15 12:00 UTC（一月中旬）构造服务，
// 建好按原定义存在的套餐 d.planID 与一个尚无订阅、无账单无欠费的账户。
func newActivationRaceService(t *testing.T, acct string, d activationDefs) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, d.oldDef)
	mustAccount(t, s, acct)
	return s, clk
}

// contendActivationWithPlanUpdate 用启动屏障让“开通订阅”与“修改套餐定义”
// 真正并发地（而非顺序提交）同时首次提交，返回两项操作各自的错误。
func contendActivationWithPlanUpdate(s *Service, acct string, updated Plan) (subErr, updErr error) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		subErr = s.Subscribe(acct, updated.ID, utc(2026, 1, 1, 0, 0))
	}()
	go func() {
		defer wg.Done()
		<-start
		updErr = s.UpdatePlan(updated)
	}()
	close(start)
	wg.Wait()
	return subErr, updErr
}

// assertActivationSnapshotCoherent 在两项操作争用结束后查询账户：订阅必须
// 已生效；当前套餐标识必须仍是该套餐；四项条件必须整份等于原定义或修改后
// 定义中的恰好一份（不允许字段级混搭）。返回账户保存的那一份完整快照。
func assertActivationSnapshotCoherent(t *testing.T, s *Service, acct string, d activationDefs) PlanTerms {
	t.Helper()
	oldTerms, updatedTerms := termsOf(d.oldDef), termsOf(d.updatedDef)

	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	if !st.Subscribed {
		t.Fatalf("%q subscription not active after race: %+v", acct, st)
	}
	got := st.CurrentTerms
	if got.PlanID != d.planID {
		t.Fatalf("%q plan id = %q, want %q", acct, got.PlanID, d.planID)
	}
	if got != oldTerms && got != updatedTerms {
		t.Fatalf("%q saved mixed terms %+v, want whole old %+v or whole updated %+v",
			acct, got, oldTerms, updatedTerms)
	}
	// 逐项再核一遍，使字段级混搭在失败信息中无处遁形：四个计费字段必须
	// 同时来自同一份定义。
	if got.MonthlyFee == oldTerms.MonthlyFee {
		if got != oldTerms {
			t.Fatalf("%q monthly fee from old but other fields not: %+v", acct, got)
		}
	} else {
		if got != updatedTerms {
			t.Fatalf("%q terms neither fully old nor fully updated: %+v", acct, got)
		}
	}
	return got
}

// assertLaterPlanUpdateDoesNotRewriteSnapshot 把套餐改成第三组四项全不相同的
// 有效条件，然后确认账户保存的开通快照原样保留，套餐当前定义则确实已变。
func assertLaterPlanUpdateDoesNotRewriteSnapshot(t *testing.T, s *Service, acct string, saved PlanTerms, d activationDefs) {
	t.Helper()
	if err := s.UpdatePlan(d.laterDef); err != nil {
		t.Fatalf("second plan update: %v", err)
	}
	if got := s.Status2(d.planID); got != d.laterDef {
		t.Fatalf("plan current definition = %+v, want later %+v", got, d.laterDef)
	}
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q after later update: %v", acct, err)
	}
	if !st.Subscribed || st.CurrentTerms != saved {
		t.Fatalf("%q snapshot rewritten by later plan update: %+v, want %+v",
			acct, st.CurrentTerms, saved)
	}
}

// assertJanuaryBillUsesSavedSnapshot 在一月接收 25 单位用量并推进到
// 2026-02-01 00:00 UTC 为一月出账，断言账单的套餐条件、包含额度、月费与
// 账户开通保存的整份快照一一对应，账单总用量仍是 25，且金额恰为该份定义
// 下的期望值（原定义 2750，修改后定义 3180）；随后再次改价也不改变账单。
func assertJanuaryBillUsesSavedSnapshot(t *testing.T, s *Service, clk *fakeClock, acct string, saved PlanTerms, d activationDefs) {
	t.Helper()
	oldTerms, updatedTerms := termsOf(d.oldDef), termsOf(d.updatedDef)

	// 一月接收 25 单位用量（开通自 1 月 1 日起，事件落在订阅期间内）。
	clk.t = utc(2026, 1, 20, 12, 0)
	r, err := s.RecordEvent(Event{
		AccountID: acct, EventID: "usage-jan",
		At: utc(2026, 1, 20, 0, 0), Quantity: 25,
	})
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("%q jan usage: r=%+v err=%v", acct, r, err)
	}
	if u, err := s.MonthlyUsage(acct, jan(2026)); err != nil || u.Total != 25 {
		t.Fatalf("%q jan usage = %+v err=%v, want total 25", acct, u, err)
	}

	// 2026-02-01 00:00 UTC：一月账期结束，直接出账。
	clk.t = utc(2026, 2, 1, 0, 0)
	st, err := s.Status(acct)
	if err != nil || !st.Subscribed || st.CurrentTerms != saved {
		t.Fatalf("%q status at billing: st=%+v err=%v", acct, st, err)
	}
	bill := mustBill(t, s, acct, jan(2026))

	// 期望金额只可能是两份完整定义之一对应的结果。
	type wantJanBill struct {
		overageUnits, overageFee, tax, totalDue int64
	}
	var want wantJanBill
	switch saved {
	case oldTerms:
		want = wantJanBill{overageUnits: 15, overageFee: 1500, tax: 250, totalDue: 2750}
	case updatedTerms:
		want = wantJanBill{overageUnits: 5, overageFee: 1000, tax: 180, totalDue: 3180}
	default:
		t.Fatalf("%q saved terms %+v are neither old nor updated definition", acct, saved)
	}

	// 账单里的套餐条件必须整份对应账户开通结果，包含额度与月费也要对应；
	// 第三组定义的任何字段都不能混入。
	if bill.AccountID != acct || bill.Period != jan(2026) {
		t.Fatalf("%q bill identity = %s/%s", acct, bill.AccountID, bill.Period)
	}
	if bill.Terms != saved {
		t.Fatalf("%q bill terms = %+v, want saved snapshot %+v", acct, bill.Terms, saved)
	}
	if bill.MonthlyFee != saved.MonthlyFee || bill.IncludedUnits != saved.IncludedUnits {
		t.Fatalf("%q bill fee %d / included %d not from saved snapshot (fee %d / included %d)",
			acct, bill.MonthlyFee, bill.IncludedUnits, saved.MonthlyFee, saved.IncludedUnits)
	}
	if bill.TotalUsage != 25 {
		t.Fatalf("%q bill total usage = %d, want 25", acct, bill.TotalUsage)
	}
	if bill.OverageUnits != want.overageUnits || bill.OverageFee != want.overageFee ||
		bill.Tax != want.tax || bill.TotalDue != want.totalDue {
		t.Fatalf("%q bill amounts = overageUnits %d overageFee %d tax %d totalDue %d, want %+v",
			acct, bill.OverageUnits, bill.OverageFee, bill.Tax, bill.TotalDue, want)
	}
	// 超额单价也必须来自保存的快照（与超额费交叉印证）。
	if bill.OverageFee != bill.OverageUnits*saved.OveragePrice {
		t.Fatalf("%q overage fee %d != units %d * saved overage price %d",
			acct, bill.OverageFee, bill.OverageUnits, saved.OveragePrice)
	}
	if !bill.DueAt.Equal(jan(2026).End().AddDate(0, 0, 7)) {
		t.Fatalf("%q bill due at = %v, want 2026-02-08T00:00:00Z", acct, bill.DueAt)
	}

	// 出账后再次查询：账单固定不变；一月用量仍是 25；账户当前条件仍是
	// 开通保存的那一份。
	got, err := s.GetBill(acct, jan(2026))
	if err != nil {
		t.Fatalf("get bill %q: %v", acct, err)
	}
	if got != bill {
		t.Fatalf("%q bill changed between create and get:\ncreate=%+v\nget   =%+v", acct, bill, got)
	}
	if u, _ := s.MonthlyUsage(acct, jan(2026)); u.Total != 25 {
		t.Fatalf("%q jan usage after billing = %d, want 25", acct, u.Total)
	}
	st, _ = s.Status(acct)
	if !st.Subscribed || st.CurrentTerms != saved {
		t.Fatalf("%q current terms after billing = %+v, want %+v", acct, st.CurrentTerms, saved)
	}

	// 出账后再改一次套餐定义：已固定的账单与账户快照都不被改写。
	if err := s.UpdatePlan(planDef(d.planID, 4000, 40, 400, 900)); err != nil {
		t.Fatalf("post-billing plan update: %v", err)
	}
	again, err := s.GetBill(acct, jan(2026))
	if err != nil {
		t.Fatalf("get bill after update %q: %v", acct, err)
	}
	if again != bill {
		t.Fatalf("%q bill changed after later plan update:\nbefore=%+v\nafter =%+v", acct, bill, again)
	}
}

// TestConcurrentSubscribeAndPlanUpdateKeepsOneCompleteDefinition 并发提交
// “修改套餐定义”与“开通订阅”：每轮两项操作都必须成功；账户保存的条件
// 必须整份属于原定义或修改后定义；随后第三次改价、一月 25 单位用量与
// 二月一日出账全程沿用该份快照。并发调度不保证某种先后，因此多轮运行，
// 并由确定性顺序用例分别锁住两种结果。
func TestConcurrentSubscribeAndPlanUpdateKeepsOneCompleteDefinition(t *testing.T) {
	const rounds = 100
	sawOld, sawUpdated := false, false
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("race-acct-%d", iter)
		d := newActivationDefs(fmt.Sprintf("race-plan-%d", iter))
		s, clk := newActivationRaceService(t, acct, d)

		subErr, updErr := contendActivationWithPlanUpdate(s, acct, d.updatedDef)
		if subErr != nil {
			t.Fatalf("iter %d subscribe failed: %v", iter, subErr)
		}
		if updErr != nil {
			t.Fatalf("iter %d plan update failed: %v", iter, updErr)
		}
		// 修改总是成功：争用结束后套餐当前定义必为修改后的定义。
		if got := s.Status2(d.planID); got != d.updatedDef {
			t.Fatalf("iter %d plan definition = %+v, want updated %+v", iter, got, d.updatedDef)
		}

		saved := assertActivationSnapshotCoherent(t, s, acct, d)
		switch saved {
		case termsOf(d.oldDef):
			sawOld = true
		case termsOf(d.updatedDef):
			sawUpdated = true
		}

		assertLaterPlanUpdateDoesNotRewriteSnapshot(t, s, acct, saved, d)
		assertJanuaryBillUsesSavedSnapshot(t, s, clk, acct, saved, d)
	}
	// 提示性要求：若调度始终只给出一种顺序，确定性用例仍会分别保障两种结果。
	if !sawOld || !sawUpdated {
		t.Logf("scheduler only exercised one order this run (old=%v updated=%v); deterministic subtests cover both",
			sawOld, sawUpdated)
	}
}

// TestSubscribeAndPlanUpdateBothOrdersDeterministic 以确定顺序锁住两种先后
// 关系：开通先被接受时账户保存原定义（一月账单 2750）；修改先被接受时
// 账户保存修改后的定义（一月账单 3180）。两种顺序下第三组定义都不能混入。
func TestSubscribeAndPlanUpdateBothOrdersDeterministic(t *testing.T) {
	t.Run("subscribe-accepted-first-keeps-old-definition", func(t *testing.T) {
		acct := "det-sub-first"
		d := newActivationDefs("det-sub-first-plan")
		s, clk := newActivationRaceService(t, acct, d)

		// 顺序一：开通先被接受，账户当场保存原定义。
		if err := s.Subscribe(acct, d.planID, utc(2026, 1, 1, 0, 0)); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		st, _ := s.Status(acct)
		if !st.Subscribed || st.CurrentTerms != termsOf(d.oldDef) {
			t.Fatalf("after subscribe: %+v, want old terms", st)
		}
		// 修改随后被接受：只影响之后新快照，已开通订阅不被改写。
		if err := s.UpdatePlan(d.updatedDef); err != nil {
			t.Fatalf("update: %v", err)
		}
		saved := assertActivationSnapshotCoherent(t, s, acct, d)
		if saved != termsOf(d.oldDef) {
			t.Fatalf("subscribe-first must keep old definition, got %+v", saved)
		}

		assertLaterPlanUpdateDoesNotRewriteSnapshot(t, s, acct, saved, d)
		assertJanuaryBillUsesSavedSnapshot(t, s, clk, acct, saved, d)
	})

	t.Run("update-accepted-first-keeps-updated-definition", func(t *testing.T) {
		acct := "det-upd-first"
		d := newActivationDefs("det-upd-first-plan")
		s, clk := newActivationRaceService(t, acct, d)

		// 顺序二：修改先被接受，套餐定义已换成新值。
		if err := s.UpdatePlan(d.updatedDef); err != nil {
			t.Fatalf("update: %v", err)
		}
		if got := s.Status2(d.planID); got != d.updatedDef {
			t.Fatalf("plan definition = %+v, want updated", got)
		}
		// 开通随后被接受：必须按修改后的完整定义保存快照。
		if err := s.Subscribe(acct, d.planID, utc(2026, 1, 1, 0, 0)); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		saved := assertActivationSnapshotCoherent(t, s, acct, d)
		if saved != termsOf(d.updatedDef) {
			t.Fatalf("update-first must keep updated definition, got %+v", saved)
		}

		assertLaterPlanUpdateDoesNotRewriteSnapshot(t, s, acct, saved, d)
		assertJanuaryBillUsesSavedSnapshot(t, s, clk, acct, saved, d)
	})
}
