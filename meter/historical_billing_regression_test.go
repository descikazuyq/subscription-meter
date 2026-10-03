package meter

import (
	"errors"
	"testing"
	"time"
)

func may(y int) Month { return Month{Year: y, Month: time.May} }

// TestHistoricalBillsKeepLockedTermsAcrossChangeCancelResubscribe 是历史账单
// 计价的端到端回归：同一账户先安排换套餐（锁定目标套餐当时的完整条件）、
// 按月取消、空档一个月后按修改过的套餐重新开通，再先出新月账单、后补开旧月
// 账单。旧账期必须使用那段订阅当时锁定的条件快照，既不能被后来的订阅覆盖，
// 也不能被同一套餐的新价格覆盖。
//
// 时间线（全部 UTC）：
//
//	2026-01-01 开通套餐甲：月费 1000、额度 10、超额单价 100、税率 10%。
//	2026-01-15 安排 2 月起换套餐乙，安排时乙：月费 2000、额度 20、
//	           超额单价 200、税率 10%；安排锁定该完整快照。
//	之后       把乙的当前定义改为：月费 4000、额度 1、超额单价 500、税率 20%。
//	1 月用量 12、2 月用量 25，均在当时接收，两月均未出账。
//	2026-02-15 登记按月取消，2026-03-01 00:00 终止；3 月整月无订阅。
//	2026-04-01 按修改后的乙重新开通；4 月无用量。
//	2026-05-01 先出 4 月账单，再尝试 3 月，最后补开 2 月、1 月账单。
func TestHistoricalBillsKeepLockedTermsAcrossChangeCancelResubscribe(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))

	jia := Plan{ID: "jia", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 100, TaxRateBasisPoints: 1000}
	yiOld := Plan{ID: "yi", MonthlyFee: 2000, IncludedUnits: 20, OveragePrice: 200, TaxRateBasisPoints: 1000}
	yiNew := Plan{ID: "yi", MonthlyFee: 4000, IncludedUnits: 1, OveragePrice: 500, TaxRateBasisPoints: 2000}
	jiaTerms := termsOf(jia)
	yiOldTerms := termsOf(yiOld)
	yiNewTerms := termsOf(yiNew)

	mustPlan(t, s, jia)
	mustPlan(t, s, yiOld)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "jia", utc(2026, 1, 1, 0, 0))

	// 1 月中旬安排 2 月起改用乙：安排成功时锁定乙的完整条件与生效账期。
	clk.t = utc(2026, 1, 15, 12, 0)
	change := mustSchedule(t, s, "u", "yi")
	if !change.Created || change.Change.TargetPlanID != "yi" ||
		change.Change.EffectivePeriod != feb(2026) || change.Change.Terms != yiOldTerms {
		t.Fatalf("scheduled change = %+v, want locked old yi terms effective Feb", change)
	}

	// 安排成功后修改乙的当前定义：待生效安排仍持锁，不重新取价。
	if err := s.UpdatePlan(yiNew); err != nil {
		t.Fatalf("update yi: %v", err)
	}
	st, _ := s.Status("u")
	if st.PendingChange == nil || st.PendingChange.Terms != yiOldTerms {
		t.Fatalf("pending change re-priced by plan update: %+v", st.PendingChange)
	}
	// 同一目标套餐的重复安排返回原安排、不重新取价。
	replay, err := s.SchedulePlanChange("u", "yi")
	if err != nil || replay.Created || replay.Change.Terms != yiOldTerms {
		t.Fatalf("repeat schedule re-priced: %+v %v", replay, err)
	}

	// 1 月接收 12 单位。
	clk.t = utc(2026, 1, 20, 12, 0)
	if r, err := s.RecordEvent(Event{AccountID: "u", EventID: "usage-jan", At: utc(2026, 1, 20, 0, 0), Quantity: 12}); err != nil ||
		!r.Accepted || r.Period != jan(2026) {
		t.Fatalf("jan usage: %+v %v", r, err)
	}

	// 2/1 零点切换自动生效，无需任何操作；当前条件是锁定的旧乙，而非新定义。
	clk.t = utc(2026, 2, 10, 12, 0)
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms != yiOldTerms || st.PendingChange != nil {
		t.Fatalf("feb current terms = %+v pending = %+v, want locked old yi", st.CurrentTerms, st.PendingChange)
	}
	// 2 月接收 25 单位。
	if r, err := s.RecordEvent(Event{AccountID: "u", EventID: "usage-feb", At: utc(2026, 2, 10, 0, 0), Quantity: 25}); err != nil ||
		!r.Accepted || r.Period != feb(2026) {
		t.Fatalf("feb usage: %+v %v", r, err)
	}

	// 2 月中旬登记按月取消：终止时刻为 3/1 零点，2 月仍按完整月费与额度计费。
	clk.t = utc(2026, 2, 15, 12, 0)
	cancelRes := mustCancel(t, s, "u")
	if !cancelRes.Cancelled || !cancelRes.Cancellation.EndAt.Equal(utc(2026, 3, 1, 0, 0)) {
		t.Fatalf("cancel result = %+v, want end 2026-03-01", cancelRes)
	}

	// 3/1 终止：无有效订阅、无当前套餐；3 月整月空档，事件被拒。
	clk.t = utc(2026, 3, 10, 12, 0)
	st, _ = s.Status("u")
	if st.Subscribed || st.CurrentTerms != (PlanTerms{}) || st.ScheduledEnd != nil {
		t.Fatalf("status during gap: %+v", st)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "usage-mar", At: utc(2026, 3, 5, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("march event in gap: %v", err)
	}

	// 4/1 按修改后的乙重新开通：当前条件采用新定义；历史用量继续保留。
	clk.t = utc(2026, 4, 1, 0, 0)
	mustSubscribe(t, s, "u", "yi", utc(2026, 4, 1, 0, 0))
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms != yiNewTerms {
		t.Fatalf("current terms after resubscribe = %+v, want updated yi", st.CurrentTerms)
	}
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 12 {
		t.Fatalf("jan usage after resubscribe = %d, want 12", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 25 {
		t.Fatalf("feb usage after resubscribe = %d, want 25", u.Total)
	}

	// 5 月：先为重新开通后的 4 月出账——零用量，按新乙：
	// 月费 4000、税 20% = 800、应付 4800。
	clk.t = utc(2026, 5, 1, 0, 0)
	aprBill, err := s.CreateBill("u", apr(2026))
	if err != nil {
		t.Fatalf("create apr bill: %v", err)
	}
	wantHistoricalBill(t, aprBill, apr(2026), yiNewTerms,
		0, // total usage
		4000, 0, 800, 4800)

	// 重新开通不能把无订阅的 3 月变成可出账月份：出账仍返回账期未被订阅
	// 覆盖的现有错误，且查询不到该月账单。
	if _, err := s.CreateBill("u", mar(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("create march gap bill: %v", err)
	}
	if _, err := s.GetBill("u", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("march bill queryable after failed create: %v", err)
	}

	// 补开 2 月账单：必须保持安排换套餐时锁定的旧乙条件，
	// 月费 2000、额度 20、超额 (25-20)*200 = 1000、税 3000*10% = 300、应付 3300。
	febBill, err := s.CreateBill("u", feb(2026))
	if err != nil {
		t.Fatalf("backfill feb bill: %v", err)
	}
	wantHistoricalBill(t, febBill, feb(2026), yiOldTerms,
		25, // total usage
		2000, 1000, 300, 3300)

	// 再补开 1 月账单：仍按甲条件，
	// 月费 1000、额度 10、超额 (12-10)*100 = 200、税 1200*10% = 120、应付 1320。
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("backfill jan bill: %v", err)
	}
	wantHistoricalBill(t, janBill, jan(2026), jiaTerms,
		12, // total usage
		1000, 200, 120, 1320)

	// 已成功生成的历史账单，随后查询得到完全相同的计费条件、金额与用量；
	// 重复出账返回同一张账单。
	for _, original := range []Bill{aprBill, febBill, janBill} {
		got, err := s.GetBill("u", original.Period)
		if err != nil {
			t.Fatalf("get bill %s: %v", original.Period, err)
		}
		if got != original {
			t.Fatalf("bill %s changed between create and get:\ncreate=%+v\nget   =%+v", original.Period, original, got)
		}
		again, err := s.CreateBill("u", original.Period)
		if err != nil {
			t.Fatalf("recreate bill %s: %v", original.Period, err)
		}
		if again != original {
			t.Fatalf("bill %s not idempotent:\nfirst =%+v\nagain=%+v", original.Period, original, again)
		}
	}

	// 各月用量查询与账单展示的用量明细分别对应历史月份。
	for _, w := range []struct {
		period         Month
		total, overage int64
		included       int64
	}{
		{jan(2026), 12, 2, 10},
		{feb(2026), 25, 5, 20},
		{apr(2026), 0, 0, 1},
	} {
		if u, _ := s.MonthlyUsage("u", w.period); u.Total != w.total {
			t.Fatalf("usage %s = %d, want %d", w.period, u.Total, w.total)
		}
		b, _ := s.GetBill("u", w.period)
		if b.TotalUsage != w.total || b.OverageUnits != w.overage || b.IncludedUnits != w.included {
			t.Fatalf("bill %s usage detail = total %d over %d included %d, want total %d over %d included %d",
				w.period, b.TotalUsage, b.OverageUnits, b.IncludedUnits, w.total, w.overage, w.included)
		}
	}
	// 空档 3 月用量查询为零且没有账单。
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 0 {
		t.Fatalf("march usage = %d, want 0", u.Total)
	}

	// 补开旧账单后，账户当前套餐仍是重新开通时采用的新乙条件；
	// 账单按账期排列且只包含 1、2、4 月（3 月空档不在其中）。
	st, _ = s.Status("u")
	if !st.Subscribed || st.CurrentTerms != yiNewTerms {
		t.Fatalf("current terms after backfill = %+v, want updated yi", st.CurrentTerms)
	}
	wantSummaries := []struct {
		period Month
		due    int64
	}{
		{jan(2026), 1320},
		{feb(2026), 3300},
		{apr(2026), 4800},
	}
	if len(st.Bills) != len(wantSummaries) {
		t.Fatalf("status bills = %+v, want %d", st.Bills, len(wantSummaries))
	}
	for i, w := range wantSummaries {
		got := st.Bills[i]
		if got.Period != w.period || got.TotalDue != w.due || got.Paid != 0 ||
			got.Balance != w.due || got.Settled {
			t.Fatalf("status bills[%d] = %+v, want period %s due %d unpaid", i, got, w.period, w.due)
		}
		if !got.DueAt.Equal(w.period.End().AddDate(0, 0, 7)) {
			t.Fatalf("status bills[%d] due at = %v", i, got.DueAt)
		}
	}
}

// wantHistoricalBill 断言一张历史账单展示的套餐条件、包含额度、用量明细与
// 全部金额字段；尚未登记付款时已付为零、余额等于应付、未付清。
func wantHistoricalBill(t *testing.T, b Bill, period Month, wantTerms PlanTerms,
	totalUsage, monthlyFee, overageFee, tax, totalDue int64) {
	t.Helper()
	if b.AccountID != "u" || b.Period != period {
		t.Fatalf("bill identity = %s/%s, want u/%s", b.AccountID, b.Period, period)
	}
	// 套餐条件必须是该账期当时锁定的完整快照，而不只是总额碰巧正确。
	if b.Terms != wantTerms {
		t.Fatalf("bill %s terms = %+v, want %+v", period, b.Terms, wantTerms)
	}
	wantOverage := totalUsage - wantTerms.IncludedUnits
	if wantOverage < 0 {
		wantOverage = 0
	}
	if b.TotalUsage != totalUsage || b.IncludedUnits != wantTerms.IncludedUnits || b.OverageUnits != wantOverage {
		t.Fatalf("bill %s usage detail = total %d included %d overage %d, want total %d included %d overage %d",
			period, b.TotalUsage, b.IncludedUnits, b.OverageUnits,
			totalUsage, wantTerms.IncludedUnits, wantOverage)
	}
	if b.MonthlyFee != monthlyFee || b.OverageFee != overageFee || b.Tax != tax || b.TotalDue != totalDue {
		t.Fatalf("bill %s amounts = fee %d overageFee %d tax %d due %d, want %d %d %d %d",
			period, b.MonthlyFee, b.OverageFee, b.Tax, b.TotalDue,
			monthlyFee, overageFee, tax, totalDue)
	}
	// 尚未登记付款：已付为零、余额等于应付。
	if b.Paid != 0 || b.Balance != totalDue || b.Settled {
		t.Fatalf("bill %s payment state = paid %d balance %d settled %v, want 0/%d/false",
			period, b.Paid, b.Balance, b.Settled, totalDue)
	}
	if !b.DueAt.Equal(period.End().AddDate(0, 0, 7)) {
		t.Fatalf("bill %s due at = %v, want %s+7d", period, b.DueAt, period.End())
	}
}
