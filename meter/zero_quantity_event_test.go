package meter

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件为“数量为 0 的事件是合法事件”提供专门的自动化回归保障，固定零数量
// 事件的接收、去重、账期归属、月用量查询与账户状态口径，以及出账/欠费停用
// 限制对它们同等生效。
//
// 用量入口（RecordEvent）与计量规则（事件按实际发生瞬间归入 UTC 自然月、
// 账户内标识去重、订阅期间校验、已出账/停用拦截、加法溢出检查）全部沿用
// 现有实现：零只是一个合法的非负数量，不是“没有事件”。一条成功接收的零
// 数量事件必须可与“根本没有接收记录”区分——累计值同为 0，但事件标识已被
// 占用、Status 的月度用量列表中应出现这个值为 0 的月份记录。
//
// 场景时间线：订阅自 2026-01-01 起持续有效，时钟在各用例中按需拨动。

var (
	// 与 timezone_usage_test.go 中的 zonePlus8 / zoneMinus5 等同款固定偏移区，
	// 用于证明事件身份按实际瞬间比较：同一瞬间换一种时区表示仍是同一条事件。
	zeroZoneA = time.FixedZone("ZeroA/UTC+8", 8*60*60)
	zeroZoneB = time.FixedZone("ZeroB/UTC-5", -5*60*60)
)

// zeroQtyFixture 构造一个“现在”为 2026-01-15 中午、订阅自月初起有效的
// 账户 a：套餐月费 1000 分、包含 10 单位、超额 100 分/单位、税率 10%。
// 零事件本身不产生费用，但账户内其他事件与后续出账仍按这套条件计量。
func zeroQtyFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return s, clk
}

// usageTotals 从 Status 的月度用量中取出各月累计，便于断言“出现值为 0 的
// 月份记录”与“拒绝后没有这个月记录”。Status 与 MonthlyUsage 口径必须一致。
func usageTotals(t *testing.T, s *Service, id string) map[Month]int64 {
	t.Helper()
	st, err := s.Status(id)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	got := make(map[Month]int64, len(st.MonthlyUsage))
	for _, u := range st.MonthlyUsage {
		got[u.Period] = u.Total
	}
	return got
}

// wantUsageInStatus 断言账户 id 的 Status 月度用量列表中 period 的值为 want
// （want=0 时该月必须“以 0 值出现”，而不是缺席）。
func wantUsageInStatus(t *testing.T, s *Service, id string, period Month, want int64) {
	t.Helper()
	totals := usageTotals(t, s, id)
	if got, ok := totals[period]; !ok {
		t.Fatalf("status monthly usage for %s missing %s (a received zero-qty event must leave a 0-valued record)", id, period)
	} else if got != want {
		t.Fatalf("status usage %s for %s = %d, want %d", period, id, got, want)
	}
}

// TestZeroQuantityEventAcceptedAndVisible 固定正常接收链路：
// 账户已开通、事件时刻在有效订阅期间、账期尚未出账且未停用时，一个新标识、
// 数量为 0 的事件应成功接收（Accepted=true），并返回该实际瞬间所属的 UTC
// 自然月；当月累计仍为 0，但 Status 中必须出现这个值为 0 的月份记录。
// 若该月此前已有正数量事件，零事件绝不能把累计“上报”成 0。
func TestZeroQuantityEventAcceptedAndVisible(t *testing.T) {
	s, _ := zeroQtyFixture(t)

	zeroAt := utc(2026, 1, 15, 9, 0)
	zeroEv := Event{AccountID: "a", EventID: "evt-zero", At: zeroAt, Quantity: 0}

	// 先在同月上报一条正数量事件（账户内其他事件继续计入）。
	posEv := Event{AccountID: "a", EventID: "evt-pos", At: utc(2026, 1, 14, 0, 0), Quantity: 12}
	rp, err := s.RecordEvent(posEv)
	if err != nil || !rp.Accepted || rp.Period != jan(2026) {
		t.Fatalf("positive event: %+v %v", rp, err)
	}

	// 新标识、数量 0：首次接收。
	rz, err := s.RecordEvent(zeroEv)
	if err != nil {
		t.Fatalf("zero quantity event rejected: %v", err)
	}
	if !rz.Accepted {
		t.Fatalf("zero quantity event not marked accepted: %+v", rz)
	}
	if rz.Period != jan(2026) {
		t.Fatalf("zero event period = %s, want %s", rz.Period, jan(2026))
	}

	// 累计不变（仍为 12），不能被零数量清空。
	if u, err := s.MonthlyUsage("a", jan(2026)); err != nil || u.Total != 12 {
		t.Fatalf("jan usage after zero event = %+v %v, want 12", u, err)
	}
	// 但请求确实生效了：Status 中一月记录存在（值为既有 12）；零事件是否被
	// 接收与“没有接收记录”的区别（标识占用）在去重用例中专门验证。
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 12}) {
		t.Fatalf("status usage = %+v, want single jan record of 12", st.MonthlyUsage)
	}

	// 第二个账户：当月只有一条零事件，累计为 0 且必须在 Status 中出现 0 值记录，
	// 与“根本没有接收记录”区分开。
	mustAccount(t, s, "b")
	if err := s.Subscribe("b", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	zeroOnly := Event{AccountID: "b", EventID: "evt-zero-only", At: utc(2026, 1, 15, 9, 0), Quantity: 0}
	r2, err := s.RecordEvent(zeroOnly)
	if err != nil || !r2.Accepted || r2.Period != jan(2026) {
		t.Fatalf("b zero event: %+v %v", r2, err)
	}
	if u, err := s.MonthlyUsage("b", jan(2026)); err != nil || u.Total != 0 {
		t.Fatalf("b jan usage = %+v %v, want 0", u, err)
	}
	totalsB := usageTotals(t, s, "b")
	if got := totalsB[jan(2026)]; got != 0 {
		t.Fatalf("b status jan total = %d, want 0", got)
	}
	if _, present := totalsB[feb(2026)]; present {
		t.Fatalf("b should have no feb usage record, got %+v", totalsB)
	}

	// 收到零事件前的账户在一月的查询同样返回 0，但 Status 不应有该月记录
	// ——这正是“正常接收一条零事件”与“根本没接收”必须可区分的对照。
	mustAccount(t, s, "c")
	if err := s.Subscribe("c", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.MonthlyUsage("c", jan(2026)); u.Total != 0 {
		t.Fatalf("c jan usage = %d, want 0", u.Total)
	}
	stC, err := s.Status("c")
	if err != nil {
		t.Fatal(err)
	}
	if len(stC.MonthlyUsage) != 0 {
		t.Fatalf("c with no events must have no usage records, got %+v", stC.MonthlyUsage)
	}
}

// TestZeroQuantityEventDedupAndConflict 固定零事件标识已被占用后的行为：
// 原样重报成功但不再接收（Accepted=false），继续返回原账期；改成正数或另一个
// 实际瞬间都报 ErrEventConflict，原事件与原月份、改后月份的累计均不变；
// 只换时区表示、未换瞬间的重报视为同一事件，必须成功而不是冲突。
func TestZeroQuantityEventDedupAndConflict(t *testing.T) {
	s, clk := zeroQtyFixture(t)

	at := utc(2026, 1, 15, 9, 0)
	ev := Event{AccountID: "a", EventID: "evt-zero", At: at, Quantity: 0}
	r1, err := s.RecordEvent(ev)
	if err != nil || !r1.Accepted || r1.Period != jan(2026) {
		t.Fatalf("first accept: %+v %v", r1, err)
	}
	wantUsageInStatus(t, s, "a", jan(2026), 0)

	// 原样重报：成功、不再次接收，返回原账期。
	r2, err := s.RecordEvent(ev)
	if err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	if r2.Accepted {
		t.Fatalf("identical replay accepted again: %+v", r2)
	}
	if r2.Period != jan(2026) {
		t.Fatalf("replay period = %s, want %s", r2.Period, jan(2026))
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 0 {
		t.Fatalf("usage changed after replay = %d, want 0", u.Total)
	}

	// 数量改为正数：冲突，一月用量不变。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-zero", At: at, Quantity: 5}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("qty change to positive should conflict: %v", err)
	}

	// 拨到二月后，把发生时刻改成另一个实际瞬间（二月）：仍是冲突；
	// 原月份与改后月份的累计都不变，二月不能因这次失败请求产生记录。
	clk.t = utc(2026, 2, 10, 12, 0)
	febInstant := utc(2026, 2, 2, 8, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-zero", At: febInstant, Quantity: 0}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("instant change should conflict: %v", err)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 0 {
		t.Fatalf("jan usage after conflicts = %d, want 0", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 0 {
		t.Fatalf("feb usage after failed feb re-report = %d, want 0 (no record may be created)", u.Total)
	}
	st, _ := s.Status("a")
	for _, u := range st.MonthlyUsage {
		if u.Period == feb(2026) {
			t.Fatalf("feb record must not appear after conflict: %+v", st.MonthlyUsage)
		}
	}

	// 只改时区表示、不改实际瞬间：成功（幂等重报），不能变成冲突。
	sameInstantOtherZone := time.Date(2026, 1, 15, 17, 0, 0, 0, zeroZoneA) // 09:00 UTC
	if !sameInstantOtherZone.UTC().Equal(at) {
		t.Fatalf("test setup: %v != %v", sameInstantOtherZone.UTC(), at)
	}
	r3, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-zero", At: sameInstantOtherZone, Quantity: 0})
	if err != nil {
		t.Fatalf("same-instant timezone-only replay failed: %v", err)
	}
	if r3.Accepted {
		t.Fatalf("timezone-only replay should be idempotent: %+v", r3)
	}
	if r3.Period != jan(2026) {
		t.Fatalf("timezone-only replay period = %s, want %s", r3.Period, jan(2026))
	}

	// 另一种偏移方向再验一次：04:00 钟面（UTC-5）= 09:00 UTC。
	sameInstantMinus5 := time.Date(2026, 1, 15, 4, 0, 0, 0, zeroZoneB)
	if !sameInstantMinus5.UTC().Equal(at) {
		t.Fatalf("test setup: %v != %v", sameInstantMinus5.UTC(), at)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-zero", At: sameInstantMinus5, Quantity: 0}); err != nil {
		t.Fatalf("same-instant minus5 replay failed: %v", err)
	}

	// 账户内其他事件继续计入：同月一条正数事件与零事件并存，累计只加正数。
	other := Event{AccountID: "a", EventID: "evt-other", At: utc(2026, 1, 16, 0, 0), Quantity: 7}
	if _, err := s.RecordEvent(other); err != nil {
		t.Fatalf("other event: %v", err)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 7 {
		t.Fatalf("jan usage = %d, want 7 (zero event must not alter positive total)", u.Total)
	}
	// 原样重报零事件仍然成功，账期仍为一月。
	if r, err := s.RecordEvent(ev); err != nil || r.Accepted || r.Period != jan(2026) {
		t.Fatalf("zero replay after positive event: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 7 {
		t.Fatalf("jan usage changed by zero replay = %d, want 7", u.Total)
	}
}

// TestZeroQuantityEventBilledMonthAndReplay 固定零数量不能绕过出账限制：
// 已出账月份的新零数量事件返回 ErrMonthBilled；此前已接收的零事件原样重报
// 仍成功；账单用量与金额保持出账时固定的值。
func TestZeroQuantityEventBilledMonthAndReplay(t *testing.T) {
	s, clk := zeroQtyFixture(t)

	// 一月：一条正数事件（5 单位）+ 一条零事件。
	pos := Event{AccountID: "a", EventID: "evt-pos", At: utc(2026, 1, 10, 0, 0), Quantity: 5}
	zero := Event{AccountID: "a", EventID: "evt-zero", At: utc(2026, 1, 11, 0, 0), Quantity: 0}
	for _, e := range []Event{pos, zero} {
		if r, err := s.RecordEvent(e); err != nil || !r.Accepted || r.Period != jan(2026) {
			t.Fatalf("seed %q: %+v %v", e.EventID, r, err)
		}
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 5 {
		t.Fatalf("jan usage = %d, want 5", u.Total)
	}

	// 月未结束不能出账，拨动时钟后出账。
	clk.t = utc(2026, 2, 1, 0, 0)
	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatalf("create bill: %v", err)
	}
	// 用量明细固定为 5；零事件不贡献用量。
	if b.TotalUsage != 5 || b.OverageUnits != 0 {
		t.Fatalf("bill usage: total=%d overage=%d, want 5/0", b.TotalUsage, b.OverageUnits)
	}
	// 月费 1000 + 超额 0，税 10% = 100，应付 1100；该值出账后固定。
	if b.MonthlyFee != 1000 || b.OverageFee != 0 || b.Tax != 100 || b.TotalDue != 1100 || b.Balance != 1100 {
		t.Fatalf("bill amounts: %+v", b)
	}
	billCopy := b

	// 已出账月份的“新标识、零数量”事件：必须按月份已出账拒绝。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-zero-new", At: utc(2026, 1, 12, 0, 0), Quantity: 0}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new zero event on billed month: %v", err)
	}
	// 此前已接收的零事件原样重报仍成功，不再次接收，仍返回一月账期。
	if r, err := s.RecordEvent(zero); err != nil || r.Accepted || r.Period != jan(2026) {
		t.Fatalf("accepted zero event replay after bill: %+v %v", r, err)
	}
	// 正数事件的重报同规则（既有公开行为不回归）。
	if r, err := s.RecordEvent(pos); err != nil || r.Accepted || r.Period != jan(2026) {
		t.Fatalf("positive event replay after bill: %+v %v", r, err)
	}

	// 账单用量与金额保持固定，月用量也保持 5。
	got, err := s.GetBill("a", jan(2026))
	if err != nil {
		t.Fatal(err)
	}
	if got != billCopy {
		t.Fatalf("bill changed after replays: %+v vs %+v", got, billCopy)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 5 {
		t.Fatalf("jan usage = %d, want 5", u.Total)
	}
}

// TestZeroQuantityEventSuspensionRejectionAndRecovery 固定零数量不能绕过停用：
// 账户因到期欠费停用时，对尚未出账月份首次上报零数量事件返回 ErrSuspended，
// 不增加月份记录、不占用标识；结清欠费后（订阅仍有效、该月仍未出账）同一请求
// 作为首次事件被接收（Accepted=true），再报一次才是重复。拒绝不留下事件记录，
// 而正数量事件、账单关闭与欠费停用的既有公开行为保持不变。
func TestZeroQuantityEventSuspensionRejectionAndRecovery(t *testing.T) {
	s, clk := zeroQtyFixture(t)

	// 一月出一张欠费账单：月费 1000（无超额），截止 2026-02-08。
	clk.t = utc(2026, 2, 1, 0, 0)
	janBill, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatalf("jan bill: %v", err)
	}
	if janBill.TotalDue != 1100 {
		t.Fatalf("jan bill due = %d, want 1100", janBill.TotalDue)
	}

	// 到达截止时刻仍有余额：停用。订阅仍然有效，二月尚未出账。
	clk.t = utc(2026, 2, 8, 0, 0)
	if st, _ := s.Status("a"); !st.Suspended || !st.Subscribed {
		t.Fatalf("want suspended with active subscription, got %+v", st)
	}

	// 首次上报二月的零数量事件：按停用拒绝。
	zeroFeb := Event{AccountID: "a", EventID: "evt-zero-feb", At: utc(2026, 2, 2, 0, 0), Quantity: 0}
	if _, err := s.RecordEvent(zeroFeb); !errors.Is(err, ErrSuspended) {
		t.Fatalf("zero event while suspended: %v", err)
	}
	// 不增加月份记录（Status 中不能出现二月条目），月用量查询仍为 0，
	// 且这与“接收了一条零事件”有本质区别。注意：一月已出账，Status 的月度
	// 用量列表会包含一月（值为 0），这是账单与用量并集展示的既有行为；
	// 本断言只针对被拒的二月——它既不在用量表中也没有账单，必须完全缺席。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 0 {
		t.Fatalf("feb usage after suspension rejection = %d, want 0", u.Total)
	}
	st, _ := s.Status("a")
	for _, u := range st.MonthlyUsage {
		if u.Period == feb(2026) {
			t.Fatalf("feb usage record must not exist after rejection, got %+v", st.MonthlyUsage)
		}
	}
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 0}) {
		t.Fatalf("only the billed jan entry may show, got %+v", st.MonthlyUsage)
	}

	// 不占用标识：停用期间把数量改成正数再报仍是停用错误，而不是冲突——
	// 证明标识从未落库。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-zero-feb", At: utc(2026, 2, 2, 0, 0), Quantity: 9}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("different content under same unconsumed id while suspended must still be ErrSuspended, got %v", err)
	}

	// 结清一月欠费（1100）后恢复；二月仍未出账、订阅仍有效。
	if r, err := s.RecordPayment("a", "pay-jan", jan(2026), 1100); err != nil ||
		!r.Registered || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("pay jan in full: %+v %v", r, err)
	}
	if st, _ := s.Status("a"); st.Suspended {
		t.Fatalf("account should recover after settling overdue bill: %+v", st)
	}

	// 原请求作为“首次事件”被接收，而不是按重报处理。
	rFirst, err := s.RecordEvent(zeroFeb)
	if err != nil {
		t.Fatalf("zero event after recovery should be accepted: %v", err)
	}
	if !rFirst.Accepted || rFirst.Period != feb(2026) {
		t.Fatalf("zero event after recovery: %+v, want accepted/%s", rFirst, feb(2026))
	}
	// 现在 Status 中必须出现二月的 0 值记录——拒绝后没有，接收后有。
	wantUsageInStatus(t, s, "a", feb(2026), 0)
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 0 {
		t.Fatalf("feb usage = %d, want 0", u.Total)
	}
	// 再报一次才视为重复。
	rReplay, err := s.RecordEvent(zeroFeb)
	if err != nil || rReplay.Accepted || rReplay.Period != feb(2026) {
		t.Fatalf("second report must be dedup: %+v %v", rReplay, err)
	}

	// 账单关闭与欠费停用的既有行为仍在：一月账单金额固定、已结清。
	b, _ := s.GetBill("a", jan(2026))
	if b.TotalDue != 1100 || b.Paid != 1100 || b.Balance != 0 || !b.Settled {
		t.Fatalf("jan bill state after recovery: %+v", b)
	}
	if b.TotalUsage != 0 {
		t.Fatalf("jan bill total usage = %d, want 0", b.TotalUsage)
	}
}

// TestZeroQuantityEventDoesNotConsumeIDOnOtherRejections 补充固定：零数量事件
// 在“订阅期间之外 / 未来时刻 / 已出账月份”等既有接收限制下被拒时同样不占用
// 标识、不产生 0 值月份记录；条件满足后以同标识首次上报成功。
func TestZeroQuantityEventDoesNotConsumeIDOnOtherRejections(t *testing.T) {
	s, clk := zeroQtyFixture(t)

	// 时刻早于开通（2025-12-31）：拒绝，不占标识。
	before := Event{AccountID: "a", EventID: "evt-z", At: utc(2025, 12, 31, 23, 0), Quantity: 0}
	if _, err := s.RecordEvent(before); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("zero before subscription: %v", err)
	}
	// 晚于当前时刻：拒绝。
	future := Event{AccountID: "a", EventID: "evt-z", At: clk.get().Add(time.Second), Quantity: 0}
	if _, err := s.RecordEvent(future); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("zero future event: %v", err)
	}

	// 合法的零事件首次接收成功，证明标识未被两次失败消耗。
	good := Event{AccountID: "a", EventID: "evt-z", At: utc(2026, 1, 15, 9, 0), Quantity: 0}
	if r, err := s.RecordEvent(good); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("zero event after earlier rejections: %+v %v", r, err)
	}
	wantUsageInStatus(t, s, "a", jan(2026), 0)

	// 已出账月份的新零事件被拒后，该月仍无 0 值记录（一月已因 good 有 0 值，
	// 这里验证被拒的新标识不在一月另生记录——记录数仍为 1）。
	clk.t = utc(2026, 2, 1, 0, 0)
	if _, err := s.CreateBill("a", jan(2026)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "evt-z-late", At: utc(2026, 1, 20, 0, 0), Quantity: 0}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("late zero on billed jan: %v", err)
	}
	st, _ := s.Status("a")
	if len(st.MonthlyUsage) != 1 || st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 0}) {
		t.Fatalf("usage records after billed rejection: %+v", st.MonthlyUsage)
	}
}

// TestZeroQuantityEventConcurrentFirstReport 并发固定：同一新标识、相同内容的
// 零数量事件并发首次上报时，恰好一次 Accepted=true，其余为幂等重报；月累计
// 保持 0，且 Status 只出现一条一月记录。零数量走与正数量完全相同的串行裁决
// 临界区，不能因为“加 0 无害”而放过重复接收。
func TestZeroQuantityEventConcurrentFirstReport(t *testing.T) {
	s, _ := zeroQtyFixture(t)
	ev := Event{AccountID: "a", EventID: "evt-zero-race", At: utc(2026, 1, 15, 9, 0), Quantity: 0}

	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	var accepted, replayed int
	var errs []error
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			r, err := s.RecordEvent(ev)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if r.Accepted {
				accepted++
			} else {
				if r.Period != jan(2026) {
					errs = append(errs, errors.New("replay returned wrong period"))
					return
				}
				replayed++
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("concurrent zero reports: %v", errs[0])
	}
	if accepted != 1 || replayed != n-1 {
		t.Fatalf("accepted=%d replayed=%d, want 1 and %d", accepted, replayed, n-1)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 0 {
		t.Fatalf("jan usage = %d, want 0", u.Total)
	}
	wantUsageInStatus(t, s, "a", jan(2026), 0)
}
