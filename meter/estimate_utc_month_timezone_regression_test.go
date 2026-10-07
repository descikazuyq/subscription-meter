package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为当前账期预计费用查询（EstimateCurrentBill）在“服务时钟与事件时间都
// 带时区偏移”时的归期提供回归保障：
//
//	“当前账期”只由服务时刻的实际瞬间所在的 UTC 自然月决定，与服务时钟字面上
//	写出的本地年月无关；每条用量归属哪个月份只由其发生时刻的实际瞬间决定，
//	既不能按本地钟面月份归账，也不能按提交时刻（服务时刻）归账，更不能把
//	不同月份的用量混在一起计费。到达 UTC 月初零点的瞬间，直接查询即翻到
//	新月份预估，无须先提交新月事件或为旧月出账；旧月未出账时跨月补报仍计入
//	旧月，不占用新月额度。
//
// 时间线（同一账户、同一份套餐：月费 1000 分、含 10 单位、超额 100 分/单位、
// 税率 10%；2026-02-01 00:00 UTC 起持续订阅、无欠费账单）：
//
//	服务时刻 2026-03-01T00:30:00+08:00（= 02-28 16:30 UTC，实际仍在二月），
//	已接收一条 2026-03-01T00:15:00+08:00（= 02-28 16:15 UTC）、数量 15 的
//	用量：预估必须仍是二月——累计 15、超额 5、超额费 500、税 150、应付 1650，
//	不能因本地日期已进三月而返回三月的零用量结果。
//	到达 2026-03-01 00:00 UTC 的瞬间直接查询即转为三月预估：零用量、零超额，
//	完整月费照计，税 100、应付 1100；二月的 15 留在二月。
//	服务时刻来到 03-01 02:00 UTC、二月仍未出账：补报 02-28 23:30 UTC 数量 4，
//	二月累计变为 19，三月预估保持 1100；再接收 2026-02-28T20:30:00-05:00
//	（= 03-01 01:30 UTC，本地钟面在二月、实际已在三月）数量 12，三月预估变为
//	累计 12、超额 2、超额费 200、税 120、应付 1320。
//
// 全程查询都明确标为预估、采用同一份订阅套餐快照、不保存正式账单、不替尚未
// 出账的二月关闭补报；查询预估本身不增加也不搬动任何用量。

// TestEstimateCurrentBillUsesUTCClockAndEventInstants 端到端回归上述主线。
func TestEstimateCurrentBillUsesUTCClockAndEventInstants(t *testing.T) {
	// 服务时钟起点早于开通；账户自 2026-02-01 00:00 UTC 起持续订阅。
	s, clk := newTestService(utc(2026, 1, 31, 0, 0))
	mustPlan(t, s, planDef("p", 1000, 10, 100, 1000))
	mustAccount(t, s, "a")
	mustSubscribe(t, s, "a", "p", utc(2026, 2, 1, 0, 0))
	terms := planTerms("p", 1000, 10, 100, 1000)

	// 阶段一：服务时刻本地钟面 03-01 00:30（UTC+8），实际仍是 02-28 16:30 UTC。
	nowStillFeb := time.Date(2026, time.March, 1, 0, 30, 0, 0, zonePlus8)
	// 锁定测试前提：本地月份已是三月，UTC 月份却仍是二月。
	if nowStillFeb.Month() != time.March {
		t.Fatalf("test setup: service wall month = %s, want March", nowStillFeb.Month())
	}
	if !nowStillFeb.UTC().Equal(utc(2026, 2, 28, 16, 30)) || MonthOf(nowStillFeb) != feb(2026) {
		t.Fatalf("test setup: service instant = %v, want 2026-02-28 16:30 UTC (February)",
			nowStillFeb.UTC())
	}
	clk.t = nowStillFeb
	if st, _ := s.Status("a"); !st.Subscribed || st.Suspended {
		t.Fatalf("status before events = %+v, want subscribed and not suspended", st)
	}

	// 一条本地钟面在三月的事件：03-01 00:15 +08 == 02-28 16:15 UTC，归入二月。
	event15 := Event{
		AccountID: "a", EventID: "e-plus8-wall-march",
		At:       time.Date(2026, time.March, 1, 0, 15, 0, 0, zonePlus8),
		Quantity: 15,
	}
	if event15.At.Month() != time.March {
		t.Fatalf("test setup: event wall month = %s, want March", event15.At.Month())
	}
	if !event15.At.UTC().Equal(utc(2026, 2, 28, 16, 15)) {
		t.Fatalf("test setup: event instant = %v, want 2026-02-28 16:15 UTC", event15.At.UTC())
	}
	r, err := s.RecordEvent(event15)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("plus8 wall-march event: %+v %v, want accepted into 2026-02", r, err)
	}

	// 预估必须给二月：累计 15、超额 5、超额费 500、税 150、预计应付 1650，
	// 绝不能因为本地日期已进入三月而返回三月的零用量结果。
	est, err := s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("february estimate with offset clock: %v", err)
	}
	wantEstimate(t, est, EstimatedBill{
		AccountID: "a", Estimated: true, Period: feb(2026), Terms: terms,
		TotalUsage: 15, IncludedUnits: 10, OverageUnits: 5,
		MonthlyFee: 1000, OverageFee: 500, Tax: 150, EstimatedTotalDue: 1650,
	})

	// 各月用量与归期一致：二月 15，三月为零；预估不增加也不搬动用量。
	if u, err := s.MonthlyUsage("a", feb(2026)); err != nil || u.Total != 15 {
		t.Fatalf("feb usage = %+v %v, want 15", u, err)
	}
	if u, err := s.MonthlyUsage("a", mar(2026)); err != nil || u.Total != 0 {
		t.Fatalf("mar usage = %+v %v, want 0", u, err)
	}
	// 预估不落正式账单：二月尚未结束本来就不能出账，预估之后仍无账单。
	if _, err := s.GetBill("a", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("estimate saved a february bill: %v", err)
	}

	// 月初边界的前一纳秒：本地/UTC 任何表示下仍属二月，预估不变。
	clk.t = time.Date(2026, time.February, 28, 23, 59, 59, 999999999, time.UTC)
	est, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("estimate one nanosecond before march: %v", err)
	}
	wantEstimate(t, est, EstimatedBill{
		AccountID: "a", Estimated: true, Period: feb(2026), Terms: terms,
		TotalUsage: 15, IncludedUnits: 10, OverageUnits: 5,
		MonthlyFee: 1000, OverageFee: 500, Tax: 150, EstimatedTotalDue: 1650,
	})

	// 阶段二：恰在 2026-03-01 00:00 UTC，无须先提交三月事件或生成二月账单，
	// 直接查询就应转为三月预估：零用量、零超额，完整月费照计，税 100、应付 1100。
	clk.t = utc(2026, 3, 1, 0, 0)
	est, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("march estimate at exact boundary: %v", err)
	}
	wantEstimate(t, est, EstimatedBill{
		AccountID: "a", Estimated: true, Period: mar(2026), Terms: terms,
		TotalUsage: 0, IncludedUnits: 10, OverageUnits: 0,
		MonthlyFee: 1000, OverageFee: 0, Tax: 100, EstimatedTotalDue: 1100,
	})

	// 二月的 15 单位保留在二月，不占用三月额度；翻月查询没有替二月出账。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 15 {
		t.Fatalf("feb usage after rollover = %d, want 15", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 0 {
		t.Fatalf("mar usage after rollover = %d, want 0", u.Total)
	}
	if _, err := s.GetBill("a", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("rollover estimate billed february: %v", err)
	}
	if _, err := s.GetBill("a", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("estimate saved a march bill: %v", err)
	}

	// 阶段三：服务时刻来到 03-01 02:00 UTC，二月仍未出账。
	clk.t = utc(2026, 3, 1, 2, 0)

	// 补报一条发生在 02-28 23:30 UTC、数量 4 的用量：跨月补报仍计入尚未
	// 出账的二月，二月累计 15+4 = 19；三月预估保持 1100 分。
	lateFeb := Event{
		AccountID: "a", EventID: "e-late-feb",
		At:       utc(2026, 2, 28, 23, 30),
		Quantity: 4,
	}
	if r, err := s.RecordEvent(lateFeb); err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("late february event: %+v %v, want accepted into 2026-02", r, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 19 {
		t.Fatalf("feb usage after late report = %d, want 19", u.Total)
	}
	est, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("march estimate after late february report: %v", err)
	}
	wantEstimate(t, est, EstimatedBill{
		AccountID: "a", Estimated: true, Period: mar(2026), Terms: terms,
		TotalUsage: 0, IncludedUnits: 10, OverageUnits: 0,
		MonthlyFee: 1000, OverageFee: 0, Tax: 100, EstimatedTotalDue: 1100,
	})

	// 再接收一条本地钟面在二月的事件：02-28 20:30 -05 == 03-01 01:30 UTC，
	// 实际发生时刻属于三月，必须归入三月而不是二月。
	wallFebEvent := Event{
		AccountID: "a", EventID: "e-minus5-wall-february",
		At:       time.Date(2026, time.February, 28, 20, 30, 0, 0, zoneMinus5),
		Quantity: 12,
	}
	// 锁定测试前提：提交时间的本地月份是二月，实际瞬间却在三月。
	if wallFebEvent.At.Month() != time.February {
		t.Fatalf("test setup: event wall month = %s, want February", wallFebEvent.At.Month())
	}
	if !wallFebEvent.At.UTC().Equal(utc(2026, 3, 1, 1, 30)) || MonthOf(wallFebEvent.At) != mar(2026) {
		t.Fatalf("test setup: event instant = %v, want 2026-03-01 01:30 UTC (March)",
			wallFebEvent.At.UTC())
	}
	if r, err := s.RecordEvent(wallFebEvent); err != nil || !r.Accepted || r.Period != mar(2026) {
		t.Fatalf("minus5 wall-february event: %+v %v, want accepted into 2026-03", r, err)
	}

	// 三月预估：累计 12、超额 2、超额费 200、税 120、预计应付 1320。
	est, err = s.EstimateCurrentBill("a")
	if err != nil {
		t.Fatalf("march estimate with usage: %v", err)
	}
	wantEstimate(t, est, EstimatedBill{
		AccountID: "a", Estimated: true, Period: mar(2026), Terms: terms,
		TotalUsage: 12, IncludedUnits: 10, OverageUnits: 2,
		MonthlyFee: 1000, OverageFee: 200, Tax: 120, EstimatedTotalDue: 1320,
	})

	// 二月累计仍是 19，不被这条“本地二月、实际三月”的事件搬动或占用。
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 19 {
		t.Fatalf("feb usage changed = %d, want 19", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 12 {
		t.Fatalf("mar usage = %d, want 12", u.Total)
	}

	// 重复预估结果完全一致：查询不增加、不搬动任何用量，也不落任何账单。
	again, err := s.EstimateCurrentBill("a")
	if err != nil || again != est {
		t.Fatalf("repeated estimate differs: %+v %+v %v", again, est, err)
	}
	if u, _ := s.MonthlyUsage("a", feb(2026)); u.Total != 19 {
		t.Fatalf("feb usage after repeated estimate = %d, want 19", u.Total)
	}
	if u, _ := s.MonthlyUsage("a", mar(2026)); u.Total != 12 {
		t.Fatalf("mar usage after repeated estimate = %d, want 12", u.Total)
	}

	// 全程没有正式账单：预估不落库；二月仍未出账（此前的跨月补报已被接收，
	// 预估没有替它关闭补报）；三月账期未结束不能正式出账。
	if _, err := s.GetBill("a", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("february bill exists at end: %v", err)
	}
	if _, err := s.GetBill("a", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("march bill exists at end: %v", err)
	}
	if _, err := s.CreateBill("a", mar(2026)); !errors.Is(err, ErrBillMonthNotEnded) {
		t.Fatalf("march closed by estimates: %v", err)
	}

	// 状态口径与预估一致：订阅持续有效、无欠费停用，用量只含二月 19 与三月 12，
	// 账单列表为空。
	st, err := s.Status("a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Subscribed || st.Suspended {
		t.Fatalf("final status = %+v, want subscribed and not suspended", st)
	}
	if st.CurrentTerms != terms {
		t.Fatalf("current terms = %+v, want same subscription snapshot %+v", st.CurrentTerms, terms)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: feb(2026), Total: 19}) ||
		st.MonthlyUsage[1] != (Usage{Period: mar(2026), Total: 12}) {
		t.Fatalf("final monthly usage = %+v, want [2026-02=19 2026-03=12]", st.MonthlyUsage)
	}
	if len(st.Bills) != 0 {
		t.Fatalf("final bills = %+v, want none", st.Bills)
	}
}
