package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 本文件回归“两条不同标识的用量事件并发上报同一账户、同一账期，
// 累计值接近 int64 可保存上限（9223372036854775807）”的行为。
//
// 这里的上限是数值存储上限，与套餐包含额度无关：超出套餐额度仍按原规则
// 记录，只有累计值装不下整次请求时才以 ErrOverflow 整单拒绝。实现把
// “去重判定 → 各类资格校验 → 溢出检查 → 事件与累计值落库”全部放在同一把
// 互斥锁的临界区内，因此并发上报必然串行裁决：先到的一笔看到当时的真实
// 累计值，后到的一笔看到先到一笔落库后的累计值。
//
// 这些测试锁住该行为，防止日后把溢出检查与累计拆成两个步骤（TOCTOU）而出现：
// 两笔都成功导致越过上限或回绕为负、丢失先前累计、失败的一笔占用事件标识，
// 或已满上限后把完全相同重报误判为溢出。

// eventOutcome 记录一条并发上报事件的标识、数量与返回结果。
type eventOutcome struct {
	id  string
	at  time.Time
	qty int64
	r   EventResult
	err error
}

// mustUsageNearLimit 在指定账户上准备“2026-01 账期累计值距 int64 上限
// 只剩 headroom 单位”的初始状态：开通 2026-01-01 起生效的订阅，
// 上报一条数量为 maxInt64-headroom 的种子事件，并校验累计值落位。
func mustUsageNearLimit(t *testing.T, s *Service, acct string, headroom int64) {
	t.Helper()
	mustAccount(t, s, acct)
	if err := s.Subscribe(acct, "plan-metered", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe %q: %v", acct, err)
	}
	seed := maxInt64 - headroom
	r, err := s.RecordEvent(Event{
		AccountID: acct,
		EventID:   "seed",
		At:        utc(2026, 1, 5, 0, 0),
		Quantity:  seed,
	})
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("seed event %q: r=%+v err=%v", acct, r, err)
	}
	if u := mustMonthlyUsage(t, s, acct); u != seed {
		t.Fatalf("seed usage %q: got %d, want %d", acct, u, seed)
	}
}

// mustMonthlyUsage 返回账户 2026-01 账期的累计用量。
func mustMonthlyUsage(t *testing.T, s *Service, acct string) int64 {
	t.Helper()
	u, err := s.MonthlyUsage(acct, jan(2026))
	if err != nil {
		t.Fatalf("monthly usage %q: %v", acct, err)
	}
	return u.Total
}

// statusMonthlyUsage 返回账户状态中 2026-01 账期的累计用量，
// 测试断言其与单月用量查询一致。
func statusMonthlyUsage(t *testing.T, s *Service, acct string) int64 {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	for _, u := range st.MonthlyUsage {
		if u.Period == jan(2026) {
			return u.Total
		}
	}
	t.Fatalf("status %q has no Jan usage: %+v", acct, st.MonthlyUsage)
	return 0
}

// contendEvents 用启动屏障让两条不同标识的事件在同一时刻上报同一账户，
// 返回与传入顺序一致的两条结果。屏障保证两条真正并发，而非顺序提交。
func contendEvents(s *Service, acct string, first, second eventOutcome) []eventOutcome {
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

// TestConcurrentEventsNearInt64LimitOnlyOneFits 覆盖主场景：
// 2026-01 累计值距 int64 上限只剩 10 单位时，两条标识不同、数量 7 与 8 的
// 事件并发上报。它们单独看都放得下，但合计放不下：恰好一条首次接收，
// 另一条返回可识别为 ErrOverflow 的错误。允许任意一条先成功，不指定胜者。
func TestConcurrentEventsNearInt64LimitOnlyOneFits(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "plan-metered", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 1, TaxRateBasisPoints: 0})

	const seed = maxInt64 - 10
	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("a%d", iter)
		mustUsageNearLimit(t, s, acct, 10)

		res := contendEvents(s, acct,
			eventOutcome{id: "e7", at: utc(2026, 1, 10, 10, 0), qty: 7},
			eventOutcome{id: "e8", at: utc(2026, 1, 10, 11, 0), qty: 8},
		)

		// 恰好一条首次接收且归入 2026-01 账期，恰好一条整单溢出被拒；
		// 不允许两条都成功，也不允许返回标识冲突等其它错误。
		var winner eventOutcome
		var loser *eventOutcome
		nAccepted, nOverflow := 0, 0
		for i := range res {
			oc := &res[i]
			switch {
			case oc.err == nil && oc.r.Accepted:
				if oc.r.Period != jan(2026) {
					t.Fatalf("iter %d accepted event %q period: %+v", iter, oc.id, oc.r)
				}
				nAccepted++
				winner = *oc
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

		// 成功返回的事件数量必须与最终累计值一致：累计值 = 原值 + 胜者数量。
		// 不能两笔都成功、不能丢失先前用量、不能为负或越过上限。
		want := seed + winner.qty
		if got := mustMonthlyUsage(t, s, acct); got != want {
			t.Fatalf("iter %d usage after contention: got %d, want %d (winner %q qty %d)",
				iter, got, want, winner.id, winner.qty)
		}
		if got := statusMonthlyUsage(t, s, acct); got != want {
			t.Fatalf("iter %d status usage diverges: got %d, want %d", iter, got, want)
		}

		// 溢出被拒的事件不占用标识、不增加累计：保留原标识与发生时刻，
		// 把数量改成当时尚可累计的剩余量再提交，应作为首次上报成功，
		// 使累计值恰好达到上限，而不是被当成冲突或重复。
		remaining := maxInt64 - want
		topUp, err := s.RecordEvent(Event{
			AccountID: acct,
			EventID:   loser.id,
			At:        loser.at,
			Quantity:  remaining,
		})
		if errors.Is(err, ErrEventConflict) {
			t.Fatalf("iter %d rejected event id %q was consumed: %v", iter, loser.id, err)
		}
		if err != nil || !topUp.Accepted || topUp.Period != jan(2026) {
			t.Fatalf("iter %d reuse rejected id %q for remaining %d: r=%+v err=%v",
				iter, loser.id, remaining, topUp, err)
		}
		if got := mustMonthlyUsage(t, s, acct); got != maxInt64 {
			t.Fatalf("iter %d usage after top-up: got %d, want %d (maxInt64)", iter, got, maxInt64)
		}
		if got := statusMonthlyUsage(t, s, acct); got != maxInt64 {
			t.Fatalf("iter %d status usage after top-up: got %d, want %d", iter, got, maxInt64)
		}

		// 将已经成功接收的事件按原内容重报：仍返回未再次接收并保留原账期；
		// 即使累计值已到上限，也不能因此报溢出或再次增加用量。
		replay, err := s.RecordEvent(Event{
			AccountID: acct,
			EventID:   winner.id,
			At:        winner.at,
			Quantity:  winner.qty,
		})
		if err != nil || replay.Accepted || replay.Period != jan(2026) {
			t.Fatalf("iter %d replay winner %q at limit: r=%+v err=%v, want accepted=false period=2026-01",
				iter, winner.id, replay, err)
		}
		if got := mustMonthlyUsage(t, s, acct); got != maxInt64 {
			t.Fatalf("iter %d usage after replay: got %d, want %d (unchanged)", iter, got, maxInt64)
		}
		if got := statusMonthlyUsage(t, s, acct); got != maxInt64 {
			t.Fatalf("iter %d status usage after replay: got %d, want %d", iter, got, maxInt64)
		}
	}
}

// TestConcurrentEventsExactlyReachInt64Limit 覆盖刚好能容纳的边界：
// 从距上限 10 单位的独立初始状态，并发上报标识不同的 4 单位与 6 单位事件，
// 两笔均应首次接收，最终累计值恰好达到上限；不能把合法上报误判为溢出。
func TestConcurrentEventsExactlyReachInt64Limit(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "plan-metered", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 1, TaxRateBasisPoints: 0})

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("b%d", iter)
		mustUsageNearLimit(t, s, acct, 10)

		res := contendEvents(s, acct,
			eventOutcome{id: "e4", at: utc(2026, 1, 10, 10, 0), qty: 4},
			eventOutcome{id: "e6", at: utc(2026, 1, 10, 11, 0), qty: 6},
		)

		// 两笔都必须首次接收并归入 2026-01 账期：合计恰达上限不是溢出。
		for _, oc := range res {
			if oc.err != nil || !oc.r.Accepted || oc.r.Period != jan(2026) {
				t.Fatalf("iter %d event %q should be accepted: r=%+v err=%v", iter, oc.id, oc.r, oc.err)
			}
		}

		// 最终累计值恰好等于上限，单月用量查询与账户状态一致。
		if got := mustMonthlyUsage(t, s, acct); got != maxInt64 {
			t.Fatalf("iter %d usage: got %d, want %d (maxInt64)", iter, got, maxInt64)
		}
		if got := statusMonthlyUsage(t, s, acct); got != maxInt64 {
			t.Fatalf("iter %d status usage: got %d, want %d", iter, got, maxInt64)
		}
	}
}

// TestEventOverflowContentionBothOrdersDeterministic 以确定的顺序锁住两种可能
// 的获胜结果（并发测试不保证每次都调度出两种顺序，这里把两条路径各自完整走一遍）：
// 失败笔不占标识，可保留原标识与时刻、改数量为剩余量首次上报并恰达上限；
// 随后原样重报成功笔只返回未再次接收，不再次累计也不报溢出。
func TestEventOverflowContentionBothOrdersDeterministic(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "plan-metered", MonthlyFee: 100, IncludedUnits: 10, OveragePrice: 1, TaxRateBasisPoints: 0})

	// 顺序一：7 先成功（累计 maxInt64-3），8 溢出失败。
	mustUsageNearLimit(t, s, "seven-first", 10)
	r7, err := s.RecordEvent(Event{AccountID: "seven-first", EventID: "e7", At: utc(2026, 1, 10, 10, 0), Quantity: 7})
	if err != nil || !r7.Accepted || r7.Period != jan(2026) {
		t.Fatalf("7 first: r=%+v err=%v", r7, err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "seven-first", EventID: "e8", At: utc(2026, 1, 10, 11, 0), Quantity: 8}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("8 after 7 should overflow: %v", err)
	}
	if got := mustMonthlyUsage(t, s, "seven-first"); got != maxInt64-3 {
		t.Fatalf("seven-first usage: got %d, want %d", got, maxInt64-3)
	}
	// e8 未被占用：保留标识与时刻，改数量 3 首次成功，恰达上限。
	topUp, err := s.RecordEvent(Event{AccountID: "seven-first", EventID: "e8", At: utc(2026, 1, 10, 11, 0), Quantity: 3})
	if err != nil || !topUp.Accepted || topUp.Period != jan(2026) {
		t.Fatalf("reuse e8 for 3: r=%+v err=%v", topUp, err)
	}
	// 重报 e7：未再次接收、保留原账期，已到上限也不报溢出、不再累计。
	replay, err := s.RecordEvent(Event{AccountID: "seven-first", EventID: "e7", At: utc(2026, 1, 10, 10, 0), Quantity: 7})
	if err != nil || replay.Accepted || replay.Period != jan(2026) {
		t.Fatalf("replay e7: r=%+v err=%v", replay, err)
	}
	if got := mustMonthlyUsage(t, s, "seven-first"); got != maxInt64 {
		t.Fatalf("seven-first final usage: got %d, want %d", got, maxInt64)
	}
	if got := statusMonthlyUsage(t, s, "seven-first"); got != maxInt64 {
		t.Fatalf("seven-first final status usage: got %d, want %d", got, maxInt64)
	}

	// 顺序二：8 先成功（累计 maxInt64-2），7 溢出失败。
	mustUsageNearLimit(t, s, "eight-first", 10)
	r8, err := s.RecordEvent(Event{AccountID: "eight-first", EventID: "e8", At: utc(2026, 1, 10, 11, 0), Quantity: 8})
	if err != nil || !r8.Accepted || r8.Period != jan(2026) {
		t.Fatalf("8 first: r=%+v err=%v", r8, err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "eight-first", EventID: "e7", At: utc(2026, 1, 10, 10, 0), Quantity: 7}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("7 after 8 should overflow: %v", err)
	}
	if got := mustMonthlyUsage(t, s, "eight-first"); got != maxInt64-2 {
		t.Fatalf("eight-first usage: got %d, want %d", got, maxInt64-2)
	}
	// e7 未被占用：保留标识与时刻，改数量 2 首次成功，恰达上限。
	topUp, err = s.RecordEvent(Event{AccountID: "eight-first", EventID: "e7", At: utc(2026, 1, 10, 10, 0), Quantity: 2})
	if err != nil || !topUp.Accepted || topUp.Period != jan(2026) {
		t.Fatalf("reuse e7 for 2: r=%+v err=%v", topUp, err)
	}
	// 重报 e8：未再次接收、保留原账期，已到上限也不报溢出、不再累计。
	replay, err = s.RecordEvent(Event{AccountID: "eight-first", EventID: "e8", At: utc(2026, 1, 10, 11, 0), Quantity: 8})
	if err != nil || replay.Accepted || replay.Period != jan(2026) {
		t.Fatalf("replay e8: r=%+v err=%v", replay, err)
	}
	if got := mustMonthlyUsage(t, s, "eight-first"); got != maxInt64 {
		t.Fatalf("eight-first final usage: got %d, want %d", got, maxInt64)
	}
	if got := statusMonthlyUsage(t, s, "eight-first"); got != maxInt64 {
		t.Fatalf("eight-first final status usage: got %d, want %d", got, maxInt64)
	}
}
