package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“数量为 0 的用量事件是合法事件”提供专门的自动化回归保障，固定这类
// 事件的接收与去重行为。所有用例只经过现有公开入口（RecordEvent /
// MonthlyUsage / Status / CreateBill / GetBill / RecordPayment）与既有计量
// 规则，不引入任何零数量专属通道：数量 0 走的是与正数量完全相同的校验、
// 归期、去重、出账与停用路径，唯一区别是它不改变账期累计值。
//
// 关键约束（题述）：
//   - 零数量事件被首次接收时返回 Accepted=true 与发生时刻所属 UTC 自然月；
//     账户状态中必须出现这个值为 0 的月份记录，以区分“接收了一条零数量事件”
//     与“根本没有接收记录”——累计值同为 0 并不表示请求没有生效。
//   - 零数量上报不能清空该月已有累计：已有正数量时累计保持原值。
//   - 接收后事件标识即被占用：原样重报成功但不再接收（Accepted=false），
//     返回原账期；改数量为正、改发生时刻为另一个实际瞬间都返回冲突，
//     原事件及原月份、改后时刻所属月份累计均不变；只换时区表示、不换瞬间
//     的重报仍成功，不能变成冲突。
//   - 零数量不能绕过接收限制：已出账月份的新零数量事件返回月份已出账错误，
//     停用期间的新零数量事件返回停用错误，且都不留月份记录、不占用标识。

// findStatusUsage 在账户状态的各月用量中查找指定账期，返回该条目与是否存在。
// 零数量事件的回归必须借助“条目是否存在”而非累计值判定：值为 0 的月份记录
// 与完全没有记录在 MonthlyUsage 单点查询里都是 0，只有 Status 的月份列表能
// 把二者区分开。
func findStatusUsage(st AccountStatus, m Month) (Usage, bool) {
	for _, u := range st.MonthlyUsage {
		if u.Period == m {
			return u, true
		}
	}
	return Usage{}, false
}

// TestZeroQuantityAcceptedAsFirstEventCreatesZeroMonthRecord 锁定最基本事实：
// 在账户已开通、事件时刻属于有效订阅、账期未出账且未停用时，一个全新标识、
// 数量为 0 的事件应被成功接收（Accepted=true），返回首次接收与该时刻所属的
// UTC 自然月；该月累计仍为 0，但状态中必须出现这条值为 0 的月份记录。
func TestZeroQuantityAcceptedAsFirstEventCreatesZeroMonthRecord(t *testing.T) {
	s, _ := newTestService(utc(2026, 2, 20, 12, 0))
	mustPlan(t, s, Plan{ID: "p", TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 1, 1, 0, 0))

	zero := Event{AccountID: "a", EventID: "z-first", At: utc(2026, 2, 15, 9, 30), Quantity: 0}
	r, err := s.RecordEvent(zero)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("first zero event = %+v %v, want accepted into 2026-02", r, err)
	}

	// 累计用量为 0。
	if u, err := s.MonthlyUsage("a", feb(2026)); err != nil || u.Total != 0 {
		t.Fatalf("feb usage after zero event = %+v %v, want total 0", u, err)
	}

	st, err := s.Status("a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// 值为 0 的二月记录必须出现：这是“接收了一条零数量事件”的可观察证据。
	got, ok := findStatusUsage(st, feb(2026))
	if !ok || got != (Usage{Period: feb(2026), Total: 0}) {
		t.Fatalf("status usage = %+v, want an explicit 2026-02=0 record", st.MonthlyUsage)
	}
	// 从未发生任何事件、也未出账的一月必须缺省：用它对照证明二月这条 0 记录
	// 确由本次零数量上报产生，而不是所有月份都天然带一个 0。
	if _, ok := findStatusUsage(st, jan(2026)); ok {
		t.Fatalf("january must have no usage record at all: %+v", st.MonthlyUsage)
	}

	// 标识已被占用：原样重报不再首次接收，但成功返回原账期。
	replay, err := s.RecordEvent(zero)
	if err != nil || replay.Accepted || replay.Period != feb(2026) {
		t.Fatalf("identical zero replay = %+v %v, want dedup success into 2026-02", replay, err)
	}
}

// TestZeroQuantityDoesNotClearExistingUsage 锁定零数量不能清空当月已有累计：
// 该月已有正数量事件时，再接收一条新标识的零数量事件后累计保持原值；零数量
// 落在一个新月时只新建值为 0 的月份记录，不动其他月份。随后正数量事件仍可在
// 该零数量月份上正常累加。
func TestZeroQuantityDoesNotClearExistingUsage(t *testing.T) {
	s, _ := newTestService(utc(2026, 3, 20, 12, 0))
	mustPlan(t, s, Plan{ID: "p", TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 1, 1, 0, 0))

	if r, err := s.RecordEvent(Event{AccountID: "a", EventID: "jan-pos", At: utc(2026, 1, 10, 0, 0), Quantity: 5}); err != nil ||
		!r.Accepted || r.Period != jan(2026) {
		t.Fatalf("seed january usage: %+v %v", r, err)
	}

	// 同一已存在累计的月份接收零数量事件：成功，但一月累计保持 5，不被清零。
	zJan := Event{AccountID: "a", EventID: "z-jan", At: utc(2026, 1, 20, 0, 0), Quantity: 0}
	if r, err := s.RecordEvent(zJan); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("zero event in non-empty january = %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 5 {
		t.Fatalf("january usage after zero event = %d, want 5 (must not be cleared)", u.Total)
	}

	// 零数量落在尚无记录的二月：新建二月=0 记录，一月仍为 5。
	zFeb := Event{AccountID: "a", EventID: "z-feb", At: utc(2026, 2, 10, 0, 0), Quantity: 0}
	if r, err := s.RecordEvent(zFeb); err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("zero event in fresh february = %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 0 {
		t.Fatalf("february usage = %d, want 0", u.Total)
	}
	st, _ := s.Status("a")
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 5}) ||
		st.MonthlyUsage[1] != (Usage{Period: feb(2026), Total: 0}) {
		t.Fatalf("status usage = %+v, want [jan 5, feb 0]", st.MonthlyUsage)
	}

	// 已有零数量记录的二月随后仍可正常累加正数量，零数量不会把月份“锁”在 0。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "feb-pos", At: utc(2026, 2, 15, 0, 0), Quantity: 3}); err != nil {
		t.Fatalf("positive event after zero event: %v", err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 3 {
		t.Fatalf("february usage after later positive event = %d, want 3", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 5 {
		t.Fatalf("january usage changed = %d, want 5", u.Total)
	}

	// 两条零数量事件的标识都已占用：原样重报均不再首次接收、返回原账期。
	if r, err := s.RecordEvent(zJan); err != nil || r.Accepted || r.Period != jan(2026) {
		t.Fatalf("z-jan replay = %+v %v", r, err)
	}
	if r, err := s.RecordEvent(zFeb); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("z-feb replay = %+v %v", r, err)
	}
}

// TestZeroQuantityDedupConflictAndTimezoneIdentity 锁定零数量事件的身份规则：
// 原样重报去重、改数量/改实际瞬间冲突、只换时区表示仍是同一事件。
// 账户内另有的正数量事件全程继续计入。
func TestZeroQuantityDedupConflictAndTimezoneIdentity(t *testing.T) {
	s, _ := newTestService(utc(2026, 3, 20, 12, 0))
	mustPlan(t, s, Plan{ID: "p", TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 1, 1, 0, 0))

	// 既有正数量事件：二月 4、三月 6，用于验证冲突与零数量都不动它们。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "feb-pos", At: utc(2026, 2, 10, 0, 0), Quantity: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "mar-pos", At: utc(2026, 3, 10, 0, 0), Quantity: 6}); err != nil {
		t.Fatal(err)
	}

	// 零数量事件落在 2026-02-01 02:00 UTC（二月）。
	zeroAt := utc(2026, 2, 1, 2, 0)
	z := Event{AccountID: "a", EventID: "z", At: zeroAt, Quantity: 0}
	if r, err := s.RecordEvent(z); err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("first zero event = %+v %v, want accepted into february", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 4 {
		t.Fatalf("february usage after zero = %d, want 4", u.Total)
	}

	// 原样重报：成功但不再接收，继续返回原账期二月。
	if r, err := s.RecordEvent(z); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("identical zero replay = %+v %v, want dedup into february", r, err)
	}

	// 把数量改为正数、时刻不变：冲突。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "z", At: zeroAt, Quantity: 7}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("same id with positive quantity = %v, want ErrEventConflict", err)
	}
	// 把发生时刻改成另一个实际瞬间（另一个月份三月）、数量仍为 0：冲突。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "z", At: utc(2026, 3, 15, 0, 0), Quantity: 0}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("same id with moved instant = %v, want ErrEventConflict", err)
	}

	// 冲突不改变任何月份：原月份二月仍 4，改后时刻所属月份三月仍 6，
	// 且原零数量事件仍归二月。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 4 {
		t.Fatalf("february usage after conflicts = %d, want 4", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 6 {
		t.Fatalf("march usage after conflicts = %d, want 6", u.Total)
	}
	if r, err := s.RecordEvent(z); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("original zero event not preserved: %+v %v", r, err)
	}

	// 只改变时区表示、不改变实际瞬间：仍是同一事件，重报成功而非冲突。
	// -05:00 表示本地钟面落在 1 月 31 日 21:00，但实际瞬间仍是 02-01 02:00Z，
	// 归期必须仍是二月——身份与账期都按实际瞬间比较。
	minus5Rep := time.Date(2026, time.January, 31, 21, 0, 0, 0, zoneMinus5)
	if !minus5Rep.Equal(zeroAt) {
		t.Fatalf("test setup: minus5 rep %v is not same instant as %v", minus5Rep, zeroAt)
	}
	if minus5Rep.Month() != time.January || MonthOf(minus5Rep) != feb(2026) {
		t.Fatalf("test setup: minus5 wall month=%s period=%s, want January/2026-02", minus5Rep.Month(), MonthOf(minus5Rep))
	}
	if r, err := s.RecordEvent(Event{AccountID: "a", EventID: "z", At: minus5Rep, Quantity: 0}); err != nil ||
		r.Accepted || r.Period != feb(2026) {
		t.Fatalf("minus5 same-instant replay = %+v %v, want dedup into february", r, err)
	}
	// +08:00 表示（本地 02-01 10:00）同样是同一瞬间。
	plus8Rep := time.Date(2026, time.February, 1, 10, 0, 0, 0, zonePlus8)
	if !plus8Rep.Equal(zeroAt) {
		t.Fatalf("test setup: plus8 rep is not the same instant")
	}
	if r, err := s.RecordEvent(Event{AccountID: "a", EventID: "z", At: plus8Rep, Quantity: 0}); err != nil ||
		r.Accepted || r.Period != feb(2026) {
		t.Fatalf("plus8 same-instant replay = %+v %v, want dedup into february", r, err)
	}

	// 上报结果、月用量查询、账户状态三者一致；账户内既有正数量事件继续计入。
	st, _ := s.Status("a")
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: feb(2026), Total: 4}) ||
		st.MonthlyUsage[1] != (Usage{Period: mar(2026), Total: 6}) {
		t.Fatalf("status usage = %+v, want [feb 4, mar 6]", st.MonthlyUsage)
	}
}

// TestZeroQuantityRejectedForBilledMonthButAcceptedReplaySucceeds 锁定零数量
// 不能绕过“已出账月份拒绝新事件”：已出账月份的新标识零数量事件返回
// ErrMonthBilled；此前已接收的零数量事件原样重报仍成功，账单用量与金额固定。
// 被出账拒绝的事件不占用标识（可在另一个未出账月份以同标识首次接收）。
func TestZeroQuantityRejectedForBilledMonthButAcceptedReplaySucceeds(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 20, 12, 0))
	// 月费 1000、额度 0、超额 100/单位、无税：5 单位 → 超额费 500、应付 1500。
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 100, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 1, 1, 0, 0))

	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "jan-pos", At: utc(2026, 1, 10, 0, 0), Quantity: 5}); err != nil {
		t.Fatal(err)
	}
	// 出账前先成功接收一条一月的零数量事件，一月累计仍为 5。
	zJan := Event{AccountID: "a", EventID: "z-jan", At: utc(2026, 1, 15, 0, 0), Quantity: 0}
	if r, err := s.RecordEvent(zJan); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("zero event before billing = %+v %v", r, err)
	}

	clk.t = utc(2026, 2, 1, 0, 0)
	bill, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatalf("create january bill: %v", err)
	}
	if bill.TotalUsage != 5 || bill.OverageUnits != 5 || bill.OverageFee != 500 ||
		bill.MonthlyFee != 1000 || bill.Tax != 0 || bill.TotalDue != 1500 ||
		bill.Balance != 1500 || bill.Settled {
		t.Fatalf("january bill = %+v, want usage 5 / due 1500 unpaid", bill)
	}

	// 已出账月份的“新标识”零数量事件：返回月份已出账错误，不留用量。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "z-new", At: utc(2026, 1, 12, 0, 0), Quantity: 0}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new zero event in billed month = %v, want ErrMonthBilled", err)
	}

	// 此前成功接收的零数量事件原样重报：仍成功、不再次接收、返回原账期，
	// 即使一月已出账也不受影响。
	if r, err := s.RecordEvent(zJan); err != nil || r.Accepted || r.Period != jan(2026) {
		t.Fatalf("accepted zero replay after billed = %+v %v", r, err)
	}

	// 账单用量与金额保持固定，重报与被拒新事件都不改账单。
	if got, _ := s.GetBill("a", jan(2026)); got != bill {
		t.Fatalf("january bill changed: %+v want %+v", got, bill)
	}
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 5 {
		t.Fatalf("january usage changed = %d, want 5", u.Total)
	}

	// 被出账拒绝不占用标识：截止日之前（二月二日，账户未停用）把同一标识
	// 首次上报到尚未出账的二月，应被首次接收。若一月那次拒绝留下过记录，
	// 这里只会得到冲突——以此证明失败没有消耗事件标识。
	clk.t = utc(2026, 2, 2, 12, 0)
	r, err := s.RecordEvent(Event{AccountID: "a", EventID: "z-new", At: utc(2026, 2, 1, 0, 0), Quantity: 0})
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("rejected id reused in unbilled month = %+v %v, want first acceptance into february", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 0 {
		t.Fatalf("february usage = %d, want 0", u.Total)
	}
	if got, _ := s.GetBill("a", jan(2026)); got != bill {
		t.Fatalf("january bill changed after reuse: %+v want %+v", got, bill)
	}
}

// TestZeroQuantityRejectedWhileSuspendedThenAcceptedAfterPayment 锁定零数量
// 不能绕过欠费停用：到期欠费停用时，对尚未出账月份首次上报零数量事件返回
// ErrSuspended，不增加月份记录、不占用标识；结清欠费后（订阅仍有效、该月
// 尚未出账）原请求可作为首次事件被接收，再次重报才算重复。正数量事件的
// 既有停用/恢复行为同时保留，一月账单金额固定。
func TestZeroQuantityRejectedWhileSuspendedThenAcceptedAfterPayment(t *testing.T) {
	s, clk := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 1, 1, 0, 0))

	// 一月账单 1000，截止 2026-02-08；到点欠费停用，订阅本身仍有效。
	if _, err := s.CreateBill("a", jan(2026)); err != nil {
		t.Fatalf("create january bill: %v", err)
	}
	clk.t = utc(2026, 2, 8, 0, 0)
	st, _ := s.Status("a")
	if !st.Suspended || !st.Subscribed {
		t.Fatalf("status = suspended %v subscribed %v, want both true", st.Suspended, st.Subscribed)
	}

	// 对尚未出账的二月首次上报零数量事件：停用错误。
	zFeb := Event{AccountID: "a", EventID: "z-feb", At: utc(2026, 2, 5, 8, 0), Quantity: 0}
	if _, err := s.RecordEvent(zFeb); !errors.Is(err, ErrSuspended) {
		t.Fatalf("zero event while suspended = %v, want ErrSuspended", err)
	}
	// 正数量的新事件同样被拒：零数量没有获得任何绕过限制的特权。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "pos-feb", At: utc(2026, 2, 6, 0, 0), Quantity: 2}); !errors.Is(err, ErrSuspended) {
		t.Fatalf("positive event while suspended = %v, want ErrSuspended", err)
	}

	// 拒绝不留痕迹：二月没有月份记录（区别于接收了零数量事件），累计为 0。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 0 {
		t.Fatalf("february usage after suspension rejection = %d, want 0", u.Total)
	}
	st, _ = s.Status("a")
	if _, ok := findStatusUsage(st, feb(2026)); ok {
		t.Fatalf("february must have no usage record after rejected zero event: %+v", st.MonthlyUsage)
	}

	// 结清到期欠费：停用解除、订阅仍有效。
	pay, err := s.RecordPayment("a", "pay-jan", jan(2026), 1000)
	if err != nil || !pay.Registered || pay.BillBalance != 0 || !pay.Settled {
		t.Fatalf("payment = %+v %v, want registered settlement", pay, err)
	}
	st, _ = s.Status("a")
	if st.Suspended || !st.Subscribed {
		t.Fatalf("status after payment = suspended %v subscribed %v, want false/true", st.Suspended, st.Subscribed)
	}

	// 原零数量请求此刻作为“首次事件”被接收（Accepted=true）：这直接证明停用
	// 期间的拒绝没有占用标识、也没有留下事件记录——否则这里只会是去重或冲突。
	r, err := s.RecordEvent(zFeb)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("zero event after recovery = %+v %v, want first acceptance into february", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 0 {
		t.Fatalf("february usage after accepted zero = %d, want 0", u.Total)
	}
	st, _ = s.Status("a")
	if got, ok := findStatusUsage(st, feb(2026)); !ok || got != (Usage{Period: feb(2026), Total: 0}) {
		t.Fatalf("february record missing after recovery acceptance: %+v", st.MonthlyUsage)
	}

	// 再次原样重报才算重复：不再接收，返回原账期。
	if r, err := s.RecordEvent(zFeb); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("zero replay after recovery = %+v %v, want dedup into february", r, err)
	}

	// 停用期间同被拒绝的正数量事件，恢复后同样首次接收并累加，保留正数量的
	// 既有行为；二月累计变为 2，零数量事件不受影响。
	if r, err := s.RecordEvent(Event{AccountID: "a", EventID: "pos-feb", At: utc(2026, 2, 6, 0, 0), Quantity: 2}); err != nil ||
		!r.Accepted || r.Period != feb(2026) {
		t.Fatalf("positive event after recovery = %+v %v, want first acceptance", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 2 {
		t.Fatalf("february usage = %d, want 2", u.Total)
	}
	if r, err := s.RecordEvent(zFeb); err != nil || r.Accepted {
		t.Fatalf("zero event must still dedup after positive event: %+v %v", r, err)
	}

	// 一月账单金额固定、现已结清。
	b, err := s.GetBill("a", jan(2026))
	if err != nil {
		t.Fatalf("get january bill: %v", err)
	}
	if b.TotalDue != 1000 || b.Paid != 1000 || b.Balance != 0 || !b.Settled {
		t.Fatalf("january bill after payment = %+v, want due/paid 1000 settled", b)
	}
}

// TestZeroQuantityIsWithinNonNegativeContract 固定入参边界：负数仍按非法参数
// 拒绝，数量恰为 0 属于合法的非负数量，按正常事件接收。
func TestZeroQuantityIsWithinNonNegativeContract(t *testing.T) {
	s, _ := newTestService(utc(2026, 1, 20, 12, 0))
	mustPlan(t, s, Plan{ID: "p", TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 1, 1, 0, 0))

	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "neg", At: utc(2026, 1, 10, 0, 0), Quantity: -1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative quantity = %v, want ErrInvalidArgument", err)
	}
	r, err := s.RecordEvent(Event{AccountID: "a", EventID: "zero", At: utc(2026, 1, 10, 0, 0), Quantity: 0})
	if err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("zero quantity = %+v %v, want accepted into january", r, err)
	}
}
