package meter

import (
	"errors"
	"testing"
	"time"
)

// 时区回归保障：用量按“实际发生时刻”归入 UTC 自然月，同一实际时刻的不同
// 时区表示不改变事件身份。业务账户提交的时间可以带正负时区偏移：
//   - 不能按时间中的本地年月归账；
//   - 不能按上报时刻归账；
//   - 同一实际时刻换一个时区表示、保持事件标识与数量不变，是已接收事件的
//     重报，返回原账期且不再累计；
//   - 保留同一本地钟面时间但换时区导致实际时刻改变时，同一事件标识必须冲突；
//   - UTC 月初零点前一纳秒归前一期，恰在零点归新一期，即使二者在提交时区
//     显示同一个本地月份也不能混到一起；
//   - 已出账后，换时区表示的相同事件重报仍成功；新事件标识即使发生于已出账
//     月份也按现有规则拒绝，账单金额与用量保持固定。

var (
	locPlus8  = time.FixedZone("UTC+08:00", 8*60*60)
	locMinus5 = time.FixedZone("UTC-05:00", -5*60*60)
)

// localAt 按 loc 时区的本地钟面日期构造一个实际时刻。
func localAt(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, loc)
}

// tzBillingFixture 构造规格中的正常使用账户：
// 月费 1000 分、每月含 10 单位、超额 100 分/单位、税率 10%；
// 订阅自 2026-02-01 00:00 UTC 起持续有效，时钟在 3 月（两条事件均已发生之后）。
func tzBillingFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(utc(2026, 3, 2, 12, 0))
	mustPlan(t, s, planDef("p", 1000, 10, 100, 1000))
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 2, 1, 0, 0))
	return s, clk
}

// tzScenarioEvents 上报规格中的两条事件，返回这两条事件本身。
// 两条事件在 3 月一并上报，但按各自实际发生时刻归账：
//   - 2026-03-01 00:30，UTC+08:00（= 2026-02-28 16:30 UTC）→ 2 月，12 单位；
//   - 2026-02-28 20:30，UTC-05:00（= 2026-03-01 01:30 UTC）→ 3 月，8 单位。
func tzScenarioEvents(t *testing.T, s *Service) (Event, Event) {
	t.Helper()
	febEvent := Event{
		AccountID: "a",
		EventID:   "evt-feb",
		At:        localAt(2026, time.March, 1, 0, 30, locPlus8),
		Quantity:  12,
	}
	marEvent := Event{
		AccountID: "a",
		EventID:   "evt-mar",
		At:        localAt(2026, time.February, 28, 20, 30, locMinus5),
		Quantity:  8,
	}

	// 先钉死两条本地钟面时间对应的实际时刻，避免测试本身把时区算错。
	if !febEvent.At.Equal(utc(2026, 2, 28, 16, 30)) {
		t.Fatalf("+08:00 event instant = %v, want 2026-02-28T16:30:00Z", febEvent.At.UTC())
	}
	if !marEvent.At.Equal(utc(2026, 3, 1, 1, 30)) {
		t.Fatalf("-05:00 event instant = %v, want 2026-03-01T01:30:00Z", marEvent.At.UTC())
	}

	r1, err := s.RecordEvent(febEvent)
	if err != nil || !r1.Accepted || r1.Period != feb(2026) {
		t.Fatalf("+08:00 event attributed by local/report month: %+v %v", r1, err)
	}
	r2, err := s.RecordEvent(marEvent)
	if err != nil || !r2.Accepted || r2.Period != mar(2026) {
		t.Fatalf("-05:00 event attributed by local/report month: %+v %v", r2, err)
	}
	return febEvent, marEvent
}

// TestTimezoneAttributionByInstant 按实际发生时刻归入 UTC 自然月：
// 本地钟面在 3 月的事件可以归 2 月，本地钟面在 2 月的事件可以归 3 月；
// 上报时刻同在 3 月不影响归期；各月累计用量与随后账单一致。
func TestTimezoneAttributionByInstant(t *testing.T) {
	s, clk := tzBillingFixture(t)
	tzScenarioEvents(t, s)

	// 2 月只累计 12，3 月只累计 8；不按本地年月、也不按上报时刻（均在 3 月）归账。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 12 {
		t.Fatalf("feb usage = %d, want 12", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 8 {
		t.Fatalf("mar usage = %d, want 8", u.Total)
	}
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: feb(2026), Total: 12}) ||
		st.MonthlyUsage[1] != (Usage{Period: mar(2026), Total: 8}) {
		t.Fatalf("monthly usage = %+v", st.MonthlyUsage)
	}

	// 3 月尚未结束时不能给 3 月出账；2 月已结束，但其账单放到 4 月一并生成。
	if _, err := s.CreateBill("a", mar(2026)); !errors.Is(err, ErrBillMonthNotEnded) {
		t.Fatalf("open march bill: %v", err)
	}

	clk.t = utc(2026, 4, 1, 0, 0)
	febBill, err := s.CreateBill("a", feb(2026))
	if err != nil {
		t.Fatalf("feb bill: %v", err)
	}
	// 12 单位：超额 2；(1000 + 2*100) * 10% = 120；应付 1320。
	if febBill.TotalUsage != 12 || febBill.IncludedUnits != 10 || febBill.OverageUnits != 2 {
		t.Fatalf("feb usage detail: %+v", febBill)
	}
	if febBill.MonthlyFee != 1000 || febBill.OverageFee != 200 ||
		febBill.Tax != 120 || febBill.TotalDue != 1320 || febBill.Balance != 1320 {
		t.Fatalf("feb amounts: %+v", febBill)
	}
	if !febBill.DueAt.Equal(utc(2026, 3, 8, 0, 0)) {
		t.Fatalf("feb due at = %v", febBill.DueAt)
	}

	marBill, err := s.CreateBill("a", mar(2026))
	if err != nil {
		t.Fatalf("mar bill: %v", err)
	}
	// 8 单位：没有超额；1000 * 10% = 100；应付 1100。
	if marBill.TotalUsage != 8 || marBill.IncludedUnits != 10 || marBill.OverageUnits != 0 {
		t.Fatalf("mar usage detail: %+v", marBill)
	}
	if marBill.MonthlyFee != 1000 || marBill.OverageFee != 0 ||
		marBill.Tax != 100 || marBill.TotalDue != 1100 || marBill.Balance != 1100 {
		t.Fatalf("mar amounts: %+v", marBill)
	}
	if !marBill.DueAt.Equal(utc(2026, 4, 8, 0, 0)) {
		t.Fatalf("mar due at = %v", marBill.DueAt)
	}

	// 归期在上报返回值、各月累计与账单之间一致，重复出账仍是同一张。
	if again, err := s.CreateBill("a", feb(2026)); err != nil || again != febBill {
		t.Fatalf("feb bill not idempotent: %+v vs %+v err=%v", again, febBill, err)
	}
	if again, err := s.CreateBill("a", mar(2026)); err != nil || again != marBill {
		t.Fatalf("mar bill not idempotent: %+v vs %+v err=%v", again, marBill, err)
	}
}

// TestTimezoneReplaySameInstantDifferentOffset 同一实际时刻换时区表示、
// 标识与数量不变：重报成功、返回原账期、不再增加用量；
// 保留同一本地钟面时间但换时区导致实际时刻变化：必须冲突，原用量不变。
func TestTimezoneReplaySameInstantDifferentOffset(t *testing.T) {
	s, _ := tzBillingFixture(t)
	febEvent, marEvent := tzScenarioEvents(t, s)

	// 2 月那条事件改用 UTC-05:00 表示（本地显示 2026-02-28 11:30），
	// 实际时刻、标识、数量都不变：重报成功且归原账期 2 月。
	r := Event{AccountID: "a", EventID: febEvent.EventID, At: febEvent.At.In(locMinus5), Quantity: 12}
	if got, want := r.At, utc(2026, 2, 28, 16, 30); !got.Equal(want) {
		t.Fatalf("replay instant = %v, want %v", got.UTC(), want)
	}
	if rr, err := s.RecordEvent(r); err != nil || rr.Accepted || rr.Period != feb(2026) {
		t.Fatalf("same instant, other offset replay: %+v %v", rr, err)
	}

	// 3 月那条改用 UTC 表示：同样是重报，归 3 月。
	r2 := Event{AccountID: "a", EventID: marEvent.EventID, At: marEvent.At.UTC(), Quantity: 8}
	if rr, err := s.RecordEvent(r2); err != nil || rr.Accepted || rr.Period != mar(2026) {
		t.Fatalf("same instant in UTC replay: %+v %v", rr, err)
	}

	// 用量不被重报增加。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 12 {
		t.Fatalf("feb usage after replays = %d, want 12", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 8 {
		t.Fatalf("mar usage after replays = %d, want 8", u.Total)
	}

	// 同一本地钟面（03-01 00:30）换至 UTC-05:00，实际时刻变成 05:30 UTC：
	// 同一事件标识必须报冲突，不能按“钟面时间相同”当作重报。
	sameWall := Event{
		AccountID: "a",
		EventID:   febEvent.EventID,
		At:        localAt(2026, time.March, 1, 0, 30, locMinus5),
		Quantity:  12,
	}
	if sameWall.At.Equal(febEvent.At) {
		t.Fatal("test setup: wall-clock-only change unexpectedly same instant")
	}
	if _, err := s.RecordEvent(sameWall); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("same local wall time, shifted instant: %v", err)
	}

	// 反方向同理：02-28 20:30 换至 UTC+08:00 实际时刻变为 12:30 UTC，冲突。
	sameWall2 := Event{
		AccountID: "a",
		EventID:   marEvent.EventID,
		At:        localAt(2026, time.February, 28, 20, 30, locPlus8),
		Quantity:  8,
	}
	if sameWall2.At.Equal(marEvent.At) {
		t.Fatal("test setup: wall-clock-only change unexpectedly same instant")
	}
	if _, err := s.RecordEvent(sameWall2); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("same local wall time, shifted instant (other side): %v", err)
	}

	// 冲突不改变累计用量，也不破坏原事件：原事件换时区重报仍成功。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 12 {
		t.Fatalf("feb usage after conflicts = %d, want 12", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 8 {
		t.Fatalf("mar usage after conflicts = %d, want 8", u.Total)
	}
	if rr, err := s.RecordEvent(r); err != nil || rr.Accepted || rr.Period != feb(2026) {
		t.Fatalf("original replay after conflicts: %+v %v", rr, err)
	}
}

// TestTimezoneMonthBoundaryNanosecond UTC 月初零点前一纳秒归前一期、恰在零点
// 归新一期；两条事件在提交时区（UTC+08:00）都显示本地 3 月，也不能混到一起。
func TestTimezoneMonthBoundaryNanosecond(t *testing.T) {
	s, _ := tzBillingFixture(t)

	marStart := feb(2026).End() // 2026-03-01 00:00:00 UTC
	before := Event{
		AccountID: "a",
		EventID:   "edge-before",
		At:        marStart.Add(-time.Nanosecond).In(locPlus8),
		Quantity:  1,
	}
	at := Event{
		AccountID: "a",
		EventID:   "edge-at",
		At:        marStart.In(locPlus8),
		Quantity:  1,
	}

	// 两条在提交时区都显示本地 3 月（07:59:59.999999999 与 08:00:00）。
	for _, ev := range []Event{before, at} {
		if y, mo, _ := ev.At.In(locPlus8).Date(); y != 2026 || mo != time.March {
			t.Fatalf("event %s local date = %d-%02d, want 2026-03 in +08:00", ev.EventID, y, mo)
		}
	}

	rb, err := s.RecordEvent(before)
	if err != nil || !rb.Accepted || rb.Period != feb(2026) {
		t.Fatalf("one nanosecond before UTC midnight: %+v %v", rb, err)
	}
	ra, err := s.RecordEvent(at)
	if err != nil || !ra.Accepted || ra.Period != mar(2026) {
		t.Fatalf("exactly at UTC midnight: %+v %v", ra, err)
	}

	// 即使本地显示同月，两条事件分属两个 UTC 账期，各累计一次。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 1 {
		t.Fatalf("feb edge usage = %d, want 1", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 1 {
		t.Fatalf("mar edge usage = %d, want 1", u.Total)
	}

	// 反方向：UTC 已进入 3 月、本地仍是 2 月的时刻必须归 3 月
	// （2026-03-01 04:00 UTC = 2026-02-28 23:00，UTC-05:00）。
	cross := Event{
		AccountID: "a",
		EventID:   "edge-local-feb",
		At:        localAt(2026, time.February, 28, 23, 0, locMinus5),
		Quantity:  1,
	}
	if !cross.At.Equal(utc(2026, 3, 1, 4, 0)) {
		t.Fatalf("cross instant = %v, want 2026-03-01T04:00Z", cross.At.UTC())
	}
	if rc, err := s.RecordEvent(cross); err != nil || !rc.Accepted || rc.Period != mar(2026) {
		t.Fatalf("UTC March while local still February: %+v %v", rc, err)
	}
}

// TestTimezoneReplayAfterBilledAndNewEventRejected 出账后：
// 换时区表示的相同事件重报仍成功；新事件标识即使发生于已出账月份也拒绝，
// 账单金额与用量保持固定。
func TestTimezoneReplayAfterBilledAndNewEventRejected(t *testing.T) {
	s, clk := tzBillingFixture(t)
	febEvent, marEvent := tzScenarioEvents(t, s)

	clk.t = utc(2026, 4, 1, 0, 0)
	febBill, err := s.CreateBill("a", feb(2026))
	if err != nil {
		t.Fatal(err)
	}
	marBill, err := s.CreateBill("a", mar(2026))
	if err != nil {
		t.Fatal(err)
	}

	// 已出账后，换时区表示的相同事件重报仍成功，返回原账期、不再累计。
	febReplay := Event{AccountID: "a", EventID: febEvent.EventID, At: febEvent.At.UTC(), Quantity: 12}
	if r, err := s.RecordEvent(febReplay); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("feb replay after billed: %+v %v", r, err)
	}
	marReplay := Event{AccountID: "a", EventID: marEvent.EventID, At: marEvent.At.In(locPlus8), Quantity: 8}
	if r, err := s.RecordEvent(marReplay); err != nil || r.Accepted || r.Period != mar(2026) {
		t.Fatalf("mar replay after billed: %+v %v", r, err)
	}

	// 新事件标识即使发生于已出账月份，也按现有规则拒绝（2 月、3 月各试一条）。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "new-feb", At: utc(2026, 2, 15, 0, 0), Quantity: 1}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new event in billed february: %v", err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "new-mar", At: utc(2026, 3, 15, 0, 0), Quantity: 1}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new event in billed march: %v", err)
	}
	// 被拒事件不消耗标识之外的任何状态：用量固定。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 12 {
		t.Fatalf("feb usage changed after billing: %d", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 8 {
		t.Fatalf("mar usage changed after billing: %d", u.Total)
	}

	// 账单金额与用量明细保持固定。
	if got, _ := s.GetBill("a", feb(2026)); got != febBill {
		t.Fatalf("feb bill changed: %+v vs %+v", got, febBill)
	}
	if got, _ := s.GetBill("a", mar(2026)); got != marBill {
		t.Fatalf("mar bill changed: %+v vs %+v", got, marBill)
	}
}
