package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“提前登记未来才开通的订阅后、等待期内的用量上报”提供回归保障。
// 能否接收一条事件，既要看事件是否已经发生（不晚于当前时刻），也要看事件发生
// 时刻是否属于某段实际订阅期间（开通计入、终止不计入）；账户保存了一份尚未生效
// 的订阅不构成接收资格。等待期内的任何拒绝都不得占用事件标识、不得累计数量、
// 不得提前激活订阅。

// TestFutureEventDuringWaitingRejectedThenAcceptedAtActivation 覆盖题述主线：
// 一月十日登记一月二十日十二点开通的订阅；等待期内提前上报发生于开通瞬间、
// 数量为三的事件返回 ErrEventInFuture；当前时刻恰好到达开通瞬间后，以原事件
// 标识、原发生时刻、原数量再次提交，作为首次接收成功并归入一月账期；原样重报
// 仍成功但不再次接收。等待期的拒绝不能占用事件标识，否则合法提交会被误当成重报。
func TestFutureEventDuringWaitingRejectedThenAcceptedAtActivation(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 500))
	mustAccount(t, s, "u")
	activateAt := utc(2026, 1, 20, 12, 0)
	mustSubscribe(t, s, "u", "a", activateAt)

	ev := Event{AccountID: "u", EventID: "evt-at-activation", At: activateAt, Quantity: 3}

	// 登记当天提前上报“发生于开通瞬间”的事件：此刻事件仍晚于当前时刻，
	// 必须按 ErrEventInFuture 拒绝，不能因为账户已保存订阅就接收。
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("early report at activation instant = %v, want ErrEventInFuture", err)
	}
	// 拒绝不累计数量：一月没有任何用量。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 0 {
		t.Fatalf("jan usage after rejected future event = %d, want 0", u.Total)
	}
	// 拒绝也不提前激活订阅：状态仍显示未生效且无任何用量记录。
	st, _ := s.Status("u")
	assertNotSubscribed(t, st)
	if len(st.MonthlyUsage) != 0 {
		t.Fatalf("monthly usage while waiting = %+v, want empty", st.MonthlyUsage)
	}

	// 开通前一分钟提交同一条“发生于开通瞬间”的事件：仍晚于当前时刻，依旧拒绝。
	clk.t = activateAt.Add(-time.Minute)
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("report one minute before activation = %v, want ErrEventInFuture", err)
	}
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 0 {
		t.Fatalf("jan usage after second rejection = %d, want 0", u.Total)
	}

	// 当前时刻恰好到达开通瞬间：以原标识、原时刻、原数量提交，首次接收成功，
	// 归入一月（UTC 自然月）账期。等待期的拒绝没有占用该事件标识。
	clk.t = activateAt
	r, err := s.RecordEvent(ev)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("first acceptance at activation instant: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 3 {
		t.Fatalf("jan usage after acceptance = %d, want 3", u.Total)
	}
	// 订阅此刻才生效，用量同时体现在账户查询中。
	st, _ = s.Status("u")
	if !st.Subscribed {
		t.Fatalf("Subscribed = false at activation instant: %+v", st)
	}
	if st.CurrentTerms.PlanID != "a" {
		t.Fatalf("CurrentTerms = %+v, want plan a snapshot", st.CurrentTerms)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 3}) {
		t.Fatalf("monthly usage at activation = %+v, want [2026-01 = 3]", st.MonthlyUsage)
	}

	// 此后原样重报仍成功，但表示没有再次接收：返回一月账期，数量保持为三。
	r2, err := s.RecordEvent(ev)
	if err != nil || r2.Accepted || r2.Period != jan(2026) {
		t.Fatalf("identical replay after activation: %+v %v", r2, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 3 {
		t.Fatalf("jan usage after replay = %d, want 3", u.Total)
	}
	st, _ = s.Status("u")
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Total != 3 {
		t.Fatalf("monthly usage after replay = %+v, want unchanged [2026-01 = 3]", st.MonthlyUsage)
	}
}

// TestPastEventBeforeActivationRejectedWithinSameMonth 锁定“事件已经发生却早于
// 订阅开通”的情形：开通之后补报当天 11:59 的事件必须返回
// ErrEventBeforeSubscription，不能因属于同一个 UTC 自然月而接收；开通时刻本身
// 包含在订阅期间内。被拒事件同样不占用标识、不累计数量。
func TestPastEventBeforeActivationRejectedWithinSameMonth(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s, planDef("a", 0, 100, 0, 0))
	mustAccount(t, s, "u")
	activateAt := utc(2026, 1, 20, 12, 0)
	mustSubscribe(t, s, "u", "a", activateAt)

	// 开通之后一分钟，补报当天 11:59 的事件：事件已经发生，但早于开通时刻。
	clk.t = activateAt.Add(time.Minute)
	before := Event{AccountID: "u", EventID: "late-before-open", At: utc(2026, 1, 20, 11, 59), Quantity: 2}
	if _, err := s.RecordEvent(before); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("backfill before activation = %v, want ErrEventBeforeSubscription", err)
	}
	// 开通前一纳秒同样不属于订阅期间。
	nanosBefore := Event{AccountID: "u", EventID: "ns-before-open", At: activateAt.Add(-time.Nanosecond), Quantity: 1}
	if _, err := s.RecordEvent(nanosBefore); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("one nanosecond before activation = %v, want ErrEventBeforeSubscription", err)
	}
	// 不能因为与开通时刻同属一个自然月就接收：一月累计仍为零。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 0 {
		t.Fatalf("jan usage after before-activation rejections = %d, want 0", u.Total)
	}

	// 开通时刻本身包含在订阅期间内：同分钟、同月份的合法事件接收成功。
	atExact := Event{AccountID: "u", EventID: "at-open", At: activateAt, Quantity: 5}
	r, err := s.RecordEvent(atExact)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("event exactly at activation: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 5 {
		t.Fatalf("jan usage = %d, want 5", u.Total)
	}

	// 被拒事件不占用标识：同一标识改报订阅期间内、且已经发生的合法时刻与数量，
	// 必须按全新事件接收，而不是 ErrEventConflict（若拒绝时落库则会冲突）。
	// 当前时刻为开通后一分钟，开通瞬间已是过去。
	reuse := Event{AccountID: "u", EventID: "late-before-open", At: activateAt, Quantity: 4}
	r2, err := s.RecordEvent(reuse)
	if err != nil || !r2.Accepted || r2.Period != jan(2026) {
		t.Fatalf("reuse rejected event id for valid event: %+v %v", r2, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 9 {
		t.Fatalf("jan usage after id reuse = %d, want 9", u.Total)
	}
}

// TestFutureActivationEventJudgedByInstantAcrossZones 验证时区不影响判定：
// 开通时刻、事件发生时刻与当前时刻分别用不同时区表示时，服务一律按实际瞬间
// 比较——等待期拒绝、开通瞬间接收、归入的 UTC 自然月均与全 UTC 表示一致。
func TestFutureActivationEventJudgedByInstantAcrossZones(t *testing.T) {
	plus1 := time.FixedZone("UTC+1", 60*60)
	activateUTC := utc(2026, 1, 20, 12, 0)

	s, clk := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "z")
	// 开通时刻用 +08:00 表示：本地 1 月 20 日 20:00 即 UTC 12:00。
	mustSubscribe(t, s, "z", "a", time.Date(2026, 1, 20, 20, 0, 0, 0, zonePlus8))

	// 事件时刻同样用 +08:00 表示开通瞬间：登记当天（UTC 1 月 10 日）提前上报，
	// 按实际瞬间判定仍是未来事件。
	ev := Event{
		AccountID: "z", EventID: "zoned-activation-event",
		At:       time.Date(2026, 1, 20, 20, 0, 0, 0, zonePlus8),
		Quantity: 3,
	}
	if !ev.At.Equal(activateUTC) {
		t.Fatalf("test setup: event instant %v != %v", ev.At.UTC(), activateUTC)
	}
	if _, err := s.RecordEvent(ev); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("zoned future event = %v, want ErrEventInFuture", err)
	}

	// 当前时刻改用 -05:00 表示：本地 07:00 == UTC 12:00，恰好到达开通瞬间。
	clk.t = time.Date(2026, 1, 20, 7, 0, 0, 0, zoneMinus5)
	// 事件时刻改用 +01:00 表示：本地 13:00 == UTC 12:00，与登记的开通时刻
	// 是同一瞬间，首次接收成功，归入一月 UTC 账期。
	evPlus1 := Event{
		AccountID: "z", EventID: "zoned-activation-event",
		At:       time.Date(2026, 1, 20, 13, 0, 0, 0, plus1),
		Quantity: 3,
	}
	if !evPlus1.At.Equal(activateUTC) {
		t.Fatalf("test setup: +01:00 event instant %v != %v", evPlus1.At.UTC(), activateUTC)
	}
	r, err := s.RecordEvent(evPlus1)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("zoned event at zoned activation instant: %+v %v", r, err)
	}

	// 开通前一分钟（本地 19:59 +08 == 11:59 UTC）的事件已经发生却早于订阅：
	// 即使与开通时刻同属一个自然月也拒绝。
	before := Event{
		AccountID: "z", EventID: "zoned-before",
		At:       time.Date(2026, 1, 20, 19, 59, 0, 0, zonePlus8),
		Quantity: 1,
	}
	if _, err := s.RecordEvent(before); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("zoned event before activation = %v, want ErrEventBeforeSubscription", err)
	}

	// 用纯 UTC 表示原样重报开通瞬间事件：同一实际瞬间，成功但不再次接收，
	// 账期仍为一月；一月累计始终为三。
	if r, err := s.RecordEvent(Event{
		AccountID: "z", EventID: "zoned-activation-event",
		At: activateUTC, Quantity: 3,
	}); err != nil || r.Accepted || r.Period != jan(2026) {
		t.Fatalf("utc replay of zoned activation event: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("z", jan(2026)); u.Total != 3 {
		t.Fatalf("jan usage across zones = %d, want 3", u.Total)
	}
	st, _ := s.Status("z")
	if !st.Subscribed || st.CurrentTerms.PlanID != "a" {
		t.Fatalf("status across zones = %+v, want subscribed plan a", st)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 3}) {
		t.Fatalf("monthly usage across zones = %+v, want [2026-01 = 3]", st.MonthlyUsage)
	}
}

// TestUsageBackfillDuringFutureResubscribeGap 覆盖重新开通等待期的同一规则：
// 旧订阅已于二月一日终止，账户提前登记二月十日十二点重新开通，二月五日仍处于
// 空档。账户未欠费、旧一月尚未出账时，补报旧订阅期间真实发生的用量成功，只
// 增加一月累计量，账户仍显示新订阅未生效；空档事件与指向未来开通瞬间的事件
// 都不能借未来登记获得接收资格。
func TestUsageBackfillDuringFutureResubscribeGap(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("c", 3000, 30, 300, 0))
	mustAccount(t, s, "g")
	mustSubscribe(t, s, "g", "a", utc(2026, 1, 1, 0, 0))
	if _, err := s.RecordEvent(Event{AccountID: "g", EventID: "jan-old", At: utc(2026, 1, 10, 0, 0), Quantity: 2}); err != nil {
		t.Fatal(err)
	}
	mustCancel(t, s, "g") // 旧订阅二月一日终止。

	// 二月二日旧订阅已终止：提前登记二月十日中午重新开通。
	clk.t = utc(2026, 2, 2, 9, 0)
	reopenAt := utc(2026, 2, 10, 12, 0)
	mustSubscribe(t, s, "g", "c", reopenAt)

	// 二月五日处于两段订阅之间的空档：新订阅未生效，账户也没有欠费停用。
	clk.t = utc(2026, 2, 5, 10, 0)
	st, _ := s.Status("g")
	assertNotSubscribed(t, st)
	if st.Suspended {
		t.Fatalf("account suspended during gap: %+v", st)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 2}) {
		t.Fatalf("gap usage before backfill = %+v, want [2026-01 = 2]", st.MonthlyUsage)
	}

	// 旧一月尚未出账：补报旧订阅期间（一月二十日）真实发生的用量，接收成功，
	// 归入一月账期，只增加一月累计量。
	backfill := Event{AccountID: "g", EventID: "jan-backfill", At: utc(2026, 1, 20, 8, 0), Quantity: 4}
	r, err := s.RecordEvent(backfill)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("backfill old subscription usage during gap: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("g", jan(2026)); u.Total != 6 {
		t.Fatalf("jan usage after backfill = %d, want 6", u.Total)
	}
	if u, _ := s.MonthlyUsage("g", feb(2026)); u.Total != 0 {
		t.Fatalf("feb usage after january backfill = %d, want 0", u.Total)
	}
	// 补报旧用量不改变等待状态：账户仍显示新订阅未生效，也没有被提前激活。
	st, _ = s.Status("g")
	assertNotSubscribed(t, st)
	if st.Suspended {
		t.Fatalf("account suspended after backfill: %+v", st)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 6}) {
		t.Fatalf("gap usage after backfill = %+v, want only [2026-01 = 6]", st.MonthlyUsage)
	}

	// 发生在二月空档内（二月三日，相对当前已是过去）的事件：以“不属于订阅
	// 期间”的既有错误拒绝，不能借未来登记获得接收资格。
	gapPast := Event{AccountID: "g", EventID: "gap-past", At: utc(2026, 2, 3, 8, 0), Quantity: 1}
	if _, err := s.RecordEvent(gapPast); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event in resubscribe gap = %v, want ErrEventBeforeSubscription", err)
	}
	// 指向未来重新开通瞬间的事件在此刻仍是未来事件，同样不得接收。
	atReopen := Event{AccountID: "g", EventID: "gap-at-reopen", At: reopenAt, Quantity: 1}
	if _, err := s.RecordEvent(atReopen); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("event at future reopen instant during gap = %v, want ErrEventInFuture", err)
	}
	// 两次拒绝都不留下用量：二月仍无累计，一月保持 6。
	if u, _ := s.MonthlyUsage("g", feb(2026)); u.Total != 0 {
		t.Fatalf("feb usage after gap rejections = %d, want 0", u.Total)
	}
	if u, _ := s.MonthlyUsage("g", jan(2026)); u.Total != 6 {
		t.Fatalf("jan usage changed after gap rejections = %d, want 6", u.Total)
	}

	// 到达重新开通瞬间：新订阅生效，历史用量只含一月；拒绝未提前激活任何东西。
	clk.t = reopenAt
	st, _ = s.Status("g")
	if !st.Subscribed {
		t.Fatalf("not subscribed at reopen instant: %+v", st)
	}
	if st.CurrentTerms.PlanID != "c" || st.CurrentTerms.MonthlyFee != 3000 {
		t.Fatalf("current terms at reopen = %+v, want plan c snapshot", st.CurrentTerms)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 6}) {
		t.Fatalf("usage at reopen instant = %+v, want only [2026-01 = 6]", st.MonthlyUsage)
	}

	// 一月补报事件原样重报：成功但不再次接收，账期仍是旧订阅的一月。
	if r, err := s.RecordEvent(backfill); err != nil || r.Accepted || r.Period != jan(2026) {
		t.Fatalf("replay january backfill after reopen: %+v %v", r, err)
	}
	// 空档事件在重新开通后仍不属于任何订阅期间：依旧拒绝。
	if _, err := s.RecordEvent(gapPast); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("gap event rechecked after reopen = %v, want ErrEventBeforeSubscription", err)
	}
	// 等待期以 ErrEventInFuture 被拒的同一事件标识未被占用：此刻合法提交，
	// 首次接收并归入二月账期。
	r2, err := s.RecordEvent(atReopen)
	if err != nil || !r2.Accepted || r2.Period != feb(2026) {
		t.Fatalf("rejected future-gap event accepted at reopen: %+v %v", r2, err)
	}
	if u, _ := s.MonthlyUsage("g", feb(2026)); u.Total != 1 {
		t.Fatalf("feb usage at reopen instant = %d, want 1", u.Total)
	}
	if u, _ := s.MonthlyUsage("g", jan(2026)); u.Total != 6 {
		t.Fatalf("jan usage changed after reopen = %d, want 6", u.Total)
	}

	// 此后为旧一月出账：账单体量旧订阅期间的套餐条件与全部一月用量（含空档期
	// 补报的 4，共 6），不被二月的新订阅覆盖。
	clk.t = utc(2026, 2, 11, 0, 0)
	janBill := mustBill(t, s, "g", jan(2026))
	if janBill.Terms.PlanID != "a" || janBill.TotalUsage != 6 {
		t.Fatalf("january bill after reopen = %+v, want plan a terms and usage 6", janBill)
	}
	st, _ = s.Status("g")
	if !st.Subscribed || st.CurrentTerms.PlanID != "c" {
		t.Fatalf("status after january bill = %+v, want subscribed plan c", st)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 6}) ||
		st.MonthlyUsage[1] != (Usage{Period: feb(2026), Total: 1}) {
		t.Fatalf("monthly usage after billing = %+v, want [2026-01 = 6, 2026-02 = 1]", st.MonthlyUsage)
	}
}
