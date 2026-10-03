package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“提前登记订阅后的等待期用量接收”补充端到端回归保障。
//
// 保留的既有规则：等待期内能否接收一条事件，既要看事件是否已经发生
// （晚于当前时刻一律 ErrEventInFuture），也要看事件发生时刻是否落在某段
// 实际订阅期间 [activatedAt, end)（开通瞬间计入），不能只看账户里是否保存了
// 一份订阅。等待期的所有拒绝都不累计用量、不提前激活订阅、不占用事件标识；
// 被拒标识在订阅真正生效后原样提交必须作为首次接收（Accepted=true）。
// 判定一律按实际瞬间，与开通时刻、事件时刻、当前时刻各自携带的时区无关，
// 账期只按事件实际瞬间归入 UTC 自然月。

// TestRecordEventDuringPendingActivationRejectsUntilInstantThenAccepts 对应题述
// 主链路：一月十日登记一月二十日十二点开通；等待期内提前上报“开通瞬间、数量三”
// 的事件必须返回 ErrEventInFuture；到达开通瞬间后以原标识、原时刻、原数量提交
// 为首次接收，归入一月账期、累计为三；此后原样重报成功但不再接收，数量保持三。
// 等待期拒绝不能占用事件标识，也不能让订阅提前生效。
func TestRecordEventDuringPendingActivationRejectsUntilInstantThenAccepts(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 500))
	mustAccount(t, s, "u")
	wantTerms := termsOf(planDef("a", 1000, 10, 100, 500))
	activateAt := utc(2026, 1, 20, 12, 0)
	mustSubscribe(t, s, "u", "a", activateAt)

	openEvent := Event{AccountID: "u", EventID: "open-qty-3", At: activateAt, Quantity: 3}

	// 一月十五日提前上报发生在开通瞬间的事件：当前时刻尚未到达，属于未来事件，
	// 不能因为账户已保存订阅或事件时刻恰好等于开通时刻就接收。
	clk.t = utc(2026, 1, 15, 12, 0)
	if _, err := s.RecordEvent(openEvent); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("open-instant event submitted while waiting = %v, want ErrEventInFuture", err)
	}
	// 既在未来、又早于开通时刻的事件：是否已经发生先于是否属于订阅期间判断，
	// 必须返回 ErrEventInFuture，而不是 ErrEventBeforeSubscription。
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "future-and-before-open",
		At: utc(2026, 1, 19, 12, 0), Quantity: 1,
	}); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("future event before activation = %v, want ErrEventInFuture", err)
	}
	// 拒绝不累计用量、不提前激活订阅，账户查询中也不留下任何账期用量。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 0 {
		t.Fatalf("january usage after future rejection = %d, want 0", u.Total)
	}
	st, _ := s.Status("u")
	assertNotSubscribed(t, st)
	if len(st.MonthlyUsage) != 0 {
		t.Fatalf("monthly usage after future rejection = %+v, want none", st.MonthlyUsage)
	}

	// 等待期内另一类事件：已经发生、但发生时刻早于实际开通（即使同一自然月）。
	// 不能因为账户保存了订阅就接收，也不能与“未来事件”混淆。
	clk.t = utc(2026, 1, 19, 12, 0)
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "already-happened-before-open",
		At: utc(2026, 1, 18, 12, 0), Quantity: 2,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("past event before activation = %v, want ErrEventBeforeSubscription", err)
	}

	// 开通前一分钟原样再报：仍是未来事件。
	clk.t = activateAt.Add(-time.Minute)
	if _, err := s.RecordEvent(openEvent); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("open-instant event one minute early = %v, want ErrEventInFuture", err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 0 {
		t.Fatalf("january usage before activation = %d, want 0", u.Total)
	}
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)

	// 当前时刻恰好到达开通瞬间：原标识、原时刻、原数量必须作为首次接收成功，
	// 返回一月账期。等待期的拒绝没有占用事件标识——若被误记为重报，
	// Accepted 会是 false。
	clk.t = activateAt
	r, err := s.RecordEvent(openEvent)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("first legitimate submit at activation instant = %+v %v, want accepted into 2026-01", r, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 3 {
		t.Fatalf("january usage after acceptance = %d, want 3", u.Total)
	}

	// 此后原样重报仍成功，但表示没有再次接收，数量保持不变。
	replay, err := s.RecordEvent(openEvent)
	if err != nil || replay.Accepted || replay.Period != jan(2026) {
		t.Fatalf("identical replay = %+v %v, want success with Accepted=false in 2026-01", replay, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 3 {
		t.Fatalf("january usage after replay = %d, want 3", u.Total)
	}

	// 结果同时体现在账户查询：订阅已生效并显示登记时的套餐快照，仅有一月累计三。
	st, _ = s.Status("u")
	if !st.Subscribed {
		t.Fatalf("Subscribed = false after activation instant, status = %+v", st)
	}
	if st.CurrentTerms != wantTerms {
		t.Fatalf("CurrentTerms = %+v, want snapshot %+v", st.CurrentTerms, wantTerms)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 3}) {
		t.Fatalf("status monthly usage = %+v, want [2026-01 = 3]", st.MonthlyUsage)
	}
	if st.Suspended || len(st.Bills) != 0 {
		t.Fatalf("unexpected suspension or bills: %+v", st)
	}
}

// TestEventBeforeActivationInstantRejectedEvenSameMonth 锁定“事件已经发生却早于
// 订阅开通”的混淆情形：开通之后补报当天 11:59 的事件必须返回
// ErrEventBeforeSubscription，不能因为与开通时刻同属一个自然月就接收；
// 开通时刻本身包含在订阅期间内。被拒事件不占用标识（原样再报仍是同一错误，
// 而不是 ErrEventConflict），只累计开通瞬间那条。
func TestEventBeforeActivationInstantRejectedEvenSameMonth(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 10, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustAccount(t, s, "u")
	activateAt := utc(2026, 1, 20, 12, 0)
	mustSubscribe(t, s, "u", "a", activateAt)

	// 开通之后补报当天 11:59 的事件：已发生、同一自然月，但早于开通瞬间。
	clk.t = utc(2026, 1, 20, 12, 30)
	before := Event{AccountID: "u", EventID: "same-month-1159", At: utc(2026, 1, 20, 11, 59), Quantity: 3}
	if _, err := s.RecordEvent(before); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("11:59 event after activation = %v, want ErrEventBeforeSubscription", err)
	}
	// 原样再报仍是“早于订阅”，不能因为上一次拒绝而变成标识冲突：拒绝未占用标识。
	if _, err := s.RecordEvent(before); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("replay rejected 11:59 event = %v, want ErrEventBeforeSubscription (not conflict)", err)
	}
	// 月初事件与开通同属一月，同样不能借“同一自然月”获得接收资格。
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "same-month-jan1",
		At: utc(2026, 1, 1, 0, 0), Quantity: 1,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("january-1 event = %v, want ErrEventBeforeSubscription", err)
	}
	// 开通瞬间前一纳秒仍不属于订阅期间。
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "one-ns-before",
		At: activateAt.Add(-time.Nanosecond), Quantity: 1,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("one nanosecond before activation = %v, want ErrEventBeforeSubscription", err)
	}

	// 开通时刻本身包含在订阅期间内：同一瞬间的事件首次接收，归入一月。
	atInstant := Event{AccountID: "u", EventID: "exactly-noon", At: activateAt, Quantity: 3}
	r, err := s.RecordEvent(atInstant)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("event at activation instant = %+v %v, want accepted into 2026-01", r, err)
	}
	if r2, err := s.RecordEvent(atInstant); err != nil || r2.Accepted || r2.Period != jan(2026) {
		t.Fatalf("replay at-instant event = %+v %v, want Accepted=false replay", r2, err)
	}

	// 被拒事件一律未累计：一月用量只有开通瞬间的三。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 3 {
		t.Fatalf("january usage = %d, want 3", u.Total)
	}
	st, _ := s.Status("u")
	if !st.Subscribed || len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Total != 3 {
		t.Fatalf("status = %+v, want subscribed with only january 3", st)
	}
}

// TestPendingActivationEventDecisionByInstantAcrossZones 验证时区无关性：
// 开通时刻取 UTC 2026-02-01 00:30（-05:00 时区的本地钟面仍是 1 月 31 日）。
// 无论开通时刻、事件时刻与当前时刻用哪种时区表示，接收/拒绝结果只按实际瞬间，
// 归入的 UTC 自然月始终是二月。
func TestPendingActivationEventDecisionByInstantAcrossZones(t *testing.T) {
	activate := utc(2026, 2, 1, 0, 30)
	zones := []struct {
		name string
		loc  *time.Location
	}{
		{"utc", time.UTC},
		{"plus8", zonePlus8},
		{"minus5", zoneMinus5},
	}
	for _, z := range zones {
		t.Run(z.name, func(t *testing.T) {
			s, clk := newTestService(utc(2026, 1, 20, 12, 0))
			mustPlan(t, s, planDef("p", 1000, 10, 100, 0))
			mustAccount(t, s, "u")
			// 开通时刻用本时区表示，三种表示必须是同一实际瞬间。
			activateLocal := activate.In(z.loc)
			if !activateLocal.Equal(activate) {
				t.Fatalf("test setup: %s activation %v != %v", z.name, activateLocal, activate)
			}
			mustSubscribe(t, s, "u", "p", activateLocal)

			open := Event{AccountID: "u", EventID: "at-open", At: activateLocal, Quantity: 3}

			// 开通前一分钟：事件就发生在开通瞬间，但对当前时刻仍是未来。
			clk.t = activate.Add(-time.Minute)
			if _, err := s.RecordEvent(open); !errors.Is(err, ErrEventInFuture) {
				t.Fatalf("%s: open-instant event one minute before = %v, want ErrEventInFuture", z.name, err)
			}
			// 同一时钟点：已发生但早于开通一分钟的事件按“早于订阅”拒绝。
			if _, err := s.RecordEvent(Event{
				AccountID: "u", EventID: "already-past-before-open",
				At: activate.Add(-2 * time.Minute).In(z.loc), Quantity: 1,
			}); !errors.Is(err, ErrEventBeforeSubscription) {
				t.Fatalf("%s: past-before-open event = %v, want ErrEventBeforeSubscription", z.name, err)
			}
			if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 0 {
				t.Fatalf("%s: february usage while waiting = %d, want 0", z.name, u.Total)
			}

			// 到达开通瞬间：首次接收，账期按实际瞬间归入 UTC 二月
			// （-05:00 表示下本地钟面仍停在 1 月 31 日，也必须是二月）。
			clk.t = activate
			r, err := s.RecordEvent(open)
			if err != nil || !r.Accepted || r.Period != feb(2026) {
				t.Fatalf("%s: event at activation = %+v %v, want accepted into 2026-02", z.name, r, err)
			}
			// 换用第三种时区表示同一瞬间原样重报：仍是同一事件，不再接收，账期不变。
			r2, err := s.RecordEvent(Event{
				AccountID: "u", EventID: "at-open",
				At: activate.In(zonePlus530), Quantity: 3,
			})
			if err != nil || r2.Accepted || r2.Period != feb(2026) {
				t.Fatalf("%s: replay in third timezone = %+v %v, want Accepted=false, 2026-02", z.name, r2, err)
			}

			// 开通后补报开通前一分钟（在 -05:00 下是本地 1 月 31 日钟面）：
			// 实际瞬间早于开通，拒绝；账期归属在此没有发言权。
			clk.t = activate.Add(2 * time.Hour)
			early := Event{
				AccountID: "u", EventID: "just-before",
				At: activate.Add(-time.Minute).In(z.loc), Quantity: 1,
			}
			if _, err := s.RecordEvent(early); !errors.Is(err, ErrEventBeforeSubscription) {
				t.Fatalf("%s: one-minute-before backfill = %v, want ErrEventBeforeSubscription", z.name, err)
			}

			// 反向情形：-05:00 本地钟面仍为 1 月 31 日 23:59，实际瞬间已是
			// UTC 2 月 1 日 04:59，落在订阅期间内：接收并归入 UTC 二月。
			wallJanuary := time.Date(2026, time.January, 31, 23, 59, 0, 0, zoneMinus5)
			if MonthOf(wallJanuary) != feb(2026) {
				t.Fatalf("test setup: %v is not 2026-02 UTC", wallJanuary.UTC())
			}
			// 时钟推进到该事件的实际瞬间：恰在当前时刻发生不算未来事件。
			clk.t = wallJanuary.UTC()
			r3, err := s.RecordEvent(Event{
				AccountID: "u", EventID: "wall-january-instant",
				At: wallJanuary, Quantity: 2,
			})
			if err != nil || !r3.Accepted || r3.Period != feb(2026) {
				t.Fatalf("%s: local-January wall event = %+v %v, want accepted into 2026-02", z.name, r3, err)
			}

			// 三种时区表示下最终用量完全一致：二月累计 3+2=5，一月为零。
			if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 5 {
				t.Fatalf("%s: february usage = %d, want 5", z.name, u.Total)
			}
			if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 0 {
				t.Fatalf("%s: january usage = %d, want 0", z.name, u.Total)
			}
		})
	}
}

// TestFutureResubscribeGapBackfillsOldPeriodOnly 验证重新开通场景的同一规则：
// 旧订阅二月一日终止，账户提前登记二月十日十二点重新开通；二月五日处于空档。
// 账户未欠费、旧一月尚未出账时，补报旧订阅期间真实发生的用量成功并只增加一月
// 累计；空档内（含旧订阅终止时刻本身）与未来开通瞬间的事件分别按既有错误拒绝，
// 不能借未来登记获得接收资格。到达新开通瞬间后，等待期被拒的标识仍可首次接收，
// 旧月份归属与用量不被新订阅覆盖。
func TestFutureResubscribeGapBackfillsOldPeriodOnly(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 10, 100, 0))
	mustPlan(t, s, planDef("c", 3000, 30, 300, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))
	if r, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "jan-seed", At: utc(2026, 1, 10, 0, 0), Quantity: 4,
	}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("seed january usage: %+v %v", r, err)
	}
	mustCancel(t, s, "u") // 旧订阅 2026-02-01 00:00 终止。

	// 二月一日终止即落入历史；二月五日登记二月十日中午重新开通，此刻处于空档。
	clk.t = utc(2026, 2, 5, 12, 0)
	reopenAt := utc(2026, 2, 10, 12, 0)
	mustSubscribe(t, s, "u", "c", reopenAt)

	st, _ := s.Status("u")
	assertNotSubscribed(t, st)
	if st.Suspended {
		t.Fatalf("account suspended without any billed debt: %+v", st)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 4}) {
		t.Fatalf("gap status usage = %+v, want january history of 4", st.MonthlyUsage)
	}

	// 旧一月尚未出账：补报旧订阅期间真实发生（一月二十五日）的用量成功，
	// 只增加一月累计，账户仍显示新订阅未生效。
	backfill := Event{AccountID: "u", EventID: "jan-backfill", At: utc(2026, 1, 25, 10, 0), Quantity: 5}
	r, err := s.RecordEvent(backfill)
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("backfill old-subscription usage during gap: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 9 {
		t.Fatalf("january usage after backfill = %d, want 9", u.Total)
	}
	if r2, err := s.RecordEvent(backfill); err != nil || r2.Accepted || r2.Period != jan(2026) {
		t.Fatalf("backfill replay = %+v %v, want Accepted=false replay", r2, err)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 9 {
		t.Fatalf("january usage after backfill replay = %d, want 9", u.Total)
	}
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)
	if st.Suspended {
		t.Fatalf("unexpected suspension after backfill: %+v", st)
	}

	// 空档内已经发生的事件：不属于任何订阅期间，按既有错误拒绝，
	// 不能借未来登记获得接收资格。
	gapEvent := Event{AccountID: "u", EventID: "feb-gap", At: utc(2026, 2, 5, 9, 0), Quantity: 1}
	if _, err := s.RecordEvent(gapEvent); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("gap event = %v, want ErrEventBeforeSubscription", err)
	}
	// 拒绝不占用标识：原样再报仍是同一错误而非冲突。
	if _, err := s.RecordEvent(gapEvent); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("gap event replay = %v, want ErrEventBeforeSubscription (not conflict)", err)
	}
	// 旧订阅终止时刻本身（二月一日零点）不计入旧订阅期间，也早于新订阅：拒绝。
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "at-old-end", At: utc(2026, 2, 1, 0, 0), Quantity: 1,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event at old subscription end instant = %v, want ErrEventBeforeSubscription", err)
	}
	// 发生在新订阅开通瞬间的事件此刻仍在未来：先按未来事件拒绝，不得提前接收。
	futureOpen := Event{AccountID: "u", EventID: "at-reopen", At: reopenAt, Quantity: 1}
	if _, err := s.RecordEvent(futureOpen); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("new-subscription open event in gap = %v, want ErrEventInFuture", err)
	}

	// 空档拒绝不改变任何用量：一月仍是九，二月为零，账户查询不出现二月账期。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 9 {
		t.Fatalf("january usage after gap rejections = %d, want 9", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 0 {
		t.Fatalf("february usage after gap rejections = %d, want 0", u.Total)
	}
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0].Period != jan(2026) {
		t.Fatalf("status usage after gap rejections = %+v, want january only", st.MonthlyUsage)
	}

	// 新开通前一分钟：当时刻发生的事件仍在空档，拒绝。
	clk.t = reopenAt.Add(-time.Minute)
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "minute-before-reopen", At: reopenAt.Add(-time.Minute), Quantity: 1,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("event one minute before reopen = %v, want ErrEventBeforeSubscription", err)
	}
	st, _ = s.Status("u")
	assertNotSubscribed(t, st)

	// 到达重新开通瞬间：新订阅生效，使用登记时保存的套餐丙快照；
	// 等待期被未来错误拒绝的标识没有被占用，此刻原标识/时刻/数量首次接收入二月。
	clk.t = reopenAt
	r3, err := s.RecordEvent(futureOpen)
	if err != nil || !r3.Accepted || r3.Period != feb(2026) {
		t.Fatalf("open event resubmitted at reopen instant = %+v %v, want accepted into 2026-02", r3, err)
	}
	if r4, err := s.RecordEvent(futureOpen); err != nil || r4.Accepted || r4.Period != feb(2026) {
		t.Fatalf("reopen event replay = %+v %v, want Accepted=false replay", r4, err)
	}
	st, _ = s.Status("u")
	wantCTerms := termsOf(planDef("c", 3000, 30, 300, 0))
	if !st.Subscribed || st.CurrentTerms != wantCTerms {
		t.Fatalf("status at reopen instant = %+v, want subscribed with plan c snapshot", st)
	}

	// 新订阅生效后仍可补报旧订阅期间、尚未出账的一月用量，归一月而不是二月。
	if r5, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "jan-late-backfill", At: utc(2026, 1, 26, 0, 0), Quantity: 2,
	}); err != nil || !r5.Accepted || r5.Period != jan(2026) {
		t.Fatalf("backfill january after reopen: %+v %v", r5, err)
	}
	// 二月五日的空档事件在新订阅生效后补报依旧拒绝：空档不会被新订阅追溯覆盖。
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "feb-gap-late-report", At: utc(2026, 2, 5, 9, 0), Quantity: 1,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("gap event reported after reopen = %v, want ErrEventBeforeSubscription", err)
	}

	// 最终：一月 4+5+2=11，二月只有开通瞬间的 1；新订阅不覆盖旧月份归属。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 11 {
		t.Fatalf("january usage = %d, want 11", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 1 {
		t.Fatalf("february usage = %d, want 1", u.Total)
	}
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms != wantCTerms {
		t.Fatalf("final status subscription = %+v, want active plan c", st)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 11}) ||
		st.MonthlyUsage[1] != (Usage{Period: feb(2026), Total: 1}) {
		t.Fatalf("final monthly usage = %+v, want [2026-01 = 11, 2026-02 = 1]", st.MonthlyUsage)
	}
}
