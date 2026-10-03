package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“按实际发生时刻归入 UTC 自然月、同一时刻的不同表示不改变事件身份”
// 提供时区维度的回归保障：业务账户提交的事件时间可以携带任意正负偏移，
// 服务既不能按时间字面上的本地年月归账，也不能按上报时刻归账。
var (
	zonePlus8   = time.FixedZone("UTC+8", 8*60*60)
	zonePlus530 = time.FixedZone("UTC+5:30", 5*60*60+30*60)
	zoneMinus5  = time.FixedZone("UTC-5", -5*60*60)
)

// TestTimezoneOffsetAttributionAndBilling 覆盖题述完整正常使用链路：
// 账户自 2026-02-01 起使用同一套餐（月费 1000 分、含 10 单位、超额 100 分/单位、
// 税率 10%），两条事件都在实际发生后的 3 月补报；
// 本地钟面在 3 月的 +08:00 事件实际发生于 2 月，本地钟面在 2 月的 -05:00 事件
// 实际发生于 3 月。归期在上报返回值、各月累计用量与后续账单中必须一致。
func TestTimezoneOffsetAttributionAndBilling(t *testing.T) {
	// 上报时刻为 3 月：归账只取决于事件实际发生时刻，与上报时刻无关。
	s, clk := newTestService(utc(2026, 3, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 2, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// 本地钟面 2026-03-01 00:30（UTC+08:00）= 2026-02-28 16:30 UTC → 2 月，12 单位。
	eventFeb := Event{
		AccountID: "a", EventID: "evt-plus8-march-wall",
		At:       time.Date(2026, time.March, 1, 0, 30, 0, 0, zonePlus8),
		Quantity: 12,
	}
	// 本地钟面 2026-02-28 20:30（UTC-05:00）= 2026-03-01 01:30 UTC → 3 月，8 单位。
	eventMar := Event{
		AccountID: "a", EventID: "evt-minus5-february-wall",
		At:       time.Date(2026, time.February, 28, 20, 30, 0, 0, zoneMinus5),
		Quantity: 8,
	}

	// 锁定测试前提：提交时间的本地年月与应归账期恰好相反，
	// 任何按本地年月或上报时刻归账的实现都会在此暴露。
	if eventFeb.At.Month() != time.March || eventMar.At.Month() != time.February {
		t.Fatalf("submitted wall months = %s and %s, want March and February",
			eventFeb.At.Month(), eventMar.At.Month())
	}
	if !eventFeb.At.UTC().Equal(utc(2026, 2, 28, 16, 30)) {
		t.Fatalf("plus8 event instant = %v, want 2026-02-28 16:30 UTC", eventFeb.At.UTC())
	}
	if !eventMar.At.UTC().Equal(utc(2026, 3, 1, 1, 30)) {
		t.Fatalf("minus5 event instant = %v, want 2026-03-01 01:30 UTC", eventMar.At.UTC())
	}

	rFeb, err := s.RecordEvent(eventFeb)
	if err != nil || !rFeb.Accepted || rFeb.Period != feb(2026) {
		t.Fatalf("plus8 event result: %+v %v", rFeb, err)
	}
	rMar, err := s.RecordEvent(eventMar)
	if err != nil || !rMar.Accepted || rMar.Period != mar(2026) {
		t.Fatalf("minus5 event result: %+v %v", rMar, err)
	}

	// 2 月只累计 12，3 月只累计 8；MonthlyUsage 与 Status 口径一致，且无其他月份。
	if u, err := s.MonthlyUsage("a", feb(2026)); err != nil || u.Total != 12 {
		t.Fatalf("feb usage = %+v %v, want 12", u, err)
	}
	if u, err := s.MonthlyUsage("a", mar(2026)); err != nil || u.Total != 8 {
		t.Fatalf("mar usage = %+v %v, want 8", u, err)
	}
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: feb(2026), Total: 12}) ||
		st.MonthlyUsage[1] != (Usage{Period: mar(2026), Total: 8}) {
		t.Fatalf("status monthly usage = %+v, want [feb 12, mar 8]", st.MonthlyUsage)
	}

	wantTerms := PlanTerms{PlanID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000}

	// 2 月账期结束后出账：超额 2 单位，超额费 200 分，
	// 税 (1000+200)*10% = 120 分，应付 1320 分，沿用完整月费、额度与税额规则。
	febBill, err := s.CreateBill("a", feb(2026))
	if err != nil {
		t.Fatalf("create feb bill: %v", err)
	}
	wantFebBill := Bill{
		AccountID: "a", Period: feb(2026), Terms: wantTerms,
		TotalUsage: 12, IncludedUnits: 10, OverageUnits: 2,
		MonthlyFee: 1000, OverageFee: 200, Tax: 120, TotalDue: 1320,
		Paid: 0, Balance: 1320, Settled: false,
		DueAt: utc(2026, 3, 8, 0, 0),
	}
	if febBill != wantFebBill {
		t.Fatalf("feb bill = %+v, want %+v", febBill, wantFebBill)
	}

	// 已出账后，换时区表示的同一事件重报仍成功（此处 2 月账单已过付款截止日，
	// 账户处于欠费停用，重报同样不受影响）：返回原账期、不再累计。
	if r, err := s.RecordEvent(Event{
		AccountID: "a", EventID: eventFeb.EventID,
		At: eventFeb.At.UTC(), Quantity: 12,
	}); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("utc replay after billed: %+v %v", r, err)
	}
	// 新事件标识即使发生在该已出账月份（本地 02-15 10:00 +08 == 02-15 02:00Z）也拒绝。
	if _, err := s.RecordEvent(Event{
		AccountID: "a", EventID: "new-feb-event",
		At:       time.Date(2026, time.February, 15, 10, 0, 0, 0, zonePlus8),
		Quantity: 1,
	}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new event in billed february = %v, want ErrMonthBilled", err)
	}
	if got, _ := s.GetBill("a", feb(2026)); got != febBill {
		t.Fatalf("feb bill changed after replay/reject: %+v", got)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 12 {
		t.Fatalf("feb usage changed after billing: %d", u.Total)
	}

	// 进入 4 月后为 3 月出账：8 单位未超 10 单位额度，无超额，
	// 税 1000*10% = 100 分，应付 1100 分。
	clk.t = utc(2026, 4, 1, 0, 0)
	marBill, err := s.CreateBill("a", mar(2026))
	if err != nil {
		t.Fatalf("create mar bill: %v", err)
	}
	wantMarBill := Bill{
		AccountID: "a", Period: mar(2026), Terms: wantTerms,
		TotalUsage: 8, IncludedUnits: 10, OverageUnits: 0,
		MonthlyFee: 1000, OverageFee: 0, Tax: 100, TotalDue: 1100,
		Paid: 0, Balance: 1100, Settled: false,
		DueAt: utc(2026, 4, 8, 0, 0),
	}
	if marBill != wantMarBill {
		t.Fatalf("mar bill = %+v, want %+v", marBill, wantMarBill)
	}
	// 已出账后换 +08:00 表示重报 3 月事件（03-01 09:30 +08 == 01:30Z）：返回原账期。
	if r, err := s.RecordEvent(Event{
		AccountID: "a", EventID: eventMar.EventID,
		At:       time.Date(2026, time.March, 1, 9, 30, 0, 0, zonePlus8),
		Quantity: 8,
	}); err != nil || r.Accepted || r.Period != mar(2026) {
		t.Fatalf("mar replay after billed: %+v %v", r, err)
	}
	// 3 月新标识事件同样拒绝（本地 03-10 12:00 -05 == 03-10 17:00Z）。
	if _, err := s.RecordEvent(Event{
		AccountID: "a", EventID: "new-mar-event",
		At:       time.Date(2026, time.March, 10, 12, 0, 0, 0, zoneMinus5),
		Quantity: 1,
	}); !errors.Is(err, ErrMonthBilled) {
		t.Fatalf("new event in billed march = %v, want ErrMonthBilled", err)
	}
	if got, _ := s.GetBill("a", mar(2026)); got != marBill {
		t.Fatalf("mar bill changed after replay/reject: %+v", got)
	}

	// 最终：归期在上报返回值、各月累计与账单中始终一致，金额与用量固定。
	st, err = s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: feb(2026), Total: 12}) ||
		st.MonthlyUsage[1] != (Usage{Period: mar(2026), Total: 8}) {
		t.Fatalf("final monthly usage = %+v", st.MonthlyUsage)
	}
	if len(st.Bills) != 2 ||
		st.Bills[0] != (BillSummary{Period: feb(2026), TotalDue: 1320, Paid: 0, Balance: 1320, Settled: false, DueAt: utc(2026, 3, 8, 0, 0)}) ||
		st.Bills[1] != (BillSummary{Period: mar(2026), TotalDue: 1100, Paid: 0, Balance: 1100, Settled: false, DueAt: utc(2026, 4, 8, 0, 0)}) {
		t.Fatalf("final bill summaries = %+v", st.Bills)
	}
}

// TestEventInstantIdentityAcrossTimezoneRepresentations 锁定事件身份规则：
// 同一实际时刻的任意时区表示（含非整点偏移）都是同一事件；保留本地钟面但换时区、
// 实际时刻改变时，同一事件标识必须冲突，且冲突不改变用量。
func TestEventInstantIdentityAcrossTimezoneRepresentations(t *testing.T) {
	s, _ := newTestService(utc(2026, 3, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 2, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}

	// 原始表示：本地 03-01 00:30 +08，实际时刻 02-28 16:30Z，归入 2 月。
	orig := Event{
		AccountID: "a", EventID: "evt",
		At:       time.Date(2026, time.March, 1, 0, 30, 0, 0, zonePlus8),
		Quantity: 12,
	}
	r, err := s.RecordEvent(orig)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("original event: %+v %v", r, err)
	}

	// 同一实际时刻的 -05:00 表示（本地 02-28 11:30）：重报成功，不再次累计。
	sameMinus5 := Event{
		AccountID: "a", EventID: "evt",
		At:       time.Date(2026, time.February, 28, 11, 30, 0, 0, zoneMinus5),
		Quantity: 12,
	}
	if !sameMinus5.At.Equal(orig.At) {
		t.Fatal("test setup: minus5 representation is not the same instant")
	}
	if r, err := s.RecordEvent(sameMinus5); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("minus5 same-instant replay: %+v %v", r, err)
	}
	// 非整点偏移 +05:30（本地 02-28 22:00）与纯 UTC 表示同样是同一事件。
	samePlus530 := Event{
		AccountID: "a", EventID: "evt",
		At:       time.Date(2026, time.February, 28, 22, 0, 0, 0, zonePlus530),
		Quantity: 12,
	}
	if !samePlus530.At.Equal(orig.At) {
		t.Fatal("test setup: plus5:30 representation is not the same instant")
	}
	if r, err := s.RecordEvent(samePlus530); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("plus5:30 same-instant replay: %+v %v", r, err)
	}
	if r, err := s.RecordEvent(Event{
		AccountID: "a", EventID: "evt",
		At: orig.At.UTC(), Quantity: 12,
	}); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("utc same-instant replay: %+v %v", r, err)
	}

	// 保留本地钟面 03-01 00:30，但换用 -05:00：实际时刻变为 03-01 05:30Z（已是 3 月）。
	shifted := Event{
		AccountID: "a", EventID: "evt",
		At:       time.Date(2026, time.March, 1, 0, 30, 0, 0, zoneMinus5),
		Quantity: 12,
	}
	if shifted.At.Equal(orig.At) {
		t.Fatal("test setup: shifted representation is unexpectedly the same instant")
	}
	if _, err := s.RecordEvent(shifted); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("same wall clock, shifted instant = %v, want ErrEventConflict", err)
	}
	// 数量不变、时刻改变也是冲突；时刻相同、数量改变同样冲突。
	if _, err := s.RecordEvent(Event{
		AccountID: "a", EventID: "evt",
		At: shifted.At, Quantity: 99,
	}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("shifted instant with other qty = %v, want ErrEventConflict", err)
	}
	if _, err := s.RecordEvent(Event{
		AccountID: "a", EventID: "evt",
		At: orig.At.UTC(), Quantity: 13,
	}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("same instant with changed qty = %v, want ErrEventConflict", err)
	}

	// 冲突既不增加用量，也不改变归期：2 月仍只累计 12，3 月没有任何用量。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 12 {
		t.Fatalf("feb usage after conflicts = %d, want 12", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 0 {
		t.Fatalf("mar usage after conflicts = %d, want 0", u.Total)
	}
}

// TestUTCMonthBoundaryNanosecondAcrossZones 锁定 UTC 月初边界的纳秒精度：
// 零点前一纳秒归前一期，恰在零点归新一期；即使两条事件在提交时区显示同一个
// 本地月份，也不能混到一起。事件身份同样精确到纳秒，不能按秒截断比较。
func TestUTCMonthBoundaryNanosecondAcrossZones(t *testing.T) {
	s, _ := newTestService(utc(2026, 3, 2, 12, 0))
	mustPlan(t, s, Plan{ID: "p", TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	if err := s.Subscribe("a", "p", utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatal(err)
	}

	// +08:00 一侧：两条事件的本地钟面都显示在 3 月 1 日，却分属 2 月与 3 月。
	// 本地 03-01 07:59:59.999999999 +08 == 02-28 23:59:59.999999999 UTC（前一纳秒）。
	beforePlus8 := time.Date(2026, time.March, 1, 7, 59, 59, 999999999, zonePlus8)
	// 本地 03-01 08:00:00 +08 == 03-01 00:00:00 UTC（恰在零点）。
	exactPlus8 := time.Date(2026, time.March, 1, 8, 0, 0, 0, zonePlus8)
	wantBeforeFeb := time.Date(2026, time.February, 28, 23, 59, 59, 999999999, time.UTC)
	wantExactMar := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	if !beforePlus8.UTC().Equal(wantBeforeFeb) {
		t.Fatalf("before instant = %v, want %v", beforePlus8.UTC(), wantBeforeFeb)
	}
	if !exactPlus8.UTC().Equal(wantExactMar) {
		t.Fatalf("exact instant = %v, want %v", exactPlus8.UTC(), wantExactMar)
	}
	if beforePlus8.Month() != time.March || exactPlus8.Month() != time.March {
		t.Fatalf("plus8 wall months = %s, %s, want both March", beforePlus8.Month(), exactPlus8.Month())
	}
	// MonthOf 只认实际时刻：给同一时刻挂上任何时区表示，账期不变。
	if MonthOf(beforePlus8) != feb(2026) || MonthOf(beforePlus8.In(zoneMinus5)) != feb(2026) {
		t.Fatal("one nanosecond before March must stay in February")
	}
	if MonthOf(exactPlus8) != mar(2026) || MonthOf(exactPlus8.In(zoneMinus5)) != mar(2026) {
		t.Fatal("exact zero must fall in March")
	}

	rBefore, err := s.RecordEvent(Event{AccountID: "a", EventID: "ns-before", At: beforePlus8, Quantity: 5})
	if err != nil || !rBefore.Accepted || rBefore.Period != feb(2026) {
		t.Fatalf("one-ns-before event: %+v %v", rBefore, err)
	}
	rExact, err := s.RecordEvent(Event{AccountID: "a", EventID: "ns-exact", At: exactPlus8, Quantity: 7})
	if err != nil || !rExact.Accepted || rExact.Period != mar(2026) {
		t.Fatalf("exact-zero event: %+v %v", rExact, err)
	}
	// 同一 UTC 秒内带纳秒分量的事件（本地 03-01 07:59:59.500000001 +08
	// == 02-28 23:59:59.500000001 UTC），用于锁定身份比较不截断纳秒。
	midInstant := time.Date(2026, time.February, 28, 23, 59, 59, 500000001, time.UTC)
	rMid, err := s.RecordEvent(Event{
		AccountID: "a", EventID: "ns-mid",
		At: time.Date(2026, time.March, 1, 7, 59, 59, 500000001, zonePlus8), Quantity: 2,
	})
	if err != nil || !rMid.Accepted || rMid.Period != feb(2026) {
		t.Fatalf("sub-second event: %+v %v", rMid, err)
	}

	// -05:00 一侧的 1/2 月边界：两条事件本地钟面都显示在 1 月 31 日，却分属 1 月与 2 月。
	// 本地 01-31 18:59:59.999999999 -05 == 01-31 23:59:59.999999999 UTC（前一纳秒）。
	beforeMinus5 := time.Date(2026, time.January, 31, 18, 59, 59, 999999999, zoneMinus5)
	// 本地 01-31 19:00:00 -05 == 02-01 00:00:00 UTC（恰在零点）。
	exactMinus5 := time.Date(2026, time.January, 31, 19, 0, 0, 0, zoneMinus5)
	if beforeMinus5.Month() != time.January || exactMinus5.Month() != time.January {
		t.Fatalf("minus5 wall months = %s, %s, want both January", beforeMinus5.Month(), exactMinus5.Month())
	}
	if MonthOf(beforeMinus5) != jan(2026) || MonthOf(exactMinus5) != feb(2026) {
		t.Fatalf("minus5 boundary periods = %s, %s, want 2026-01, 2026-02",
			MonthOf(beforeMinus5), MonthOf(exactMinus5))
	}
	if r, err := s.RecordEvent(Event{AccountID: "a", EventID: "ns-before-jan", At: beforeMinus5, Quantity: 3}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("jan one-ns-before event: %+v %v", r, err)
	}
	if r, err := s.RecordEvent(Event{AccountID: "a", EventID: "ns-exact-jan", At: exactMinus5, Quantity: 4}); err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("feb exact-zero event: %+v %v", r, err)
	}

	// 用量按纳秒边界各归各期：1 月 3，2 月 5+2+4=11，3 月 7。
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 3 {
		t.Fatalf("jan usage = %d, want 3", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 11 {
		t.Fatalf("feb usage = %d, want 11", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 7 {
		t.Fatalf("mar usage = %d, want 7", u.Total)
	}

	// 前一纳秒事件换 UTC 表示重报：同一事件，返回原账期 2 月，不再累计。
	if r, err := s.RecordEvent(Event{AccountID: "a", EventID: "ns-before", At: wantBeforeFeb, Quantity: 5}); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("utc replay of one-ns-before: %+v %v", r, err)
	}
	// 同一 UTC 秒内仅相差 1 纳秒、实际时刻已变：同一标识必须冲突——
	// 防止把时刻截断到秒再比较。
	if _, err := s.RecordEvent(Event{
		AccountID: "a", EventID: "ns-mid",
		At:       midInstant.Add(time.Nanosecond),
		Quantity: 2,
	}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("one-nanosecond shift within same second = %v, want ErrEventConflict", err)
	}
	// 仅相差 1 纳秒、实际时刻已变：同一标识必须冲突——防止把时刻截断到秒再比较。
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "ns-before", At: exactPlus8, Quantity: 5}); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("one-nanosecond shift = %v, want ErrEventConflict", err)
	}
	// 零点事件换 -05:00 表示（本地 02-28 19:00）重报：仍是同一事件，账期 3 月。
	if r, err := s.RecordEvent(Event{AccountID: "a", EventID: "ns-exact", At: exactPlus8.In(zoneMinus5), Quantity: 7}); err != nil || r.Accepted || r.Period != mar(2026) {
		t.Fatalf("minus5 replay of exact-zero: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 11 {
		t.Fatalf("feb usage after boundary replays = %d, want 11", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 7 {
		t.Fatalf("mar usage after boundary replays = %d, want 7", u.Total)
	}

	// “晚于当前时刻”的判定同样基于实际时刻而非本地钟面：
	// 本地 03-02 21:00 +08 == 13:00Z，晚于当前 12:00Z。
	if _, err := s.RecordEvent(Event{
		AccountID: "a", EventID: "zoned-future",
		At:       time.Date(2026, time.March, 2, 21, 0, 0, 0, zonePlus8),
		Quantity: 1,
	}); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("zoned future event = %v, want ErrEventInFuture", err)
	}
}
