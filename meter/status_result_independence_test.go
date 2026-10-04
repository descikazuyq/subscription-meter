package meter

import (
	"testing"
	"time"
)

// 本文件为 Status 查询结果的相互独立性补充回归保障。Status 返回的
// MonthlyUsage、Bills 与 ScheduledEnd 都必须是调用方私有的快照副本：
//   - 调用方整理（重排、替换）或篡改自己拿到的结果只能留在该份结果中，
//     不能回写账户，后续查询、MonthlyUsage 与 GetBill 仍显示真实数据；
//   - 同一账户先后取得的两份状态彼此独立，互不改写；
//   - 账户后续正常操作（补报未出账月份用量、登记部分付款、撤回取消）
//     只反映在新查询中，旧结果定格在取得它的时刻；
//   - 篡改旧结果中的终止时刻既不能提前/推迟真实终止，也不能在撤回取消
//     后凭空重新登记取消；没有取消安排时编辑结果也不能制造安排。

// mustRecordEvent 登记一条用量事件，失败即终止测试。
func mustRecordEvent(t *testing.T, s *Service, e Event) {
	t.Helper()
	if r, err := s.RecordEvent(e); err != nil {
		t.Fatalf("record event %q: %v", e.EventID, err)
	} else if !r.Accepted {
		t.Fatalf("record event %q unexpectedly deduplicated", e.EventID)
	}
}

// statusUsageTotal 在状态结果中查找指定账期的累计用量，缺失即终止。
func statusUsageTotal(t *testing.T, st AccountStatus, p Month) int64 {
	t.Helper()
	for _, u := range st.MonthlyUsage {
		if u.Period == p {
			return u.Total
		}
	}
	t.Fatalf("usage for %s not found in %+v", p, st.MonthlyUsage)
	return 0
}

// statusBillSummary 在状态结果中查找指定账期的账单摘要，缺失即终止。
func statusBillSummary(t *testing.T, st AccountStatus, p Month) BillSummary {
	t.Helper()
	for _, b := range st.Bills {
		if b.Period == p {
			return b
		}
	}
	t.Fatalf("bill for %s not found in %+v", p, st.Bills)
	return BillSummary{}
}

// setupIndependenceFixture 建立带两个月用量、两张账单并已登记按月取消的
// 账户，结束时时钟停在 2026-03-05（等待取消期间，原定 4/1 终止）。
//
//	套餐 a：月费 1000、额度 10、超额单价 100、税率 0。
//	第一段订阅 1/1 开通、3/1 终止：1 月用量 12（账单 1200）、
//	2 月用量 30（账单 3000）；一月账单在 3/1 出账后当场付清，
//	二月账单未付（截止日 3/8，3/5 时尚未欠费停用）。
//	3/1 重新开通同一套餐，3/2 登记按月取消：当前订阅 4/1 终止。
//
// 返回服务、可调时钟与原定终止时刻。
func setupIndependenceFixture(t *testing.T) (*Service, *fakeClock, time.Time) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-jan", At: utc(2026, 1, 20, 0, 0), Quantity: 12})

	clk.t = utc(2026, 2, 15, 12, 0)
	mustCancel(t, s, "u") // 第一段订阅 3/1 终止；2 月仍整月有效。
	clk.t = utc(2026, 2, 20, 12, 0)
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-feb", At: utc(2026, 2, 20, 0, 0), Quantity: 30})

	// 到达 3/1：第一段订阅终止，为一、二月出账并当场付清一月账单。
	clk.t = utc(2026, 3, 1, 0, 0)
	janBill := mustBill(t, s, "u", jan(2026))
	if janBill.TotalDue != 1200 {
		t.Fatalf("jan bill total = %d, want 1200", janBill.TotalDue)
	}
	febBill := mustBill(t, s, "u", feb(2026))
	if febBill.TotalDue != 3000 {
		t.Fatalf("feb bill total = %d, want 3000", febBill.TotalDue)
	}
	if _, err := s.RecordPayment("u", "pay-jan", jan(2026), janBill.TotalDue); err != nil {
		t.Fatalf("pay jan bill: %v", err)
	}

	// 同一时刻重新开通，3/2 登记按月取消，4/1 终止。
	mustSubscribe(t, s, "u", "a", utc(2026, 3, 1, 0, 0))
	clk.t = utc(2026, 3, 2, 12, 0)
	r := mustCancel(t, s, "u")
	clk.t = utc(2026, 3, 5, 12, 0)
	return s, clk, r.Cancellation.EndAt
}

// assertRealFixtureStatus 断言状态显示夹具中的真实记录：用量按账期先后
// 为一月 12、二月 30；一月账单已付清，二月账单未付；订阅有效且等待原定终止。
func assertRealFixtureStatus(t *testing.T, st AccountStatus, end time.Time) {
	t.Helper()
	if !st.Subscribed {
		t.Fatalf("Subscribed = false, status = %+v", st)
	}
	if st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(end) {
		got := "<nil>"
		if st.ScheduledEnd != nil {
			got = st.ScheduledEnd.String()
		}
		t.Fatalf("ScheduledEnd = %s, want %v", got, end)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0].Period != jan(2026) || st.MonthlyUsage[0].Total != 12 ||
		st.MonthlyUsage[1].Period != feb(2026) || st.MonthlyUsage[1].Total != 30 {
		t.Fatalf("monthly usage = %+v, want jan=12 feb=30 in period order", st.MonthlyUsage)
	}
	if len(st.Bills) != 2 {
		t.Fatalf("bills = %+v, want two summaries", st.Bills)
	}
	jb := st.Bills[0]
	if jb.Period != jan(2026) || jb.TotalDue != 1200 || jb.Paid != 1200 || jb.Balance != 0 || !jb.Settled {
		t.Fatalf("jan summary = %+v, want paid 1200 settled", jb)
	}
	fb := st.Bills[1]
	if fb.Period != feb(2026) || fb.TotalDue != 3000 || fb.Paid != 0 || fb.Balance != 3000 || fb.Settled {
		t.Fatalf("feb summary = %+v, want unpaid 3000 not settled", fb)
	}
}

// TestStatusUsageBillsAndEndMutationsDoNotReachAccount 验证调用方篡改返回的
// 用量总数、账单摘要（已付金额、余额、付清状态）以及终止时刻（改成更早或
// 更晚、整体替换指针）都只留在该份结果中：再次查询仍看到真实数据，
// MonthlyUsage 与 GetBill 的计量、收款结果也不受摘要改动影响；重排或替换
// 返回的列表同样不能影响下一次查询按账期先后展示的记录。
func TestStatusUsageBillsAndEndMutationsDoNotReachAccount(t *testing.T) {
	s, _, end := setupIndependenceFixture(t)

	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertRealFixtureStatus(t, st, end)

	// 篡改这份结果里的用量总数与账单摘要。
	st.MonthlyUsage[0].Total = 999012
	st.MonthlyUsage[1].Total = 999030
	st.Bills[0].Paid = 1
	st.Bills[0].Balance = 1199
	st.Bills[0].Settled = false
	st.Bills[1].Paid = 3000
	st.Bills[1].Balance = 0
	st.Bills[1].Settled = true
	// 终止时刻先原地改成更早，再整体替换成更晚的指针。
	*st.ScheduledEnd = utc(2026, 3, 10, 0, 0)
	later := utc(2026, 12, 1, 0, 0)
	st.ScheduledEnd = &later

	// 再次查询：真实记录与原定终止时刻原样呈现。
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertRealFixtureStatus(t, st, end)

	// 按月查询用量与查询账单同样保持原值，不被摘要篡改波及。
	if u, err := s.MonthlyUsage("u", jan(2026)); err != nil || u.Total != 12 {
		t.Fatalf("jan MonthlyUsage = %+v %v, want total 12", u, err)
	}
	if u, err := s.MonthlyUsage("u", feb(2026)); err != nil || u.Total != 30 {
		t.Fatalf("feb MonthlyUsage = %+v %v, want total 30", u, err)
	}
	jb, err := s.GetBill("u", jan(2026))
	if err != nil || jb.Paid != 1200 || jb.Balance != 0 || !jb.Settled {
		t.Fatalf("jan GetBill = %+v %v, want paid 1200 settled", jb, err)
	}
	fb, err := s.GetBill("u", feb(2026))
	if err != nil || fb.Paid != 0 || fb.Balance != 3000 || fb.Settled {
		t.Fatalf("feb GetBill = %+v %v, want unpaid 3000", fb, err)
	}

	// 重排返回列表、替换其中元素乃至整张列表，下一次查询仍按账期先后展示。
	st.MonthlyUsage[0], st.MonthlyUsage[1] = st.MonthlyUsage[1], st.MonthlyUsage[0]
	st.MonthlyUsage[0] = Usage{Period: Month{Year: 2099, Month: time.December}, Total: 7}
	st.Bills = []BillSummary{{Period: Month{Year: 2099, Month: time.December}, TotalDue: 1, Paid: 1, Settled: true}}

	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertRealFixtureStatus(t, st, end)
}

// TestTwoStatusSnapshotsAreIndependent 验证同一账户分别取得的两份状态彼此
// 独立：重排、扩缩、替换第一份的用量/账单列表并改写其终止时刻，不改写第二份；
// 之后再改第二份，新查询与第一份各自保留自己的内容。
func TestTwoStatusSnapshotsAreIndependent(t *testing.T) {
	s, _, end := setupIndependenceFixture(t)

	a, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertRealFixtureStatus(t, a, end)
	assertRealFixtureStatus(t, b, end)

	// 全面篡改 a：原地改值、替换元素、倒序、追加虚假账期、改终止时刻。
	a.MonthlyUsage[0].Total = 11111
	a.MonthlyUsage[1] = Usage{Period: Month{Year: 2099, Month: time.December}, Total: 22222}
	a.MonthlyUsage[0], a.MonthlyUsage[1] = a.MonthlyUsage[1], a.MonthlyUsage[0]
	a.MonthlyUsage = append(a.MonthlyUsage, Usage{Period: Month{Year: 1999, Month: time.January}, Total: 33333})
	a.Bills[0].Paid, a.Bills[0].Balance, a.Bills[0].Settled = 500, 700, false
	a.Bills[1] = BillSummary{Period: Month{Year: 2099, Month: time.December}, TotalDue: 1}
	*a.ScheduledEnd = utc(2020, 1, 1, 0, 0)

	// b 与新查询都仍是真实数据。
	assertRealFixtureStatus(t, b, end)
	c, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertRealFixtureStatus(t, c, end)

	// 反向再改 b：c 与账户真实数据不变，a 保留自己被篡改掉的内容。
	b.MonthlyUsage[0].Total = 44444
	b.Bills = nil
	later := utc(2027, 1, 1, 0, 0)
	b.ScheduledEnd = &later
	c, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertRealFixtureStatus(t, c, end)
	if len(a.MonthlyUsage) != 3 || a.MonthlyUsage[0].Total != 22222 ||
		a.MonthlyUsage[1].Total != 11111 || a.MonthlyUsage[2].Total != 33333 {
		t.Fatalf("snapshot a changed after editing b: %+v", a.MonthlyUsage)
	}
	if !a.ScheduledEnd.Equal(utc(2020, 1, 1, 0, 0)) {
		t.Fatalf("snapshot a end changed after editing b: %v", *a.ScheduledEnd)
	}
}

// TestStatusSnapshotFreezesAcrossUsagePaymentAndUndoCancel 验证反方向的独立：
// 保存一份未改动的状态后，通过正常功能为尚未出账的三月补报用量、为二月账单
// 登记部分付款、再撤回取消——新查询显示更新后的累计用量、已付金额、余额与
// 空终止时刻；旧结果保留取得时的数值、付清状态与当时的终止安排。篡改旧结果
// 不能重新登记取消，也不能让已撤回的订阅在原定时刻终止。
func TestStatusSnapshotFreezesAcrossUsagePaymentAndUndoCancel(t *testing.T) {
	s, clk, end := setupIndependenceFixture(t)

	saved, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertRealFixtureStatus(t, saved, end)

	// 正常操作一：为尚未出账的三月（当前订阅覆盖）补报 8 单位。
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-mar", At: utc(2026, 3, 4, 0, 0), Quantity: 8})
	// 正常操作二：为二月账单登记部分付款 400，余额变 2600，仍未付清。
	pr, err := s.RecordPayment("u", "pay-feb-1", feb(2026), 400)
	if err != nil || pr.BillBalance != 2600 || pr.Settled {
		t.Fatalf("partial payment = %+v %v, want balance 2600 not settled", pr, err)
	}

	fresh, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.MonthlyUsage) != 3 ||
		fresh.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 12}) ||
		fresh.MonthlyUsage[1] != (Usage{Period: feb(2026), Total: 30}) ||
		fresh.MonthlyUsage[2] != (Usage{Period: mar(2026), Total: 8}) {
		t.Fatalf("fresh usage = %+v, want jan=12 feb=30 mar=8", fresh.MonthlyUsage)
	}
	fb := statusBillSummary(t, fresh, feb(2026))
	if fb.Paid != 400 || fb.Balance != 2600 || fb.Settled {
		t.Fatalf("fresh feb summary = %+v, want paid 400 balance 2600", fb)
	}

	// 旧结果保留取得时的数值与付清状态：没有三月用量，二月未付，一月仍付清。
	if len(saved.MonthlyUsage) != 2 ||
		statusUsageTotal(t, saved, jan(2026)) != 12 ||
		statusUsageTotal(t, saved, feb(2026)) != 30 {
		t.Fatalf("saved usage changed = %+v", saved.MonthlyUsage)
	}
	savedFeb := statusBillSummary(t, saved, feb(2026))
	if savedFeb.Paid != 0 || savedFeb.Balance != 3000 || savedFeb.Settled {
		t.Fatalf("saved feb summary changed = %+v", savedFeb)
	}

	// 正常操作三：撤回取消。新查询不再显示终止时刻，订阅继续有效。
	clk.t = utc(2026, 3, 6, 12, 0)
	if err := s.UndoCancelSubscription("u"); err != nil {
		t.Fatalf("undo cancel: %v", err)
	}
	fresh, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Subscribed || fresh.ScheduledEnd != nil {
		t.Fatalf("status after undo = %+v, want subscribed with nil end", fresh)
	}
	// 旧结果保留当时的安排。
	if saved.ScheduledEnd == nil || !saved.ScheduledEnd.Equal(end) {
		t.Fatalf("saved end changed after undo = %+v, want %v", saved.ScheduledEnd, end)
	}

	// 篡改旧结果：既不能重新登记取消，也不能改变撤回状态。
	*saved.ScheduledEnd = utc(2026, 3, 10, 0, 0)
	fakeLater := utc(2026, 12, 1, 0, 0)
	saved.ScheduledEnd = &fakeLater
	fresh, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Subscribed || fresh.ScheduledEnd != nil {
		t.Fatalf("tampering saved end registered a cancellation: %+v", fresh)
	}

	// 越过原定终止时刻：已撤回的订阅不在 4/1 终止，仍无终止安排。
	clk.t = utc(2026, 4, 2, 0, 0)
	fresh, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Subscribed {
		t.Fatalf("subscription ended at original time despite undo: %+v", fresh)
	}
	if fresh.ScheduledEnd != nil {
		t.Fatalf("unexpected scheduled end after undo: %v", *fresh.ScheduledEnd)
	}
	if fresh.CurrentTerms.PlanID != "a" {
		t.Fatalf("current terms after undo = %+v, want plan a", fresh.CurrentTerms)
	}
	// 旧结果继续表示取得时尚在等待取消。
	if !saved.Subscribed || saved.ScheduledEnd == nil || !saved.ScheduledEnd.Equal(fakeLater) {
		t.Fatalf("saved result mutated by later operations: %+v", saved)
	}
}

// TestCancellationEndsAtRegisteredTimeDespiteSnapshotTampering 验证未撤回的
// 取消严格按最初登记的终止时刻结束订阅：把旧结果中的终止时刻改早，订阅不会
// 提前结束；改晚，到达原定时刻仅查询状态也立即显示无有效订阅、无当前套餐、
// 无终止安排，历史用量与账单继续可查；旧结果定格在取得时的等待取消状态。
func TestCancellationEndsAtRegisteredTimeDespiteSnapshotTampering(t *testing.T) {
	s, clk, end := setupIndependenceFixture(t)

	saved, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	assertRealFixtureStatus(t, saved, end)

	// 把旧结果的终止时刻改到更早的 3/10：真实订阅不得提前终止。
	early := utc(2026, 3, 10, 0, 0)
	*saved.ScheduledEnd = early
	clk.t = utc(2026, 3, 12, 0, 0)
	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(end) {
		t.Fatalf("subscription ended at tampered early time: %+v", st)
	}
	if st.CurrentTerms.PlanID != "a" {
		t.Fatalf("current terms = %+v, want plan a while waiting", st.CurrentTerms)
	}

	// 再把旧结果的终止时刻改到更晚：原定时刻之前订阅仍然有效。
	fakeLate := utc(2026, 12, 1, 0, 0)
	saved.ScheduledEnd = &fakeLate
	clk.t = utc(2026, 3, 31, 23, 59)
	st, _ = s.Status("u")
	if !st.Subscribed || st.ScheduledEnd == nil || !st.ScheduledEnd.Equal(end) {
		t.Fatalf("status just before registered end = %+v", st)
	}

	// 到达原定终止时刻，即使只查询状态：无有效订阅、无当前套餐、无终止安排。
	clk.t = end
	st, _ = s.Status("u")
	if st.Subscribed {
		t.Fatalf("Subscribed = true at registered end, status = %+v", st)
	}
	if st.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("CurrentTerms = %+v at registered end, want zero value", st.CurrentTerms)
	}
	if st.ScheduledEnd != nil {
		t.Fatalf("ScheduledEnd = %v after end, want nil", *st.ScheduledEnd)
	}
	// 历史用量与账单继续可查，次序不变。
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0].Period != jan(2026) || st.MonthlyUsage[0].Total != 12 ||
		st.MonthlyUsage[1].Period != feb(2026) || st.MonthlyUsage[1].Total != 30 {
		t.Fatalf("history usage after end = %+v", st.MonthlyUsage)
	}
	if len(st.Bills) != 2 || st.Bills[0].Period != jan(2026) || st.Bills[1].Period != feb(2026) {
		t.Fatalf("history bills after end = %+v", st.Bills)
	}

	// 旧结果继续表示取得时尚在等待取消，不被后来的查询改写。
	if !saved.Subscribed || saved.ScheduledEnd == nil || !saved.ScheduledEnd.Equal(fakeLate) {
		t.Fatalf("saved waiting status changed = %+v", saved)
	}
}

// TestStatusWithoutCancellationKeepsEndNil 验证没有取消安排时 ScheduledEnd
// 为空，调用方在返回结果中凭空填入终止时刻不能制造取消安排：后续查询与跨月
// 后的查询都仍显示订阅有效、终止时刻为空。
func TestStatusWithoutCancellationKeepsEndNil(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-jan", At: utc(2026, 1, 20, 0, 0), Quantity: 12})
	clk.t = utc(2026, 2, 20, 12, 0)
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-feb", At: utc(2026, 2, 20, 0, 0), Quantity: 30})
	clk.t = utc(2026, 3, 1, 0, 0)
	mustBill(t, s, "u", jan(2026))
	mustBill(t, s, "u", feb(2026))

	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd != nil {
		t.Fatalf("status without cancellation = %+v, want subscribed nil end", st)
	}

	// 在返回结果中凭空填入终止时刻并篡改列表。
	fabricated := utc(2026, 4, 1, 0, 0)
	st.ScheduledEnd = &fabricated
	st.MonthlyUsage[0].Total = 1

	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd != nil {
		t.Fatalf("editing result fabricated a cancellation: %+v", st)
	}
	if statusUsageTotal(t, st, jan(2026)) != 12 {
		t.Fatalf("usage changed after editing result: %+v", st.MonthlyUsage)
	}

	// 跨月后仍无终止安排，订阅持续有效。
	clk.t = utc(2026, 5, 1, 0, 0)
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.ScheduledEnd != nil {
		t.Fatalf("fabricated end terminated the subscription: %+v", st)
	}
}
