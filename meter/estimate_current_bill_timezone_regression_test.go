package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为账期内预计费用查询（EstimateCurrentBill）补充时区维度的回归保障：
//
//   - “当前账期”只由服务时刻所在的 UTC 自然月决定：服务时钟返回的时间可以
//     携带任意偏移，本地钟面已经跨入三月、实际仍是 UTC 二月末时，预估必须
//     仍取二月的用量与条件；同一瞬间换任何时区表示结果相同。
//   - 每条用量只按其“发生时刻”的 UTC 瞬间归月：事件字面上写出的本地月份
//     （无论是 +08:00 写在三月一日凌晨，还是 -05:00 写在二月二十八日晚）都
//     不能决定归期，更不能按提交时刻所在月份把用量混进别的账期。
//   - 预估始终明确标为预估、不落正式账单、不关闭尚未出账月份的补报通道，
//     查询本身不增加也不搬动任何用量。
//
// 场景账户自 2026-02-01 00:00 UTC 起持续订阅同一份套餐（月费 1000 分、
// 包含 10 单位、超额单价 100 分、税率 10%），全程不取消、不换套餐、不出账、
// 无欠费。

// TestEstimateCurrentBillUTCMonthWithTimezoneOffsets 按时间线重演题述完整链路：
// 东八区钟面已入三月但服务时刻实际仍在二月时的二月预估；到达 UTC 三月月初
// 瞬间直接转为三月零用量预估；二月未出账期间补报晚到用量；本地钟面写在
// 二月的 -05:00 事件实际归入三月并计入三月预估。
func TestEstimateCurrentBillUTCMonthWithTimezoneOffsets(t *testing.T) {
	// 初始服务时刻为订阅生效瞬间：2026-02-01 00:00 UTC。
	s, clk := newTestService(utc(2026, 2, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 2, 1, 0, 0))
	terms := planTerms("p", 1000, 10, 100, 1000)

	// 情形一：服务时刻写为 2026-03-01 00:30（UTC+8）。本地钟面已经进入
	// 三月，但同一瞬间是 UTC 2026-02-28 16:30，当前账期仍是二月。
	nowPlus8 := time.Date(2026, time.March, 1, 0, 30, 0, 0, zonePlus8)
	clk.t = nowPlus8
	if nowPlus8.Month() != time.March {
		t.Fatalf("test setup: service wall month = %s, want March", nowPlus8.Month())
	}
	if MonthOf(nowPlus8) != feb(2026) {
		t.Fatalf("test setup: service instant %v must still fall in february", nowPlus8.UTC())
	}

	// 事件写为 2026-03-01 00:15（UTC+8）：本地日期同样是三月一日，但实际
	// 发生时刻是 UTC 2026-02-28 16:15，必须归入二月，数量 15。
	ev := Event{
		AccountID: "a", EventID: "wall-march-plus8",
		At:       time.Date(2026, time.March, 1, 0, 15, 0, 0, zonePlus8),
		Quantity: 15,
	}
	if ev.At.Month() != time.March || MonthOf(ev.At) != feb(2026) {
		t.Fatalf("test setup: event wall month=%s instant=%v, want wall March / UTC February",
			ev.At.Month(), ev.At.UTC())
	}
	r, err := s.RecordEvent(ev)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("plus8 wall-march event: %+v %v", r, err)
	}

	// 此时查询必须给出二月预估，不能因为本地日期已进入三月而返回三月的
	// 零用量结果：累计 15、超额 5、超额费 500、税 150、预计应付 1650。
	e, err := s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("february estimate across local new-year-month: %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "a", Estimated: true, Period: feb(2026), Terms: terms,
		TotalUsage: 15, IncludedUnits: 10, OverageUnits: 5,
		MonthlyFee: 1000, OverageFee: 500, Tax: 150, EstimatedTotalDue: 1650,
	})

	// 同一服务瞬间换纯 UTC 表示，预估结果必须逐字段相同——当前账期只认
	// 实际瞬间，与时钟携带的偏移无关。
	clk.t = nowPlus8.UTC()
	eUTC, err := s.EstimateCurrentBill("a")
	if err != nil || eUTC != e {
		t.Fatalf("estimate differs by timezone representation of the same instant:\n got %+v\nwant %+v",
			eUTC, e)
	}
	clk.t = nowPlus8

	// 预估是纯读：三月用量为零，二月也没有保存正式账单（此刻二月尚未结束）。
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 0 {
		t.Fatalf("march usage = %d, want 0", u.Total)
	}
	if _, err := s.GetBill("a", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("estimate saved a february bill: %v", err)
	}

	// 情形二：到达 2026-03-01 00:00 UTC 的瞬间，无须先提交三月事件，也无须
	// 生成二月账单，直接查询即转为三月预估；二月尚无用量之外的三月数据：
	// 累计、超额与超额费均为零，月费仍完整计入，税 100、预计应付 1100。
	clk.t = utc(2026, 3, 1, 0, 0)
	e, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("march estimate at boundary: %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "a", Estimated: true, Period: mar(2026), Terms: terms,
		TotalUsage: 0, IncludedUnits: 10, OverageUnits: 0,
		MonthlyFee: 1000, OverageFee: 0, Tax: 100, EstimatedTotalDue: 1100,
	})
	// 二月的 15 单位保留在二月，不占用三月额度。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 15 {
		t.Fatalf("february usage after rollover = %d, want 15", u.Total)
	}
	// 二月账期虽已结束，但预估没有替它落账单。
	if _, err := s.GetBill("a", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("february bill exists without CreateBill: %v", err)
	}

	// 情形三：服务时刻来到三月一日 02:00 UTC，二月仍未出账。补报一条发生
	// 在 2026-02-28 23:30 UTC、数量 4 的晚到用量：归入二月，二月累计变为
	// 19；三月预估保持零用量的 1100 分，二月补报没有搬动任何三月数据。
	clk.t = utc(2026, 3, 1, 2, 0)
	r, err = s.RecordEvent(Event{
		AccountID: "a", EventID: "late-feb-4",
		At:       utc(2026, 2, 28, 23, 30),
		Quantity: 4,
	})
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("late february event: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 19 {
		t.Fatalf("february usage after late report = %d, want 19", u.Total)
	}
	e, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("march estimate after late february report: %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "a", Estimated: true, Period: mar(2026), Terms: terms,
		TotalUsage: 0, IncludedUnits: 10, OverageUnits: 0,
		MonthlyFee: 1000, OverageFee: 0, Tax: 100, EstimatedTotalDue: 1100,
	})

	// 情形四：再接收一条写为 2026-02-28 20:30（UTC-5）、数量 12 的事件。
	// 本地钟面还停在二月，但实际发生时刻是 UTC 2026-03-01 01:30，属于
	// 三月。三月预估应变为累计 12、超额 2、超额费 200、税 120、应付 1320；
	// 二月的 19 单位留在二月，不被占用。
	marWallEvent := Event{
		AccountID: "a", EventID: "wall-feb-minus5",
		At:       time.Date(2026, time.February, 28, 20, 30, 0, 0, zoneMinus5),
		Quantity: 12,
	}
	if marWallEvent.At.Month() != time.February || MonthOf(marWallEvent.At) != mar(2026) {
		t.Fatalf("test setup: wall month=%s instant=%v, want wall February / UTC March",
			marWallEvent.At.Month(), marWallEvent.At.UTC())
	}
	r, err = s.RecordEvent(marWallEvent)
	if err != nil || !r.Accepted || r.Period != mar(2026) {
		t.Fatalf("minus5 wall-february event: %+v %v", r, err)
	}
	e, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("march estimate with usage: %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "a", Estimated: true, Period: mar(2026), Terms: terms,
		TotalUsage: 12, IncludedUnits: 10, OverageUnits: 2,
		MonthlyFee: 1000, OverageFee: 200, Tax: 120, EstimatedTotalDue: 1320,
	})

	// 各月用量与上述归期一致：二月 19、三月 12。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 19 {
		t.Fatalf("february usage = %d, want 19", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 12 {
		t.Fatalf("march usage = %d, want 12", u.Total)
	}
	// 重复预估结果逐字段一致，查询本身不增加也不搬动用量。
	eRepeated, err := s.EstimateCurrentBill("a")
	if err != nil || eRepeated != e {
		t.Fatalf("repeated estimate differs:\n got %+v\nwant %+v", eRepeated, e)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 12 {
		t.Fatalf("march usage changed after repeated estimate: %d", u.Total)
	}

	// 全程没有保存任何正式账单：预估不落库，二月也不因跨月而被自动出账。
	if _, err := s.GetBill("a", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("february bill saved: %v", err)
	}
	if _, err := s.GetBill("a", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("march bill saved by estimate: %v", err)
	}
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("status = subscribed %v suspended %v, want subscribed and not suspended",
			st.Subscribed, st.Suspended)
	}
	if st.CurrentTerms != terms {
		t.Fatalf("current terms = %+v, want the same subscription snapshot %+v", st.CurrentTerms, terms)
	}
	if len(st.Bills) != 0 {
		t.Fatalf("estimate produced bills in status: %+v", st.Bills)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: feb(2026), Total: 19}) ||
		st.MonthlyUsage[1] != (Usage{Period: mar(2026), Total: 12}) {
		t.Fatalf("status monthly usage = %+v, want [feb 19, mar 12]", st.MonthlyUsage)
	}

	// 二月始终未出账，补报通道保持开放：再补一条晚到用量仍被接收并归入
	// 二月（累计 20），不搬动三月的 12 单位，三月预估也不变化。
	r, err = s.RecordEvent(Event{
		AccountID: "a", EventID: "late-feb-1",
		At:       utc(2026, 2, 28, 22, 0),
		Quantity: 1,
	})
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("february should still accept late reports: %+v %v", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 20 {
		t.Fatalf("february usage = %d, want 20", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 12 {
		t.Fatalf("march usage moved after february late report: %d", u.Total)
	}
	if eFinal, err := s.EstimateCurrentBill("a"); err != nil || eFinal != e {
		t.Fatalf("march estimate changed after february late report:\n got %+v\nwant %+v",
			eFinal, e)
	}
}

// TestEstimateCurrentBillServiceClockUTCBoundary 锁定“当前账期由服务时刻的
// UTC 自然月决定”的月初边界，精确到纳秒，并从正负两个偏移方向夹测：
// 本地钟面已显示三月但 UTC 仍在二月末时取二月；本地钟面仍显示二月末但
// UTC 已到三月月初时取三月。同一瞬间的纯 UTC 表示结果必须相同。
// 预估取的是当前 UTC 月的用量桶：二月已接收 15 单位，跨进三月后立即看到
// 三月自己的零用量，而不是把二月用量带进来。
func TestEstimateCurrentBillServiceClockUTCBoundary(t *testing.T) {
	s, clk := newTestService(utc(2026, 2, 20, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "b")
	mustSubscribe(t, s, "b", "p", utc(2026, 2, 1, 0, 0))
	terms := planTerms("p", 1000, 10, 100, 1000)

	// 二月先接收 15 单位：二月预估应为超额 5、应付 1650。
	if r, err := s.RecordEvent(Event{
		AccountID: "b", EventID: "feb-15", At: utc(2026, 2, 15, 0, 0), Quantity: 15,
	}); err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("seed february usage: %+v %v", r, err)
	}

	// +08:00 一侧：本地 03-01 07:59:59.999999999 == UTC 02-28 23:59:59.999999999。
	lastFebPlus8 := time.Date(2026, time.March, 1, 7, 59, 59, 999999999, zonePlus8)
	// 本地 03-01 08:00:00 == UTC 03-01 00:00:00，恰是三月月初。
	firstMarPlus8 := time.Date(2026, time.March, 1, 8, 0, 0, 0, zonePlus8)
	if lastFebPlus8.Month() != time.March || firstMarPlus8.Month() != time.March {
		t.Fatalf("test setup: both +08:00 wall clocks should show March 1")
	}
	if MonthOf(lastFebPlus8) != feb(2026) || MonthOf(firstMarPlus8) != mar(2026) {
		t.Fatalf("test setup: +08:00 boundary instants = %v / %v",
			lastFebPlus8.UTC(), firstMarPlus8.UTC())
	}

	// 本地已是三月一日凌晨、实际还差一纳秒才到 UTC 三月：预估仍是二月，
	// 取二月的 15 单位用量。
	clk.t = lastFebPlus8
	e, err := s.EstimateCurrentBill("b")
	if err != nil {
		t.Fatalf("estimate one ns before UTC march (+08:00 clock): %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "b", Estimated: true, Period: feb(2026), Terms: terms,
		TotalUsage: 15, IncludedUnits: 10, OverageUnits: 5,
		MonthlyFee: 1000, OverageFee: 500, Tax: 150, EstimatedTotalDue: 1650,
	})
	// 同一瞬间用纯 UTC 表示，账期不变。
	clk.t = lastFebPlus8.UTC()
	if e2, err := s.EstimateCurrentBill("b"); err != nil || e2 != e {
		t.Fatalf("estimate differs for same UTC instant: %+v vs %+v", e2, e)
	}

	// 本地 08:00、恰好 UTC 三月月初零点：直接查询即转为三月预估，
	// 三月用量桶为零，二月的 15 单位不带入三月。
	clk.t = firstMarPlus8
	e, err = s.EstimateCurrentBill("b")
	if err != nil {
		t.Fatalf("estimate at UTC march instant (+08:00 clock): %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "b", Estimated: true, Period: mar(2026), Terms: terms,
		TotalUsage: 0, IncludedUnits: 10, OverageUnits: 0,
		MonthlyFee: 1000, OverageFee: 0, Tax: 100, EstimatedTotalDue: 1100,
	})
	// 同一瞬间的纯 UTC 表示结果完全相同。
	clk.t = firstMarPlus8.UTC()
	if e2, err := s.EstimateCurrentBill("b"); err != nil || e2 != e {
		t.Fatalf("estimate differs for same UTC instant: %+v vs %+v", e2, e)
	}

	// -05:00 一侧，方向恰好相反：本地钟面仍显示 02-28 晚上，UTC 却已跨月。
	// 本地 02-28 18:59:59 == UTC 02-28 23:59:59（仍属二月）。
	lastFebMinus5 := time.Date(2026, time.February, 28, 18, 59, 59, 0, zoneMinus5)
	// 本地 02-28 19:00:00 == UTC 03-01 00:00:00（已是三月）。
	firstMarMinus5 := time.Date(2026, time.February, 28, 19, 0, 0, 0, zoneMinus5)
	if lastFebMinus5.Month() != time.February || firstMarMinus5.Month() != time.February {
		t.Fatalf("test setup: both -05:00 wall clocks should show February 28")
	}
	if MonthOf(lastFebMinus5) != feb(2026) || MonthOf(firstMarMinus5) != mar(2026) {
		t.Fatalf("test setup: -05:00 boundary instants = %v / %v",
			lastFebMinus5.UTC(), firstMarMinus5.UTC())
	}

	clk.t = lastFebMinus5
	e, err = s.EstimateCurrentBill("b")
	if err != nil {
		t.Fatalf("estimate one second before UTC march (-05:00 clock): %v", err)
	}
	if e.Period != feb(2026) || e.TotalUsage != 15 || e.EstimatedTotalDue != 1650 {
		t.Fatalf("february estimate from -05:00 clock: %+v", e)
	}
	// 本地仍是二月二十八日晚、实际瞬间已到 UTC 三月月初：预估必须取三月，
	// 看到的是三月自己的零用量，不能按本地月份继续返回二月的 15 单位。
	clk.t = firstMarMinus5
	e, err = s.EstimateCurrentBill("b")
	if err != nil {
		t.Fatalf("estimate at UTC march instant (-05:00 clock): %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "b", Estimated: true, Period: mar(2026), Terms: terms,
		TotalUsage: 0, IncludedUnits: 10, OverageUnits: 0,
		MonthlyFee: 1000, OverageFee: 0, Tax: 100, EstimatedTotalDue: 1100,
	})

	// 边界两侧的预估都没有落账单、没有搬动用量：二月仍 15、三月仍 0，
	// 且两个月都没有正式账单（二月虽已结束但未出账）。
	if u, _ := s.MonthlyUsage("b", feb(2026)); u.Total != 15 {
		t.Fatalf("february usage = %d, want 15", u.Total)
	}
	if u, _ := s.MonthlyUsage("b", mar(2026)); u.Total != 0 {
		t.Fatalf("march usage = %d, want 0", u.Total)
	}
	if _, err := s.GetBill("b", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("february bill saved by estimates: %v", err)
	}
	if _, err := s.GetBill("b", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("march bill saved by estimates: %v", err)
	}
}
