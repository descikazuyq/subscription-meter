package meter

import (
	"fmt"
	"sync"
	"testing"
)

// 本文件回归“修改套餐定义（UpdatePlan）与开通订阅（Subscribe）同时提交”的交错。
//
// 修改套餐定义只影响之后新保存的条件快照，已开通订阅继续沿用开通时保存的
// 快照——这两条规则各自都有保障；这里补的是两个操作并发到达同一服务时的
// 交错：开通读取套餐定义与修改写入新定义之间不允许撕裂，账户保存的月费、
// 包含额度、超额单价、税率必须整体来自同一份定义，套餐标识也保持一致。
//
// 场景：套餐 plan 原定义为月费 1000 分、包含 10 单位、超额单价 100 分、
// 税率万分之 1000；修改为月费 2000 分、包含 20 单位、超额单价 200 分、
// 税率万分之 600。一个已建立、无账单无欠费、尚未开通订阅的账户，在
// 2026 年 1 月中旬登记自 1 月 1 日零点 UTC 起开通。两项操作同时提交时
// 都应成功：开通先被接受则账户保存原定义，修改先被接受则账户保存修改后
// 的定义；允许任意一种顺序，但不允许四项条件各自取不同版本。
//
// 实现把“读取套餐定义 → 保存开通快照”放在同一把互斥锁的临界区内，
// 这些测试锁住该行为，防止日后把读定义与存快照拆成两步而出现混合条件。

var (
	// subPlanOriginal 是开通前的套餐定义。
	subPlanOriginal = Plan{ID: "plan", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000}
	// subPlanUpdated 是第一次修改后的套餐定义。
	subPlanUpdated = Plan{ID: "plan", MonthlyFee: 2000, IncludedUnits: 20, OveragePrice: 200, TaxRateBasisPoints: 600}
	// subPlanThird 是开通结果已定之后的又一次改价：一组不同的有效条件，
	// 不得混入已保存的快照与已出的账单。
	subPlanThird = Plan{ID: "plan", MonthlyFee: 3000, IncludedUnits: 30, OveragePrice: 300, TaxRateBasisPoints: 300}

	subTermsOriginal = PlanTerms{PlanID: "plan", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000}
	subTermsUpdated  = PlanTerms{PlanID: "plan", MonthlyFee: 2000, IncludedUnits: 20, OveragePrice: 200, TaxRateBasisPoints: 600}
)

// contendSubscribeAndUpdate 用启动屏障让“开通订阅”与“修改套餐定义”真正并发
// 提交，返回两个操作各自的错误。屏障保证并发，而非顺序调用。
func contendSubscribeAndUpdate(s *Service, acct string, updated Plan) (subErr, updateErr error) {
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
		updateErr = s.UpdatePlan(updated)
	}()
	close(start)
	wg.Wait()
	return subErr, updateErr
}

// mustActiveSnapshot 断言账户订阅已生效，且当前条件完整等于两份候选定义之一
// （四项计费条件与套餐标识同源，不允许混合），返回实际保存的那份快照。
func mustActiveSnapshot(t *testing.T, s *Service, acct string) PlanTerms {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	if !st.Subscribed {
		t.Fatalf("status %q: Subscribed=false, want subscribed", acct)
	}
	terms := st.CurrentTerms
	if terms.PlanID != "plan" {
		t.Fatalf("status %q: CurrentTerms.PlanID=%q, want %q", acct, terms.PlanID, "plan")
	}
	if terms != subTermsOriginal && terms != subTermsUpdated {
		t.Fatalf("status %q: CurrentTerms=%+v mixes fields from different plan definitions", acct, terms)
	}
	return terms
}

// janBillWant 是一月账单按某份完整条件应得的全部金额明细（总用量 25 单位）。
type janBillWant struct {
	includedUnits int64
	overageUnits  int64
	monthlyFee    int64
	overageFee    int64
	tax           int64
	totalDue      int64
}

// wantJanBill 返回总用量 25 单位时按 terms 这份完整条件应得的一月账单明细。
// 数值按题面锁定：原定义超额 15 单位、超额费 1500、税额 250、应付 2750；
// 修改后定义超额 5 单位、超额费 1000、税额 180、应付 3180。
func wantJanBill(t *testing.T, terms PlanTerms) janBillWant {
	t.Helper()
	switch terms {
	case subTermsOriginal:
		return janBillWant{includedUnits: 10, overageUnits: 15, monthlyFee: 1000, overageFee: 1500, tax: 250, totalDue: 2750}
	case subTermsUpdated:
		return janBillWant{includedUnits: 20, overageUnits: 5, monthlyFee: 2000, overageFee: 1000, tax: 180, totalDue: 3180}
	default:
		t.Fatalf("no locked bill expectation for terms %+v", terms)
		return janBillWant{}
	}
}

// mustJanuaryBill 走完开通之后的后半程并锁定结果：
//  1. 把同一套餐再改成第三组不同的有效条件——账户查询仍保留开通取得的整份
//     快照，后来的改价不混入；
//  2. 账户在一月接收 25 单位用量；
//  3. 时钟推进到 2 月 1 日零点 UTC 后为一月出账——账单的条件、包含额度与
//     月费与开通结果对应，总用量仍是已接收的 25 单位。
func mustJanuaryBill(t *testing.T, s *Service, clk *fakeClock, acct string, terms PlanTerms) {
	t.Helper()

	// 第三次改价：只影响之后新保存的快照，不改写本账户开通时保存的这份。
	if err := s.UpdatePlan(subPlanThird); err != nil {
		t.Fatalf("acct %q third update: %v", acct, err)
	}
	if got := mustCurrentTerms(t, s, acct); got != terms {
		t.Fatalf("acct %q terms after third update: %+v, want activation snapshot %+v", acct, got, terms)
	}

	// 一月接收 25 单位用量（开通时刻 1 月 1 日零点之后）。
	for _, ev := range []Event{
		{AccountID: acct, EventID: "ev-jan-a", At: utc(2026, 1, 5, 12, 0), Quantity: 10},
		{AccountID: acct, EventID: "ev-jan-b", At: utc(2026, 1, 12, 12, 0), Quantity: 15},
	} {
		r, err := s.RecordEvent(ev)
		if err != nil || !r.Accepted || r.Period != jan(2026) {
			t.Fatalf("acct %q record %q: r=%+v err=%v", acct, ev.EventID, r, err)
		}
	}
	if u, err := s.MonthlyUsage(acct, jan(2026)); err != nil || u.Total != 25 {
		t.Fatalf("acct %q january usage: %+v err=%v, want 25", acct, u, err)
	}

	// 2 月 1 日零点 UTC 为一月出账。
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill(acct, jan(2026))
	if err != nil {
		t.Fatalf("acct %q create january bill: %v", acct, err)
	}
	want := wantJanBill(t, terms)
	// 账单条件整份等于开通快照：后来的改价（含第三次）不混入。
	if b.Terms != terms {
		t.Fatalf("acct %q bill terms %+v, want activation snapshot %+v", acct, b.Terms, terms)
	}
	if b.TotalUsage != 25 ||
		b.IncludedUnits != want.includedUnits || b.OverageUnits != want.overageUnits ||
		b.MonthlyFee != want.monthlyFee || b.OverageFee != want.overageFee ||
		b.Tax != want.tax || b.TotalDue != want.totalDue {
		t.Fatalf("acct %q january bill: usage=%d included=%d overage=%d fee=%d overageFee=%d tax=%d due=%d, "+
			"want usage=25 included=%d overage=%d fee=%d overageFee=%d tax=%d due=%d",
			acct, b.TotalUsage, b.IncludedUnits, b.OverageUnits, b.MonthlyFee, b.OverageFee, b.Tax, b.TotalDue,
			want.includedUnits, want.overageUnits, want.monthlyFee, want.overageFee, want.tax, want.totalDue)
	}
	if b.Paid != 0 || b.Balance != want.totalDue || b.Settled {
		t.Fatalf("acct %q january bill payment state: paid=%d balance=%d settled=%v, want 0/%d/false",
			acct, b.Paid, b.Balance, b.Settled, want.totalDue)
	}

	// 重复出账得到同一张账单；GetBill 与账户状态摘要反映同一份结果。
	again, err := s.CreateBill(acct, jan(2026))
	if err != nil || again != b {
		t.Fatalf("acct %q rebill: %+v err=%v, want identical bill", acct, again, err)
	}
	got, err := s.GetBill(acct, jan(2026))
	if err != nil || got != b {
		t.Fatalf("acct %q get bill: %+v err=%v, want %+v", acct, got, err, b)
	}
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("acct %q status: %v", acct, err)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) ||
		st.Bills[0].TotalDue != want.totalDue || st.Bills[0].Balance != want.totalDue || st.Bills[0].Settled {
		t.Fatalf("acct %q status bills: %+v, want one january bill due=%d", acct, st.Bills, want.totalDue)
	}
	// 出账后当前条件仍是开通快照，不被第三次改价覆盖。
	if st.CurrentTerms != terms {
		t.Fatalf("acct %q current terms after billing: %+v, want %+v", acct, st.CurrentTerms, terms)
	}
}

// mustCurrentTerms 断言账户订阅已生效并返回当前生效的套餐条件。
func mustCurrentTerms(t *testing.T, s *Service, acct string) PlanTerms {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	if !st.Subscribed {
		t.Fatalf("status %q: Subscribed=false after third update", acct)
	}
	return st.CurrentTerms
}

// TestConcurrentSubscribeAndUpdatePlanTermsConsistent 并发提交开通与修改：
// 两项操作都应成功，账户查询显示订阅已生效；保存的条件必须完整属于其中
// 一份定义——开通先被接受时为原定义，修改先被接受时为修改后的定义，
// 月费、包含额度、超额单价、税率不得各自取不同版本。随后第三次改价不
// 影响该快照，一月 25 单位用量按同一份条件出账。
func TestConcurrentSubscribeAndUpdatePlanTermsConsistent(t *testing.T) {
	const rounds = 100
	seen := make(map[PlanTerms]int)
	for iter := 0; iter < rounds; iter++ {
		s, clk := newTestService(utc(2026, 1, 15, 12, 0))
		mustPlan(t, s, subPlanOriginal)
		acct := fmt.Sprintf("acct-%d", iter)
		mustAccount(t, s, acct)

		subErr, updateErr := contendSubscribeAndUpdate(s, acct, subPlanUpdated)
		// 同时提交时两项操作都应成功，不要求某一项必然先成功。
		if subErr != nil {
			t.Fatalf("iter %d subscribe: %v", iter, subErr)
		}
		if updateErr != nil {
			t.Fatalf("iter %d update plan: %v", iter, updateErr)
		}

		terms := mustActiveSnapshot(t, s, acct)
		seen[terms]++

		// 修改确已生效：此后新开通的账户取得修改后的完整定义。
		later := fmt.Sprintf("later-%d", iter)
		mustAccount(t, s, later)
		if err := s.Subscribe(later, "plan", utc(2026, 1, 15, 12, 0)); err != nil {
			t.Fatalf("iter %d subscribe later account: %v", iter, err)
		}
		if got := mustCurrentTerms(t, s, later); got != subTermsUpdated {
			t.Fatalf("iter %d later account terms %+v, want updated definition %+v", iter, got, subTermsUpdated)
		}

		mustJanuaryBill(t, s, clk, acct, terms)
	}
	// 两种顺序都合法，并发调度不保证各出现多少次；两种先后关系的完整
	// 行为由 TestSubscribeUpdatePlanBothOrdersDeterministic 各自锁定。
	t.Logf("order distribution over %d rounds: %v", rounds, seen)
}

// TestSubscribeUpdatePlanBothOrdersDeterministic 以确定顺序各自完整走一遍两种
// 先后关系（并发测试不保证每次都调度出两种顺序）：
//   - 开通先被接受：账户保存原定义，一月账单超额 15 单位、超额费 1500、
//     税额 250、应付 2750；
//   - 修改先被接受：账户保存修改后的定义，一月账单超额 5 单位、超额费 1000、
//     税额 180、应付 3180。
//
// 两种情况下第三次改价都不混入已保存的快照与账单。
func TestSubscribeUpdatePlanBothOrdersDeterministic(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, subPlanOriginal)

	// 顺序一：先开通（保存原定义），后修改套餐。
	mustAccount(t, s, "sub-first")
	if err := s.Subscribe("sub-first", "plan", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("sub-first subscribe: %v", err)
	}
	if err := s.UpdatePlan(subPlanUpdated); err != nil {
		t.Fatalf("sub-first update: %v", err)
	}
	if got := mustActiveSnapshot(t, s, "sub-first"); got != subTermsOriginal {
		t.Fatalf("sub-first terms %+v, want original definition %+v", got, subTermsOriginal)
	}

	// 顺序二：先修改套餐，后开通（保存修改后的定义）。
	mustAccount(t, s, "update-first")
	if err := s.Subscribe("update-first", "plan", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("update-first subscribe: %v", err)
	}
	if got := mustActiveSnapshot(t, s, "update-first"); got != subTermsUpdated {
		t.Fatalf("update-first terms %+v, want updated definition %+v", got, subTermsUpdated)
	}

	// 第三次改价后分别为两个账户接收一月用量并出账：各按各的开通快照计费。
	mustJanuaryBill(t, s, clk, "sub-first", subTermsOriginal)
	mustJanuaryBill(t, s, clk, "update-first", subTermsUpdated)
}
