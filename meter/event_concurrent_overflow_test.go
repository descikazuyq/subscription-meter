package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 本文件回归“两条标识不同的事件并发上报到同一账户、同一账期，合计用量越过
// int64 可保存上限”的行为。
//
// 这里的上限是 int64 最大值 maxInt64（9223372036854775807），与套餐包含额度
// 无关：超出套餐额度的用量仍按原规则记录。实现把“去重判定 → 时刻/账期/停用
// 校验 → 溢出检查 → 事件与累计落库”全部放在同一把互斥锁的一个临界区内，因此
// 两笔请求必然串行裁决：先到的一笔按当时累计落库，后到的一笔看到的是包含先到
// 一笔的真实累计，合计放不下时整次请求失败并返回 ErrOverflow，且失败发生在
// 落库之前，不占用事件标识、不增加累计用量。
//
// 这些测试锁住该行为，防止日后把“读取累计 → 溢出判断 → 写回”拆成多个步骤
// （TOCTOU）而出现：两笔都成功、累计越过 maxInt64、先前用量丢失、负数、失败
// 笔占用标识，或已接收事件的原样重报在累计到顶后被误判为溢出。可观察结果同时
// 覆盖上报返回、MonthlyUsage 与 Status 中的单月用量。

// eventOutcome 记录一笔并发上报的标识、时刻、数量与返回结果。
type eventOutcome struct {
	id  string
	at  time.Time
	qty int64
	r   EventResult
	err error
}

// setupNearCapAccount 在指定账户上把 2026-01 的累计用量垫到距 int64 上限只剩
// 10 个单位：账户已有实际生效的订阅、无账单（账期尚未出账）、无欠费，种子事件
// 此前从未接收过。返回种子累计值 maxInt64-10。
func setupNearCapAccount(t *testing.T, s *Service, acct string) int64 {
	t.Helper()
	mustAccount(t, s, acct)
	if err := s.Subscribe(acct, "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe %q: %v", acct, err)
	}
	base := maxInt64 - 10
	r, err := s.RecordEvent(Event{
		AccountID: acct,
		EventID:   "base-" + acct,
		At:        utc(2026, 1, 10, 0, 0),
		Quantity:  base,
	})
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("seed base usage %q: r=%+v err=%v", acct, r, err)
	}
	return base
}

// contendEventsOnce 用启动屏障让两条不同标识的事件在同一时刻争用同一账期的
// 累计值，返回与传入顺序一致的两笔结果。屏障保证两笔真正并发，而非顺序提交。
func contendEventsOnce(s *Service, acct string, first, second eventOutcome) []eventOutcome {
	reqs := []eventOutcome{first, second}
	res := make([]eventOutcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i, q := range reqs {
		go func(i int, q eventOutcome) {
			defer wg.Done()
			<-start
			r, err := s.RecordEvent(Event{
				AccountID: acct,
				EventID:   q.id,
				At:        q.at,
				Quantity:  q.qty,
			})
			res[i] = eventOutcome{id: q.id, at: q.at, qty: q.qty, r: r, err: err}
		}(i, q)
	}
	close(start)
	wg.Wait()
	return res
}

// statusMonthUsage 返回账户状态中指定账期的用量条目，要求该账期必须出现。
func statusMonthUsage(t *testing.T, s *Service, acct string, m Month) Usage {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	for _, u := range st.MonthlyUsage {
		if u.Period == m {
			return u
		}
	}
	t.Fatalf("status %q has no usage for %s: %+v", acct, m, st.MonthlyUsage)
	return Usage{}
}

// assertUsageAtCap 断言单月用量查询与账户状态中的该月用量都恰好等于 int64 上限，
// 且账户仍为已订阅、未停用。
func assertUsageAtCap(t *testing.T, s *Service, acct string) {
	t.Helper()
	u, err := s.MonthlyUsage(acct, jan(2026))
	if err != nil {
		t.Fatalf("monthly usage %q: %v", acct, err)
	}
	if u.Total != maxInt64 {
		t.Fatalf("%q monthly usage = %d, want %d", acct, u.Total, maxInt64)
	}
	if got := statusMonthUsage(t, s, acct, jan(2026)); got.Total != maxInt64 {
		t.Fatalf("%q status monthly usage = %d, want %d", acct, got.Total, maxInt64)
	}
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("%q status: subscribed=%v suspended=%v, want true/false", acct, st.Subscribed, st.Suspended)
	}
}

// TestConcurrentDistinctEventsOnlyOneWithinInt64Cap 覆盖主例子：
// 账期累计距 int64 上限只剩 10 单位时，7 单位与 8 单位两条不同标识的事件并发
// 到达。两笔单独看都能放下，合计（15）放不下：恰好一笔首次接收，另一笔整次
// 请求失败并可被 errors.Is 识别为 ErrOverflow。允许任意一笔先成功。
func TestConcurrentDistinctEventsOnlyOneWithinInt64Cap(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 20, 12, 0))
	// 套餐包含额度与本次断言无关：越过套餐额度的用量照常记录，只受 int64 上限约束。
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 0, IncludedUnits: 0, OveragePrice: 1, TaxRateBasisPoints: 0})

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("a%d", iter)
		base := setupNearCapAccount(t, s, acct)
		at7 := utc(2026, 1, 11, 0, 0)
		at8 := utc(2026, 1, 11, 0, 1)

		res := contendEventsOnce(s, acct,
			eventOutcome{id: "e7", at: at7, qty: 7},
			eventOutcome{id: "e8", at: at8, qty: 8},
		)

		// 恰好一笔首次接收成功、恰好一笔溢出失败；不允许两笔都成功，
		// 也不允许返回冲突等其它错误。
		var winner eventOutcome
		var loser *eventOutcome
		nAccepted, nOverflow := 0, 0
		for i := range res {
			oc := &res[i]
			switch {
			case oc.err == nil && oc.r.Accepted:
				nAccepted++
				winner = *oc
				if oc.r.Period != jan(2026) {
					t.Fatalf("iter %d accepted %q in wrong period: %+v", iter, oc.id, oc.r)
				}
			case errors.Is(oc.err, ErrOverflow):
				nOverflow++
				loser = oc
			default:
				t.Fatalf("iter %d event %q unexpected: r=%+v err=%v", iter, oc.id, oc.r, oc.err)
			}
		}
		if nAccepted != 1 || nOverflow != 1 || loser == nil {
			t.Fatalf("iter %d want exactly one accepted and one overflow, got %+v", iter, res)
		}

		// 成功接收的事件数量必须与最终累计值一致：原累计 + 成功一笔的数量。
		wantTotal := base + winner.qty
		if wantTotal < 0 || wantTotal > maxInt64 {
			t.Fatalf("iter %d winner %q drives total out of range: %d", iter, winner.id, wantTotal)
		}
		u, err := s.MonthlyUsage(acct, jan(2026))
		if err != nil {
			t.Fatalf("iter %d monthly usage: %v", iter, err)
		}
		if u.Total != wantTotal {
			t.Fatalf("iter %d monthly usage = %d, want %d", iter, u.Total, wantTotal)
		}
		if got := statusMonthUsage(t, s, acct, jan(2026)); got.Total != wantTotal {
			t.Fatalf("iter %d status monthly usage = %d, want %d", iter, got.Total, wantTotal)
		}

		// 溢出被拒的事件不占用标识：保留原标识与发生时刻，把数量改成当时尚可
		// 累计的剩余量再提交，应作为首次上报成功，使累计恰好达到上限——
		// 而不是被当成标识冲突或重复。
		remaining := maxInt64 - wantTotal
		topUp, err := s.RecordEvent(Event{
			AccountID: acct,
			EventID:   loser.id,
			At:        loser.at,
			Quantity:  remaining,
		})
		if errors.Is(err, ErrEventConflict) {
			t.Fatalf("iter %d overflowed event id %q was consumed: %v", iter, loser.id, err)
		}
		if err != nil || !topUp.Accepted || topUp.Period != jan(2026) {
			t.Fatalf("iter %d reuse rejected id %q for remaining %d: r=%+v err=%v",
				iter, loser.id, remaining, topUp, err)
		}
		assertUsageAtCap(t, s, acct)

		// 已成功接收的事件按原内容重报：未再次接收、保留原账期；即使此时累计
		// 已到上限，也不能报溢出或再次增加用量。
		replay, err := s.RecordEvent(Event{
			AccountID: acct,
			EventID:   winner.id,
			At:        winner.at,
			Quantity:  winner.qty,
		})
		if err != nil || replay.Accepted || replay.Period != jan(2026) {
			t.Fatalf("iter %d replay winner %q: r=%+v err=%v, want not accepted in Jan",
				iter, winner.id, replay, err)
		}
		assertUsageAtCap(t, s, acct)
	}
}

// TestConcurrentDistinctEventsExactlyReachInt64Cap 覆盖刚好能容纳的边界：
// 从距上限 10 单位的独立初始状态，并发提交 4 单位与 6 单位的不同标识事件，
// 两笔均应首次接收，最终累计恰好达到上限，不能把合法的边界合计误判为溢出。
func TestConcurrentDistinctEventsExactlyReachInt64Cap(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 0, IncludedUnits: 0, OveragePrice: 1, TaxRateBasisPoints: 0})

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("b%d", iter)
		base := setupNearCapAccount(t, s, acct)

		res := contendEventsOnce(s, acct,
			eventOutcome{id: "e4", at: utc(2026, 1, 11, 0, 0), qty: 4},
			eventOutcome{id: "e6", at: utc(2026, 1, 11, 0, 1), qty: 6},
		)
		for _, oc := range res {
			if oc.err != nil || !oc.r.Accepted || oc.r.Period != jan(2026) {
				t.Fatalf("iter %d event %q should be accepted in Jan: r=%+v err=%v",
					iter, oc.id, oc.r, oc.err)
			}
		}
		// 两笔合计恰为剩余 10 单位：累计恰好到顶。
		if want := base + 10; want != maxInt64 {
			t.Fatalf("iter %d expected total to reach cap, got %d", iter, want)
		}
		assertUsageAtCap(t, s, acct)

		// 到顶后两笔各自原样重报：仍按重复处理，不再次累计、也不报溢出。
		for _, oc := range res {
			replay, err := s.RecordEvent(Event{
				AccountID: acct,
				EventID:   oc.id,
				At:        oc.at,
				Quantity:  oc.qty,
			})
			if err != nil || replay.Accepted || replay.Period != jan(2026) {
				t.Fatalf("iter %d replay %q at cap: r=%+v err=%v, want not accepted in Jan",
					iter, oc.id, replay, err)
			}
		}
		assertUsageAtCap(t, s, acct)
	}
}

// TestDistinctEventOverflowBothOrdersDeterministic 以确定顺序锁住两种可能的
// 获胜结果（并发测试不保证每次都调度出两种先后顺序，这里把两条路径各自完整
// 走一遍）：失败笔不占标识，可保留原标识与时刻、改用当时剩余量首次接收并把
// 累计顶到上限；随后重报先成功的事件只返回未接收，不再次累计、不报溢出。
func TestDistinctEventOverflowBothOrdersDeterministic(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 0, IncludedUnits: 0, OveragePrice: 1, TaxRateBasisPoints: 0})
	at7 := utc(2026, 1, 11, 0, 0)
	at8 := utc(2026, 1, 11, 0, 1)

	// 顺序一：8 先成功（累计 base+8，剩余 2），7 溢出失败。
	acct8 := "big-first"
	base := setupNearCapAccount(t, s, acct8)
	r8, err := s.RecordEvent(Event{AccountID: acct8, EventID: "e8", At: at8, Quantity: 8})
	if err != nil || !r8.Accepted || r8.Period != jan(2026) {
		t.Fatalf("8 first: r=%+v err=%v", r8, err)
	}
	if _, err := s.RecordEvent(Event{AccountID: acct8, EventID: "e7", At: at7, Quantity: 7}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("7 after 8 should overflow: %v", err)
	}
	if u, _ := s.MonthlyUsage(acct8, jan(2026)); u.Total != base+8 {
		t.Fatalf("usage after overflow: %d", u.Total)
	}
	// e7 未被占用：保留标识与时刻，改数量为剩余 2，首次接收并恰好到顶。
	topUp, err := s.RecordEvent(Event{AccountID: acct8, EventID: "e7", At: at7, Quantity: 2})
	if err != nil || !topUp.Accepted || topUp.Period != jan(2026) {
		t.Fatalf("reuse e7 for remaining 2: r=%+v err=%v", topUp, err)
	}
	assertUsageAtCap(t, s, acct8)
	// 重报 e8：未再次接收、保留原账期，到顶也不报溢出。
	replay, err := s.RecordEvent(Event{AccountID: acct8, EventID: "e8", At: at8, Quantity: 8})
	if err != nil || replay.Accepted || replay.Period != jan(2026) {
		t.Fatalf("replay e8 at cap: r=%+v err=%v", replay, err)
	}
	assertUsageAtCap(t, s, acct8)

	// 顺序二：7 先成功（累计 base+7，剩余 3），8 溢出失败。
	acct7 := "small-first"
	base = setupNearCapAccount(t, s, acct7)
	r7, err := s.RecordEvent(Event{AccountID: acct7, EventID: "e7", At: at7, Quantity: 7})
	if err != nil || !r7.Accepted || r7.Period != jan(2026) {
		t.Fatalf("7 first: r=%+v err=%v", r7, err)
	}
	if _, err := s.RecordEvent(Event{AccountID: acct7, EventID: "e8", At: at8, Quantity: 8}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("8 after 7 should overflow: %v", err)
	}
	if u, _ := s.MonthlyUsage(acct7, jan(2026)); u.Total != base+7 {
		t.Fatalf("usage after overflow: %d", u.Total)
	}
	// e8 未被占用：保留标识与时刻，改数量为剩余 3，首次接收并恰好到顶。
	topUp, err = s.RecordEvent(Event{AccountID: acct7, EventID: "e8", At: at8, Quantity: 3})
	if err != nil || !topUp.Accepted || topUp.Period != jan(2026) {
		t.Fatalf("reuse e8 for remaining 3: r=%+v err=%v", topUp, err)
	}
	assertUsageAtCap(t, s, acct7)
	// 重报 e7：未再次接收、保留原账期，到顶也不报溢出。
	replay, err = s.RecordEvent(Event{AccountID: acct7, EventID: "e7", At: at7, Quantity: 7})
	if err != nil || replay.Accepted || replay.Period != jan(2026) {
		t.Fatalf("replay e7 at cap: r=%+v err=%v", replay, err)
	}
	assertUsageAtCap(t, s, acct7)
}
