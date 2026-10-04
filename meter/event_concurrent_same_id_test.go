package meter

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 本文件回归“同一账户、两份不同内容使用此前从未接收过的同一个事件标识
// 同时首次上报”的行为。
//
// 规则是：事件标识在账户内去重，已接收事件的发生时刻与数量不能被同标识的
// 后续请求替换——这条规则在同时提交时同样成立。实现把“去重判定 →
// 时刻/账期/停用/溢出校验 → 事件与累计落库”放在同一把互斥锁的一个临界区内
// （见 RecordEvent），两份同标识请求必然串行裁决：恰好一份作为首次上报被
// 接收并按其实际发生时刻累计，另一份返回 ErrEventConflict，不被当成原样
// 重报，也不覆盖或合并到先成功的事件。允许任意一份先成功。
//
// 这里锁住两种确实会影响记账的内容差异：
//   - 发生时刻相同（同一实际瞬间）而数量不同；
//   - 数量相同而发生时刻分别属于相邻两个 UTC 自然月（两个月都已结束、
//     尚未出账，且都在订阅期间）——用量只应增加到成功请求所属月份。
//
// 争用结束后，标识始终对应最初成功的内容而不是最后提交的内容：按成功请求
// 原内容重报成功返回但未再次接收并保留原账期；按失败请求原内容重报仍报
// 冲突；两种操作都不再改变任何月份的累计用量。
//
// 同时覆盖事件身份的实际瞬间规则：数量一致、仅时区表示不同的请求是同一
// 事件，同时提交时只有一次首次接收，其余按重复上报（Accepted=false）成功
// 处理，不能报冲突。

// sameIDReq 是一笔参与同标识并发争用的请求：时刻与数量由调用方指定，
// 标识在发起时统一给出。
type sameIDReq struct {
	at  time.Time
	qty int64
}

// sameIDOutcome 记录一笔争用请求的返回。
type sameIDOutcome struct {
	at  time.Time
	qty int64
	r   EventResult
	err error
}

// newContentionService 构造一个“现在”为 2026-04-10 的服务与一份零月费、
// 零包含额度（超额也按 0 计价）、零税率的套餐：本文件只关心累计用量，
// 金额与额度不参与断言。
func newContentionService(t *testing.T) *Service {
	t.Helper()
	s, _ := newTestService(utc(2026, 4, 10, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 0, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	return s
}

// mustSubscribedAccount 在 s 上创建账户并开通自 2026-01-01 起生效的订阅，
// 覆盖本文件所有事件时刻。
func mustSubscribedAccount(t *testing.T, s *Service, acct string) {
	t.Helper()
	mustAccount(t, s, acct)
	if err := s.Subscribe(acct, "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe %q: %v", acct, err)
	}
}

// mustSeedEvent 登记一笔此前从未接收的种子事件，用于证明争用不会丢失账户
// 此前已接收的其他事件。
func mustSeedEvent(t *testing.T, s *Service, acct, id string, at time.Time, qty int64) {
	t.Helper()
	r, err := s.RecordEvent(Event{AccountID: acct, EventID: id, At: at, Quantity: qty})
	if err != nil || !r.Accepted || r.Period != MonthOf(at) {
		t.Fatalf("seed %q: r=%+v err=%v", id, r, err)
	}
}

// contendSameID 用启动屏障让两笔请求真正并发地（而非顺序提交）以同一事件
// 标识首次上报，返回与传入顺序一致的两笔结果。
func contendSameID(s *Service, acct, eventID string, first, second sameIDReq) []sameIDOutcome {
	reqs := []sameIDReq{first, second}
	res := make([]sameIDOutcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i, q := range reqs {
		go func(i int, q sameIDReq) {
			defer wg.Done()
			<-start
			r, err := s.RecordEvent(Event{
				AccountID: acct,
				EventID:   eventID,
				At:        q.at,
				Quantity:  q.qty,
			})
			res[i] = sameIDOutcome{at: q.at, qty: q.qty, r: r, err: err}
		}(i, q)
	}
	close(start)
	wg.Wait()
	return res
}

// assertMonthTotal 断言 MonthlyUsage 与 Status 中指定账期的累计用量一致地
// 等于 want。
func assertMonthTotal(t *testing.T, s *Service, acct string, m Month, want int64) {
	t.Helper()
	u, err := s.MonthlyUsage(acct, m)
	if err != nil {
		t.Fatalf("monthly usage %q %s: %v", acct, m, err)
	}
	if u.Total != want {
		t.Fatalf("%q %s monthly usage = %d, want %d", acct, m, u.Total, want)
	}
	if got := statusMonthUsage(t, s, acct, m); got.Total != want {
		t.Fatalf("%q %s status monthly usage = %d, want %d", acct, m, got.Total, want)
	}
}

// resolveContention 断言并发结果恰为“一笔首次接收 + 一笔 ErrEventConflict”，
// 返回赢家（被首次接收）与输家（冲突）。赢家的账期必须对应它自己的实际
// 发生时刻。
func resolveContention(t *testing.T, acct, eventID string, res []sameIDOutcome) (winner, loser sameIDOutcome) {
	t.Helper()
	var w, l *sameIDOutcome
	for i := range res {
		oc := &res[i]
		switch {
		case oc.err == nil && oc.r.Accepted:
			w = oc
		case errors.Is(oc.err, ErrEventConflict):
			l = oc
		default:
			t.Fatalf("%q contention event at %v qty %d unexpected: r=%+v err=%v",
				acct, oc.at.UTC(), oc.qty, oc.r, oc.err)
		}
	}
	if w == nil || l == nil {
		t.Fatalf("%q contention for %q want exactly one accepted and one conflict, got %+v",
			acct, eventID, res)
	}
	if w.r.Period != MonthOf(w.at) {
		t.Fatalf("%q winner period %s does not match its own event time %v",
			acct, w.r.Period, w.at.UTC())
	}
	return *w, *l
}

// TestConcurrentSameEventIDSameTimeDifferentQuantity 覆盖第一种内容差异：
// 同一实际发生时刻、不同数量的两份请求使用同一新标识同时首次上报。
// 恰好一份被首次接收并累计其数量，另一份返回冲突；账期相同，但数量只能是
// 赢家的数量，不能合并、不能被输家替换。
func TestConcurrentSameEventIDSameTimeDifferentQuantity(t *testing.T) {
	s := newContentionService(t)

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("same-time-%d", iter)
		mustSubscribedAccount(t, s, acct)
		// 账户此前已接收的其他事件继续计入，不能因争用丢失。
		mustSeedEvent(t, s, acct, "prior-jan", utc(2026, 1, 20, 9, 0), 11)
		mustSeedEvent(t, s, acct, "prior-feb", utc(2026, 2, 20, 9, 0), 13)

		at := utc(2026, 2, 15, 8, 0)
		res := contendSameID(s, acct, "dup-id",
			sameIDReq{at: at, qty: 7},
			sameIDReq{at: at, qty: 4},
		)
		winner, loser := resolveContention(t, acct, "dup-id", res)
		if winner.r.Period != feb(2026) {
			t.Fatalf("iter %d winner period = %s, want 2026-02", iter, winner.r.Period)
		}

		// 只有赢家的数量计入 2 月：种子 13 + 赢家数量；不与输家合并。
		assertMonthTotal(t, s, acct, feb(2026), 13+winner.qty)
		assertMonthTotal(t, s, acct, jan(2026), 11)

		// 按赢家原内容重报：成功、未再次接收、保留原账期，且不改变用量。
		replayW, err := s.RecordEvent(Event{
			AccountID: acct, EventID: "dup-id",
			At: winner.at, Quantity: winner.qty,
		})
		if err != nil || replayW.Accepted || replayW.Period != feb(2026) {
			t.Fatalf("iter %d replay winner: r=%+v err=%v, want not accepted in Feb",
				iter, replayW, err)
		}
		// 按输家原内容重报：仍是事件冲突，而不是原样重报成功。
		if _, err := s.RecordEvent(Event{
			AccountID: acct, EventID: "dup-id",
			At: loser.at, Quantity: loser.qty,
		}); !errors.Is(err, ErrEventConflict) {
			t.Fatalf("iter %d replay loser qty %d should conflict, got %v",
				iter, loser.qty, err)
		}
		assertMonthTotal(t, s, acct, feb(2026), 13+winner.qty)
		assertMonthTotal(t, s, acct, jan(2026), 11)
	}
}

// TestConcurrentSameEventIDSameQuantityAdjacentMonths 覆盖第二种内容差异：
// 数量相同、发生时刻分别属于相邻两个 UTC 自然月（2026-02 与 2026-03，
// 两个月都已结束、尚未出账，且都在订阅期间）。只有赢家所属月份增加该数量，
// 输家所指月份保持原用量，既有事件不丢失、两份数量不合并。
func TestConcurrentSameEventIDSameQuantityAdjacentMonths(t *testing.T) {
	s := newContentionService(t)

	// 两个月都在种子中放一笔既有用量，随后精确断言每个月只增加赢家的一份。
	febBase, marBase := int64(17), int64(23)
	janBase := int64(5)
	const qty = int64(9)

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("adj-months-%d", iter)
		mustSubscribedAccount(t, s, acct)
		mustSeedEvent(t, s, acct, "prior-jan", utc(2026, 1, 20, 9, 0), janBase)
		mustSeedEvent(t, s, acct, "prior-feb", utc(2026, 2, 20, 9, 0), febBase)
		mustSeedEvent(t, s, acct, "prior-mar", utc(2026, 3, 20, 9, 0), marBase)

		febAt := utc(2026, 2, 15, 8, 0)
		marAt := utc(2026, 3, 15, 8, 0)
		res := contendSameID(s, acct, "dup-id",
			sameIDReq{at: febAt, qty: qty},
			sameIDReq{at: marAt, qty: qty},
		)
		winner, loser := resolveContention(t, acct, "dup-id", res)

		winMonth, loseMonth := MonthOf(winner.at), MonthOf(loser.at)
		if winMonth != feb(2026) && winMonth != mar(2026) {
			t.Fatalf("iter %d winner in unexpected month %s", iter, winMonth)
		}
		// 时刻分属相邻两月，赢家账期必须对应自己的实际发生时刻。
		if winner.r.Period != winMonth || winMonth == loseMonth {
			t.Fatalf("iter %d winner period %s / loser month %s inconsistent",
				iter, winner.r.Period, loseMonth)
		}

		// 预期各月用量：只有赢家月份增加 qty，输家月份与其他月份保持原值。
		wantFeb, wantMar := febBase, marBase
		if winMonth == feb(2026) {
			wantFeb += qty
		} else {
			wantMar += qty
		}
		assertMonthTotal(t, s, acct, jan(2026), janBase)
		assertMonthTotal(t, s, acct, feb(2026), wantFeb)
		assertMonthTotal(t, s, acct, mar(2026), wantMar)

		// 赢家原内容重报：未再次接收、保留原账期；输家原内容重报：仍冲突。
		replayW, err := s.RecordEvent(Event{
			AccountID: acct, EventID: "dup-id",
			At: winner.at, Quantity: winner.qty,
		})
		if err != nil || replayW.Accepted || replayW.Period != winMonth {
			t.Fatalf("iter %d replay winner in %s: r=%+v err=%v",
				iter, winMonth, replayW, err)
		}
		if _, err := s.RecordEvent(Event{
			AccountID: acct, EventID: "dup-id",
			At: loser.at, Quantity: loser.qty,
		}); !errors.Is(err, ErrEventConflict) {
			t.Fatalf("iter %d replay loser (%s) should conflict, got %v",
				iter, loseMonth, err)
		}
		assertMonthTotal(t, s, acct, jan(2026), janBase)
		assertMonthTotal(t, s, acct, feb(2026), wantFeb)
		assertMonthTotal(t, s, acct, mar(2026), wantMar)
	}
}

// TestConcurrentSameEventIDBothOrdersDeterministic 以确定顺序锁住跨月争用的
// 两种可能获胜结果（并发测试不保证每种先后顺序都被调度到）：无论哪个月份的
// 请求先到，最终都只在先到请求所属月份累计，后到请求冲突且不落账；之后两种
// 原内容重报分别表现为“未接收成功”与“仍冲突”，累计用量保持不变。
func TestConcurrentSameEventIDBothOrdersDeterministic(t *testing.T) {
	febAt := utc(2026, 2, 15, 8, 0)
	marAt := utc(2026, 3, 15, 8, 0)
	const qty = int64(6)
	// 两个月都垫上既有用量基线，使“输家所指月份保持原用量”可经
	// MonthlyUsage 与 Status 两处精确核对。
	febBase, marBase := int64(3), int64(4)

	// 顺序一：2 月请求先成功。
	t.Run("february-first", func(t *testing.T) {
		s := newContentionService(t)
		acct := "det-feb-first"
		mustSubscribedAccount(t, s, acct)
		mustSeedEvent(t, s, acct, "prior-feb", utc(2026, 2, 20, 9, 0), febBase)
		mustSeedEvent(t, s, acct, "prior-mar", utc(2026, 3, 20, 9, 0), marBase)
		r1, err := s.RecordEvent(Event{AccountID: acct, EventID: "dup-id", At: febAt, Quantity: qty})
		if err != nil || !r1.Accepted || r1.Period != feb(2026) {
			t.Fatalf("feb first: r=%+v err=%v", r1, err)
		}
		if _, err := s.RecordEvent(Event{AccountID: acct, EventID: "dup-id", At: marAt, Quantity: qty}); !errors.Is(err, ErrEventConflict) {
			t.Fatalf("march after february should conflict: %v", err)
		}
		assertMonthTotal(t, s, acct, feb(2026), febBase+qty)
		assertMonthTotal(t, s, acct, mar(2026), marBase)
		if r, err := s.RecordEvent(Event{AccountID: acct, EventID: "dup-id", At: febAt, Quantity: qty}); err != nil || r.Accepted || r.Period != feb(2026) {
			t.Fatalf("replay feb winner: r=%+v err=%v", r, err)
		}
		if _, err := s.RecordEvent(Event{AccountID: acct, EventID: "dup-id", At: marAt, Quantity: qty}); !errors.Is(err, ErrEventConflict) {
			t.Fatalf("replay march loser should conflict: %v", err)
		}
		assertMonthTotal(t, s, acct, feb(2026), febBase+qty)
		assertMonthTotal(t, s, acct, mar(2026), marBase)
	})

	// 顺序二：3 月请求先成功。
	t.Run("march-first", func(t *testing.T) {
		s := newContentionService(t)
		acct := "det-mar-first"
		mustSubscribedAccount(t, s, acct)
		mustSeedEvent(t, s, acct, "prior-feb", utc(2026, 2, 20, 9, 0), febBase)
		mustSeedEvent(t, s, acct, "prior-mar", utc(2026, 3, 20, 9, 0), marBase)
		r1, err := s.RecordEvent(Event{AccountID: acct, EventID: "dup-id", At: marAt, Quantity: qty})
		if err != nil || !r1.Accepted || r1.Period != mar(2026) {
			t.Fatalf("mar first: r=%+v err=%v", r1, err)
		}
		if _, err := s.RecordEvent(Event{AccountID: acct, EventID: "dup-id", At: febAt, Quantity: qty}); !errors.Is(err, ErrEventConflict) {
			t.Fatalf("february after march should conflict: %v", err)
		}
		assertMonthTotal(t, s, acct, feb(2026), febBase)
		assertMonthTotal(t, s, acct, mar(2026), marBase+qty)
		if r, err := s.RecordEvent(Event{AccountID: acct, EventID: "dup-id", At: marAt, Quantity: qty}); err != nil || r.Accepted || r.Period != mar(2026) {
			t.Fatalf("replay mar winner: r=%+v err=%v", r, err)
		}
		if _, err := s.RecordEvent(Event{AccountID: acct, EventID: "dup-id", At: febAt, Quantity: qty}); !errors.Is(err, ErrEventConflict) {
			t.Fatalf("replay february loser should conflict: %v", err)
		}
		assertMonthTotal(t, s, acct, feb(2026), febBase)
		assertMonthTotal(t, s, acct, mar(2026), marBase+qty)
	})
}

// TestConcurrentSameEventIDTimezoneOnlyDifferenceIsDuplicate 锁定实际瞬间
// 规则下的并发情形：数量一致、发生时刻是同一实际瞬间、仅时区表示不同的两份
// 请求同时提交，只有一次首次接收，另一份按重复上报成功（Accepted=false、
// 原账期），不能报事件冲突，也不能累计第二遍。
func TestConcurrentSameEventIDTimezoneOnlyDifferenceIsDuplicate(t *testing.T) {
	s := newContentionService(t)

	const rounds = 100
	for iter := 0; iter < rounds; iter++ {
		acct := fmt.Sprintf("tz-%d", iter)
		mustSubscribedAccount(t, s, acct)

		// 同一实际瞬间 2026-02-28 16:30Z 的两种时区表示：
		// 本地 03-01 00:30 +08:00 与本地 02-28 11:30 -05:00，都归入 2 月。
		plus8 := time.Date(2026, time.March, 1, 0, 30, 0, 0, zonePlus8)
		minus5 := time.Date(2026, time.February, 28, 11, 30, 0, 0, zoneMinus5)
		if !plus8.Equal(minus5) {
			t.Fatal("test setup: representations are not the same instant")
		}
		res := contendSameID(s, acct, "tz-id",
			sameIDReq{at: plus8, qty: 12},
			sameIDReq{at: minus5, qty: 12},
		)

		nAccepted, nDuplicate := 0, 0
		for _, oc := range res {
			if oc.err != nil {
				t.Fatalf("iter %d same-instant contention returned error: %v", iter, oc.err)
			}
			if oc.r.Accepted {
				nAccepted++
			} else {
				nDuplicate++
			}
			if oc.r.Period != feb(2026) {
				t.Fatalf("iter %d period = %s, want 2026-02", iter, oc.r.Period)
			}
		}
		if nAccepted != 1 || nDuplicate != 1 {
			t.Fatalf("iter %d want exactly one accepted + one duplicate, got accepted=%d duplicate=%d",
				iter, nAccepted, nDuplicate)
		}

		// 数量只累计一次；之后两种表示重报都按重复处理，用量不变。
		assertMonthTotal(t, s, acct, feb(2026), 12)
		for _, at := range []time.Time{plus8, minus5} {
			r, err := s.RecordEvent(Event{AccountID: acct, EventID: "tz-id", At: at, Quantity: 12})
			if err != nil || r.Accepted || r.Period != feb(2026) {
				t.Fatalf("iter %d replay %v: r=%+v err=%v", iter, at.UTC(), r, err)
			}
		}
		assertMonthTotal(t, s, acct, feb(2026), 12)
	}
}
