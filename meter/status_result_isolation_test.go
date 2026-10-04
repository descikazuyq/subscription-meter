package meter

import (
	"testing"
)

// 状态查询返回的各月用量、账单摘要与已安排的订阅终止时刻，是结果专属的副本：
// 调用方整理或修改拿到的内容只能留在该份结果中；同一账户后续通过公开功能发生的
// 变化既不能反过来改写此前保存的结果，也不被调用方对结果的改动影响。
// 本文件只回归已有查询行为，不新增任何公开功能。

// mustStatus 是 Status 的测试便捷封装。
func mustStatus(t *testing.T, s *Service, acct string) AccountStatus {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %s: %v", acct, err)
	}
	return st
}

// summaryOf 从状态中取指定账期的账单摘要；不存在时终止测试。
func summaryOf(t *testing.T, st AccountStatus, p Month) BillSummary {
	t.Helper()
	for _, b := range st.Bills {
		if b.Period == p {
			return b
		}
	}
	t.Fatalf("bill summary %s not in status: %+v", p, st.Bills)
	return BillSummary{}
}

// usageOf 从状态中取指定账期的累计用量；不存在时返回零值结果。
func usageOf(st AccountStatus, p Month) Usage {
	for _, u := range st.MonthlyUsage {
		if u.Period == p {
			return u
		}
	}
	return Usage{Period: p}
}

// assertStatusPeriods 校验状态中各月用量与账单均按账期先后排列。
func assertStatusPeriods(t *testing.T, st AccountStatus) {
	t.Helper()
	for i := 1; i < len(st.MonthlyUsage); i++ {
		if !st.MonthlyUsage[i-1].Period.Before(st.MonthlyUsage[i].Period) {
			t.Fatalf("monthly usage not in period order: %+v", st.MonthlyUsage)
		}
	}
	for i := 1; i < len(st.Bills); i++ {
		if !st.Bills[i-1].Period.Before(st.Bills[i].Period) {
			t.Fatalf("bills not in period order: %+v", st.Bills)
		}
	}
}

// setupIndependentStatusAccount 准备一个有不同月份用量与账单、并登记了按月取消的账户：
//   - 2026-01 用量 7、账单已出账（应付 1000，截止 2/8，尚未付款）；
//   - 2026-02 用量 4、账单已出账（应付 1000，截止 3/8，尚未付款）；
//   - 2026-03-15 登记按月取消，原定终止时刻为 2026-04-01 00:00 UTC，
//     返回时时钟停在 2026-03-15，订阅处于等待取消状态（同时因两张账单
//     到期未付处于欠费停用；欠费不影响取消安排与状态副本的回归）。
//
// 需要在终止后继续接收用量的用例，应先撤回取消并结清到期账单再推进时钟。
func setupIndependentStatusAccount(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("p", 1000, 0, 0, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "p", utc(2026, 1, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "jan-1", At: utc(2026, 1, 10, 0, 0), Quantity: 7}); err != nil {
		t.Fatalf("record jan usage: %v", err)
	}
	// 2/1 为 1 月出账；随后在 1 月账单截止（2/8）前补录 2 月用量。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("u", jan(2026)); err != nil {
		t.Fatalf("create jan bill: %v", err)
	}
	clk.t = utc(2026, 2, 5, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "feb-1", At: utc(2026, 2, 5, 0, 0), Quantity: 4}); err != nil {
		t.Fatalf("record feb usage: %v", err)
	}
	// 3/1 为 2 月出账；3/15 登记按月取消，原定终止时刻为 4/1。
	clk.t = utc(2026, 3, 1, 0, 0)
	if _, err := s.CreateBill("u", feb(2026)); err != nil {
		t.Fatalf("create feb bill: %v", err)
	}
	clk.t = utc(2026, 3, 15, 12, 0)
	mustCancel(t, s, "u")
	return s, clk
}

// TestStatusResultIsolation_MutatingSnapshot 调用方对返回结果的用量总数、账单摘要
// （已付金额、余额、付清状态）以及终止时刻的改动，只能留在该份结果中：
// 再次查询、按月查用量、查账单都必须仍显示真实数据。
func TestStatusResultIsolation_MutatingSnapshot(t *testing.T) {
	s, _ := setupIndependentStatusAccount(t)
	wantEnd := utc(2026, 4, 1, 0, 0)

	// 等待取消期间取得一份状态：有真实的各月用量、两张账单与原定终止时刻。
	st := mustStatus(t, s, "u")
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("initial status: %+v", st)
	}
	if len(st.MonthlyUsage) != 2 || len(st.Bills) != 2 {
		t.Fatalf("initial status should list two months: %+v", st)
	}
	if usageOf(st, jan(2026)).Total != 7 || usageOf(st, feb(2026)).Total != 4 {
		t.Fatalf("initial usage: %+v", st.MonthlyUsage)
	}
	for _, p := range []Month{jan(2026), feb(2026)} {
		sum := summaryOf(t, st, p)
		if sum.TotalDue != 1000 || sum.Paid != 0 || sum.Balance != 1000 || sum.Settled {
			t.Fatalf("initial %s summary: %+v", p, sum)
		}
	}
	assertStatusPeriods(t, st)

	// 调用方在自己的这份结果上任意改动：用量总数、账单已付/余额/付清、终止时刻
	// 改成更早或更晚。
	st.MonthlyUsage[0].Total = 999
	st.MonthlyUsage[1].Total = -5
	st.Bills[0].Paid = 1000
	st.Bills[0].Balance = 0
	st.Bills[0].Settled = true
	st.Bills[1].Paid = 1
	st.Bills[1].Balance = 999
	*st.ScheduledEnd = utc(2026, 3, 20, 0, 0)
	*st.ScheduledEnd = utc(2026, 5, 15, 0, 0)

	// 再次查询仍看到真实数据，且仍按账期先后展示。
	st2 := mustStatus(t, s, "u")
	if !st2.Subscribed || st2.ScheduledEnd == nil || !st2.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("real end changed after mutating snapshot: %+v", st2)
	}
	if usageOf(st2, jan(2026)).Total != 7 || usageOf(st2, feb(2026)).Total != 4 {
		t.Fatalf("real usage changed: %+v", st2.MonthlyUsage)
	}
	for _, p := range []Month{jan(2026), feb(2026)} {
		sum := summaryOf(t, st2, p)
		if sum.Paid != 0 || sum.Balance != 1000 || sum.Settled {
			t.Fatalf("real %s bill summary changed: %+v", p, sum)
		}
	}
	assertStatusPeriods(t, st2)

	// 按月查询用量与查询账单也要保持原值，不能因为修改摘要而改变计量或收款结果。
	for _, c := range []struct {
		p Month
		n int64
	}{{jan(2026), 7}, {feb(2026), 4}} {
		if u, err := s.MonthlyUsage("u", c.p); err != nil || u.Total != c.n {
			t.Fatalf("monthly usage %s altered by snapshot edit: %+v %v", c.p, u, err)
		}
	}
	for _, p := range []Month{jan(2026), feb(2026)} {
		b, err := s.GetBill("u", p)
		if err != nil {
			t.Fatalf("get bill %s: %v", p, err)
		}
		if b.Paid != 0 || b.Balance != 1000 || b.Settled {
			t.Fatalf("bill %s altered by snapshot edit: %+v", p, b)
		}
	}
}

// TestStatusResultIsolation_TwoSnapshotsIndependent 同一账户分别取得的两份状态彼此
// 独立：修改其中一份不能改变另一份；返回列表被重新排列或整段替换，也不能影响
// 下一次查询按账期先后展示的记录。
func TestStatusResultIsolation_TwoSnapshotsIndependent(t *testing.T) {
	s, _ := setupIndependentStatusAccount(t)
	wantEnd := utc(2026, 4, 1, 0, 0)

	a := mustStatus(t, s, "u")
	b := mustStatus(t, s, "u")

	// 修改 a：两份用量、两份账单摘要、终止时刻全部改写；b 必须保持取得时的原值。
	a.MonthlyUsage[0].Total = 111
	a.MonthlyUsage[1].Total = 222
	a.Bills[0].Paid = 777
	a.Bills[0].Balance = 223
	a.Bills[0].Settled = true
	a.Bills[1].Paid = 888
	*a.ScheduledEnd = utc(2026, 6, 1, 0, 0)

	if usageOf(b, jan(2026)).Total != 7 || usageOf(b, feb(2026)).Total != 4 {
		t.Fatalf("snapshot b usage changed by edit to a: %+v", b.MonthlyUsage)
	}
	for _, p := range []Month{jan(2026), feb(2026)} {
		bsum := summaryOf(t, b, p)
		if bsum.Paid != 0 || bsum.Balance != 1000 || bsum.Settled {
			t.Fatalf("snapshot b %s bill changed by edit to a: %+v", p, bsum)
		}
	}
	if b.ScheduledEnd == nil || !b.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("snapshot b end changed by edit to a: %+v", b.ScheduledEnd)
	}

	// 返回的列表被重新排列、追加或整段替换：只影响这份结果本身，不影响下一次查询。
	a.MonthlyUsage = append(a.MonthlyUsage, Usage{Period: apr(2026), Total: 5})
	a.Bills = append(a.Bills, BillSummary{Period: mar(2026), TotalDue: 9})
	for i, j := 0, len(a.MonthlyUsage)-1; i < j; i, j = i+1, j-1 {
		a.MonthlyUsage[i], a.MonthlyUsage[j] = a.MonthlyUsage[j], a.MonthlyUsage[i]
	}
	for i, j := 0, len(a.Bills)-1; i < j; i, j = i+1, j-1 {
		a.Bills[i], a.Bills[j] = a.Bills[j], a.Bills[i]
	}
	a.ScheduledEnd = nil

	c := mustStatus(t, s, "u")
	assertStatusPeriods(t, c)
	if len(c.MonthlyUsage) != 2 ||
		c.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 7}) ||
		c.MonthlyUsage[1] != (Usage{Period: feb(2026), Total: 4}) {
		t.Fatalf("next status usage affected by snapshot list edits: %+v", c.MonthlyUsage)
	}
	if len(c.Bills) != 2 || c.Bills[0].Period != jan(2026) || c.Bills[1].Period != feb(2026) ||
		c.Bills[0].Balance != 1000 || c.Bills[1].Balance != 1000 {
		t.Fatalf("next status bills affected by snapshot list edits: %+v", c.Bills)
	}
	if c.ScheduledEnd == nil || !c.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("next status end affected by snapshot list edits: %+v", c.ScheduledEnd)
	}
}

// TestStatusResultIsolation_NewOperationsDontRewriteOldSnapshot 反方向保障：保存一份
// 未改动的状态后，通过正常功能增加尚未出账月份的用量、为已有账单登记部分付款，
// 新查询应显示更新后的累计用量、已付金额和余额；旧结果保留取得时的数值与付清状态。
func TestStatusResultIsolation_NewOperationsDontRewriteOldSnapshot(t *testing.T) {
	s, clk := setupIndependentStatusAccount(t)
	wantEnd := utc(2026, 4, 1, 0, 0)

	// 3 月 15 日取得旧结果：1/2 月用量分别为 7/4、两张账单均未付款、等待取消（4/1）。
	old := mustStatus(t, s, "u")
	if !old.Subscribed || old.ScheduledEnd == nil || !old.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("old status: %+v", old)
	}
	oldJan := usageOf(old, jan(2026)).Total
	oldFeb := usageOf(old, feb(2026)).Total
	oldJanSum := summaryOf(t, old, jan(2026))

	// 撤回取消让订阅延续；为已有账单登记一笔部分付款。
	if err := s.UndoCancelSubscription("u"); err != nil {
		t.Fatalf("undo cancel: %v", err)
	}
	if r, err := s.RecordPayment("u", "pay-partial", jan(2026), 400); err != nil || r.BillBalance != 600 || r.Settled {
		t.Fatalf("partial payment: %+v %v", r, err)
	}
	// 紧接着的查询显示更新后的已付金额与余额（尚未付清）。
	mid := mustStatus(t, s, "u")
	if m := summaryOf(t, mid, jan(2026)); m.Paid != 400 || m.Balance != 600 || m.Settled {
		t.Fatalf("status after partial payment: %+v", m)
	}

	// 两张到期账单都结清后解除欠费停用，才能继续接收新增用量；
	// 再为尚未出账的 3 月增加用量。
	if _, err := s.RecordPayment("u", "pay-jan-rest", jan(2026), 600); err != nil {
		t.Fatalf("settle jan: %v", err)
	}
	if _, err := s.RecordPayment("u", "pay-feb", feb(2026), 1000); err != nil {
		t.Fatalf("settle feb: %v", err)
	}
	clk.t = utc(2026, 3, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mar-1", At: utc(2026, 3, 20, 0, 0), Quantity: 5}); err != nil {
		t.Fatalf("record unbilled march usage: %v", err)
	}

	// 新查询显示更新后的累计用量；两张账单均已结清；撤回后不再有终止时刻。
	fresh := mustStatus(t, s, "u")
	if usageOf(fresh, mar(2026)).Total != 5 {
		t.Fatalf("new status march usage = %d, want 5", usageOf(fresh, mar(2026)).Total)
	}
	if usageOf(fresh, jan(2026)).Total != 7 || usageOf(fresh, feb(2026)).Total != 4 {
		t.Fatalf("new status old usage changed: %+v", fresh.MonthlyUsage)
	}
	for _, p := range []Month{jan(2026), feb(2026)} {
		m := summaryOf(t, fresh, p)
		if m.Paid != 1000 || m.Balance != 0 || !m.Settled {
			t.Fatalf("new status %s should be settled: %+v", p, m)
		}
	}
	if !fresh.Subscribed || fresh.Suspended || fresh.ScheduledEnd != nil {
		t.Fatalf("new status after undo and payments: %+v", fresh)
	}

	// 旧结果仍保留取得时的数值、付清状态与终止安排，且没有被加入 3 月用量。
	if usageOf(old, jan(2026)).Total != oldJan || usageOf(old, feb(2026)).Total != oldFeb {
		t.Fatalf("old snapshot usage changed: %+v", old.MonthlyUsage)
	}
	if len(old.MonthlyUsage) != 2 {
		t.Fatalf("old snapshot gained later usage: %+v", old.MonthlyUsage)
	}
	osum := summaryOf(t, old, jan(2026))
	if osum.Paid != oldJanSum.Paid || osum.Balance != oldJanSum.Balance || osum.Settled != oldJanSum.Settled {
		t.Fatalf("old snapshot jan summary changed: %+v, want %+v", osum, oldJanSum)
	}
	if osum.Paid != 0 || osum.Balance != 1000 || osum.Settled {
		t.Fatalf("old snapshot should keep captured jan values: %+v", osum)
	}
	if f := summaryOf(t, old, feb(2026)); f.Paid != 0 || f.Balance != 1000 || f.Settled {
		t.Fatalf("old snapshot should keep captured feb values: %+v", f)
	}
	if old.ScheduledEnd == nil || !old.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("old snapshot lost captured end: %+v", old.ScheduledEnd)
	}

	// 4/1 为 3 月出账只影响新查询：旧结果的列表长度与数值保持取得时的样子。
	clk.t = utc(2026, 4, 1, 0, 0)
	if _, err := s.CreateBill("u", mar(2026)); err != nil {
		t.Fatalf("create march bill: %v", err)
	}
	if got := mustStatus(t, s, "u"); len(got.MonthlyUsage) != 3 || len(got.Bills) != 3 {
		t.Fatalf("new status should list three months: %+v", got)
	}
	if len(old.MonthlyUsage) != 2 || len(old.Bills) != 2 {
		t.Fatalf("old snapshot mutated by later billing: usage=%+v bills=%+v", old.MonthlyUsage, old.Bills)
	}
}

// TestStatusResultIsolation_UndoCancelKeptOnlyInOldSnapshot 撤回取消后，新查询不再
// 显示终止时刻，旧结果保留当时的安排；再修改这份旧结果不能重新登记取消，也不能
// 使已经撤回的订阅在原定时刻终止。
func TestStatusResultIsolation_UndoCancelKeptOnlyInOldSnapshot(t *testing.T) {
	s, clk := setupIndependentStatusAccount(t)
	wantEnd := utc(2026, 4, 1, 0, 0)

	old := mustStatus(t, s, "u")
	if old.ScheduledEnd == nil || !old.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("old status end: %+v", old)
	}

	// 通过正常功能撤回取消。
	if err := s.UndoCancelSubscription("u"); err != nil {
		t.Fatalf("undo: %v", err)
	}
	fresh := mustStatus(t, s, "u")
	if !fresh.Subscribed || fresh.ScheduledEnd != nil {
		t.Fatalf("after undo new status should show no end: %+v", fresh)
	}
	// 旧结果仍保留当时的安排。
	if old.ScheduledEnd == nil || !old.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("old snapshot lost end after undo: %+v", old.ScheduledEnd)
	}

	// 改写旧结果里的终止时刻（改早/改晚/置空）不能重新登记取消。
	*old.ScheduledEnd = utc(2026, 3, 18, 0, 0)
	if st := mustStatus(t, s, "u"); st.ScheduledEnd != nil {
		t.Fatalf("editing old snapshot registered an earlier cancel: %+v", st)
	}
	*old.ScheduledEnd = utc(2026, 5, 1, 0, 0)
	if st := mustStatus(t, s, "u"); st.ScheduledEnd != nil {
		t.Fatalf("editing old snapshot registered a later cancel: %+v", st)
	}
	old.ScheduledEnd = nil
	if st := mustStatus(t, s, "u"); st.ScheduledEnd != nil {
		t.Fatalf("nil-ing old snapshot scheduled end: %+v", st)
	}

	// 结清到期账单以解除停用，随后 3 月仍接收用量。
	if _, err := s.RecordPayment("u", "pay-jan", jan(2026), 1000); err != nil {
		t.Fatalf("settle jan: %v", err)
	}
	if _, err := s.RecordPayment("u", "pay-feb", feb(2026), 1000); err != nil {
		t.Fatalf("settle feb: %v", err)
	}
	clk.t = utc(2026, 3, 20, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mar", At: utc(2026, 3, 20, 0, 0), Quantity: 1}); err != nil {
		t.Fatalf("march usage after undo should be accepted: %v", err)
	}

	// 跨过原定终止时刻：订阅仍有效（取消确已撤回，而非由旧结果的时间决定）。
	clk.t = utc(2026, 4, 1, 0, 0)
	st := mustStatus(t, s, "u")
	if !st.Subscribed || st.ScheduledEnd != nil || st.CurrentTerms.PlanID != "p" {
		t.Fatalf("subscription ended at old snapshot's time despite undo: %+v", st)
	}
	// 原定时刻之后仍接收新月份用量：被改写到 5 月的时间也不能令其终止。
	clk.t = utc(2026, 4, 2, 0, 0)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "apr", At: utc(2026, 4, 2, 0, 0), Quantity: 1}); err != nil {
		t.Fatalf("april usage after undone end should be accepted: %v", err)
	}
	clk.t = utc(2026, 5, 2, 0, 0)
	st = mustStatus(t, s, "u")
	if !st.Subscribed {
		t.Fatalf("subscription should remain active well after the undone end: %+v", st)
	}
}

// TestStatusResultIsolation_RealCancelEndsAtRegisteredTime 未撤回取消的账户仍须按
// 最初登记的终止时刻结束订阅，不能由调用方改过的时间决定。原定时刻之前仍有有效
// 订阅；到达该时刻后即使只查询状态，也应显示无有效订阅、无当前套餐且无终止安排。
// 此前保存的状态继续表示取得时尚在等待取消。
func TestStatusResultIsolation_RealCancelEndsAtRegisteredTime(t *testing.T) {
	s, clk := setupIndependentStatusAccount(t)
	wantEnd := utc(2026, 4, 1, 0, 0)

	// 取两份：一份原样保存，一份供调用方随意改时间。
	saved := mustStatus(t, s, "u")
	edited := mustStatus(t, s, "u")
	// 调用方把自己那份的终止时刻分别改成更早与更晚。
	*edited.ScheduledEnd = utc(2026, 3, 20, 0, 0)
	*edited.ScheduledEnd = utc(2026, 5, 1, 0, 0)

	// 原定时刻之前仍有有效订阅，终止安排仍是最初登记的 4/1。
	clk.t = utc(2026, 3, 31, 23, 59)
	st := mustStatus(t, s, "u")
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("before real end: %+v", st)
	}
	if st.CurrentTerms.PlanID != "p" {
		t.Fatalf("before real end current terms: %+v", st.CurrentTerms)
	}

	// 到达原定时刻：即使只查询状态，也无有效订阅、无当前套餐、无终止安排。
	clk.t = wantEnd
	ended := mustStatus(t, s, "u")
	if ended.Subscribed || ended.CurrentTerms != (PlanTerms{}) || ended.PendingChange != nil || ended.ScheduledEnd != nil {
		t.Fatalf("at real end: %+v", ended)
	}
	// 历史用量与账单继续可查，按账期先后排列。
	assertStatusPeriods(t, ended)
	if len(ended.MonthlyUsage) != 2 ||
		ended.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 7}) ||
		ended.MonthlyUsage[1] != (Usage{Period: feb(2026), Total: 4}) {
		t.Fatalf("history usage at end: %+v", ended.MonthlyUsage)
	}
	if len(ended.Bills) != 2 || ended.Bills[0].Period != jan(2026) || ended.Bills[1].Period != feb(2026) {
		t.Fatalf("history bills at end: %+v", ended.Bills)
	}

	// 此前保存、未改动的状态继续表示取得时尚在等待取消：原值不被后来的终止覆盖。
	if !saved.Subscribed || saved.ScheduledEnd == nil || !saved.ScheduledEnd.Equal(wantEnd) {
		t.Fatalf("saved snapshot changed at real end: %+v", saved)
	}
	if usageOf(saved, jan(2026)).Total != 7 || usageOf(saved, feb(2026)).Total != 4 {
		t.Fatalf("saved snapshot usage changed at real end: %+v", saved.MonthlyUsage)
	}
	// 被调用方改过的那份保留调用方写入的 5/1，既不影响服务，也不被服务覆盖。
	if edited.ScheduledEnd == nil || !edited.ScheduledEnd.Equal(utc(2026, 5, 1, 0, 0)) {
		t.Fatalf("edited snapshot overwritten by service: %+v", edited.ScheduledEnd)
	}

	// 原定时刻之后，被改成 5/1 的“更晚时间”不能让订阅延续：仍无有效订阅。
	clk.t = utc(2026, 4, 15, 0, 0)
	after := mustStatus(t, s, "u")
	if after.Subscribed || after.CurrentTerms != (PlanTerms{}) || after.ScheduledEnd != nil {
		t.Fatalf("subscription should not survive to edited later time: %+v", after)
	}
}

// TestStatusResultIsolation_NoArrangementCannotBeFabricated 没有取消安排时，查询中
// 的终止时刻为空；编辑返回结果（包括手动设置一个终止时刻）不能凭空产生安排。
func TestStatusResultIsolation_NoArrangementCannotBeFabricated(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("p", 1000, 0, 0, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "p", utc(2026, 1, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "jan", At: utc(2026, 1, 10, 0, 0), Quantity: 3}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	st := mustStatus(t, s, "u")
	if !st.Subscribed || st.ScheduledEnd != nil {
		t.Fatalf("status without cancellation should have nil end: %+v", st)
	}

	// 调用方在自己的结果里凭空放一个终止时刻并改用量。
	fabricated := utc(2026, 2, 1, 0, 0)
	st.ScheduledEnd = &fabricated
	st.MonthlyUsage[0].Total = 42

	// 再次查询仍无安排，用量仍是真实值。
	fresh := mustStatus(t, s, "u")
	if fresh.ScheduledEnd != nil {
		t.Fatalf("a cancellation was fabricated by editing status: %+v", fresh)
	}
	if usageOf(fresh, jan(2026)).Total != 3 {
		t.Fatalf("usage changed by snapshot edit: %+v", fresh.MonthlyUsage)
	}

	// 跨到 2 月：从未登记取消，订阅照常延续，不会在凭空的时刻终止。
	clk.t = utc(2026, 2, 1, 0, 0)
	atMonthBoundary := mustStatus(t, s, "u")
	if !atMonthBoundary.Subscribed || atMonthBoundary.ScheduledEnd != nil {
		t.Fatalf("subscription ended at fabricated time: %+v", atMonthBoundary)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "feb", At: utc(2026, 2, 1, 0, 0), Quantity: 2}); err != nil {
		t.Fatalf("usage at fabricated end should still be accepted: %v", err)
	}
}
