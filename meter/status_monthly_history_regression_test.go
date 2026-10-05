package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为 Status 的月度历史列表补充回归保障：用量上报与账单生成都可能不按
// 月份顺序发生（先报较晚月份再补报较早月份；先出较晚月份账单再补开旧账单），
// 但 Status 返回的 MonthlyUsage 与 Bills 必须只展示保存下来的记录，并严格按
// 年月先后排列，顺序不能跟着上报/出账顺序走，跨年与月份不连续时也不例外。
//
// 夹具账户（订阅自 2025-12-01 起持续有效，查询时下列月份均已结束）：
//
//	2025-12 已接收总计 7 单位用量，已出账；
//	2026-01 没有接收过任何事件，但已出账（保留零用量记录）；
//	2026-02 既无事件也无账单（不得为补齐月份而出现）；
//	2026-03 已接收总计 11 单位用量，尚未出账（只在用量列表中出现）；
//	2026-04 只接收过一条数量为零的事件（记录保留，用量为零）。
//
// 事件按 4 月 → 3 月 → 12 月的顺序补报，账单按先 1 月后 12 月的顺序补开，
// 且所有用量都在所属月份出账前被成功接收。

func dec(y int) Month { return Month{Year: y, Month: time.December} }

// statusHistoryPlan 是夹具使用的套餐：月费 1000、额度 5、超额单价 100、
// 税率 10%。12 月用量 7：超额 2、超额费 200、税 120、应付 1320；
// 1 月用量 0：无超额、税 100、应付 1100。两月应付不同，可防止列表
// 错序时把金额安到别的月份。
var statusHistoryPlan = Plan{
	ID:                 "hist-plan",
	MonthlyFee:         1000,
	IncludedUnits:      5,
	OveragePrice:       100,
	TaxRateBasisPoints: 1000, // 10%
}

// setupStatusHistoryFixture 按“乱序上报、乱序出账”建立上述账户历史，
// 时钟最终停在 2026-05-02（全部相关月份均已结束）。
func setupStatusHistoryFixture(t *testing.T) *Service {
	t.Helper()
	s, clk := newTestService(utc(2025, 12, 1, 0, 0))
	mustPlan(t, s, statusHistoryPlan)
	mustAccount(t, s, "u")
	// 订阅自 2025-12-01 起覆盖所有相关月份，之后不取消、不换套餐。
	mustSubscribe(t, s, "u", statusHistoryPlan.ID, utc(2025, 12, 1, 0, 0))

	// 用量上报故意不按月份顺序：先报最晚的 4 月，再补 3 月，最后补上年 12 月。
	// 4 月只有一条数量为零的事件：合法且必须让该月留存在用量记录中。
	clk.t = utc(2026, 4, 10, 12, 0)
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "apr-zero", At: utc(2026, 4, 5, 8, 0), Quantity: 0})

	// 3 月总量 11，拆成两条事件接收，检验累计值而不仅是单条数量。
	clk.t = utc(2026, 3, 25, 12, 0)
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "mar-6", At: utc(2026, 3, 10, 8, 0), Quantity: 6})
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "mar-5", At: utc(2026, 3, 20, 8, 0), Quantity: 5})

	// 最后补报上一年 12 月的 7 单位：两条事件合计，接收时刻仍早于 12 月出账。
	clk.t = utc(2025, 12, 25, 12, 0)
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "dec-3", At: utc(2025, 12, 10, 8, 0), Quantity: 3})
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "dec-4", At: utc(2025, 12, 22, 8, 0), Quantity: 4})

	// 时钟进入 5 月：12 月至 4 月均已结束。出账同样不按月份顺序——
	// 先出较晚的 1 月账单（当月无任何事件），再补开上一年 12 月账单。
	clk.t = utc(2026, 5, 2, 10, 0)
	janBill := mustBill(t, s, "u", jan(2026))
	if janBill.TotalUsage != 0 || janBill.TotalDue != 1100 {
		t.Fatalf("january bill = usage %d due %d, want 0 / 1100", janBill.TotalUsage, janBill.TotalDue)
	}
	decBill := mustBill(t, s, "u", dec(2025))
	if decBill.TotalUsage != 7 || decBill.TotalDue != 1320 {
		t.Fatalf("december bill = usage %d due %d, want 7 / 1320", decBill.TotalUsage, decBill.TotalDue)
	}

	// 两张账单登记不同的付款状态，防止列表错序时已付/余额/付清状态串月：
	// 12 月当场付清；1 月只付 400，余额 700 未付清（其截止时刻 2026-02-08
	// 已过，账户当前处于欠费停用，但不影响历史列表的展示）。
	if pr, err := s.RecordPayment("u", "pay-dec", dec(2025), 1320); err != nil ||
		!pr.Registered || pr.BillBalance != 0 || !pr.Settled {
		t.Fatalf("pay december bill = %+v %v, want fully settled", pr, err)
	}
	if pr, err := s.RecordPayment("u", "pay-jan-partial", jan(2026), 400); err != nil ||
		!pr.Registered || pr.BillBalance != 700 || pr.Settled {
		t.Fatalf("partial pay january bill = %+v %v, want balance 700 not settled", pr, err)
	}
	return s
}

// TestStatusMonthlyHistoryOrderedAcrossYearsAndGaps 验证乱序上报、乱序出账后，
// Status 的月度历史仍按账期先后展示保存下来的记录：
//
//	MonthlyUsage 依次为 2025-12=7、2026-01=0、2026-03=11、2026-04=0，
//	每月最多一次；2 月既无事件也无账单，不为补齐月份出现；
//	Bills 只含 2025-12 与 2026-01 两张实际账单，同样按年月先后排列
//	（下一年的 1 月不得排到上一年的 12 月之前），不为 3、4 月生成摘要。
//
// 列表用量与各月 MonthlyUsage 单月查询一致；账单摘要的应付、已付、余额、
// 付清状态与截止时刻与 GetBill 得到的当前账单完全一致，数值不得串月。
func TestStatusMonthlyHistoryOrderedAcrossYearsAndGaps(t *testing.T) {
	s := setupStatusHistoryFixture(t)

	// 3 月、4 月从未出账：Status 查询前后都查不到账单，查询本身不补出账单。
	for _, p := range []Month{feb(2026), mar(2026), apr(2026)} {
		if _, err := s.GetBill("u", p); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("bill %s before status = %v, want ErrBillNotFound", p, err)
		}
	}

	st, err := s.Status("u")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// 订阅从未取消，5 月仍生效；1 月账单已过截止时刻且未付清，账户停用。
	if !st.Subscribed {
		t.Fatalf("Subscribed = false, status = %+v", st)
	}
	if !st.Suspended {
		t.Fatalf("Suspended = false, want suspended on overdue january balance: %+v", st)
	}

	wantUsage := []Usage{
		{Period: dec(2025), Total: 7},
		{Period: jan(2026), Total: 0},
		{Period: mar(2026), Total: 11},
		{Period: apr(2026), Total: 0},
	}
	if len(st.MonthlyUsage) != len(wantUsage) {
		t.Fatalf("monthly usage = %+v, want %d entries", st.MonthlyUsage, len(wantUsage))
	}
	for i, want := range wantUsage {
		if got := st.MonthlyUsage[i]; got != want {
			t.Fatalf("monthly usage[%d] = %+v, want %+v (full list %+v)", i, got, want, st.MonthlyUsage)
		}
	}
	// 显式复验关键不变量：2 月不在列表中；每个账期只出现一次。
	seen := make(map[Month]int)
	for _, u := range st.MonthlyUsage {
		seen[u.Period]++
		if u.Period == feb(2026) {
			t.Fatalf("february fabricated into monthly usage: %+v", st.MonthlyUsage)
		}
	}
	for _, u := range st.MonthlyUsage {
		if seen[u.Period] != 1 {
			t.Fatalf("period %s appears %d times in monthly usage", u.Period, seen[u.Period])
		}
	}

	// 列表中的用量必须与对应月份的单月用量查询一致，包括没有任何记录的 2 月。
	for _, w := range []struct {
		period Month
		total  int64
	}{
		{dec(2025), 7},
		{jan(2026), 0},
		{feb(2026), 0},
		{mar(2026), 11},
		{apr(2026), 0},
	} {
		if u, err := s.MonthlyUsage("u", w.period); err != nil || u.Total != w.total {
			t.Fatalf("single-month usage %s = %+v %v, want total %d", w.period, u, err, w.total)
		}
	}

	// 对一个尚无记录月份做单月用量查询只返回零，不得凭空把该月写进账户历史。
	if u, err := s.MonthlyUsage("u", may(2026)); err != nil || u.Total != 0 {
		t.Fatalf("may usage = %+v %v, want zero", u, err)
	}

	wantBillPeriods := []Month{dec(2025), jan(2026)}
	if len(st.Bills) != len(wantBillPeriods) {
		t.Fatalf("bills = %+v, want only %v", st.Bills, wantBillPeriods)
	}
	for i, p := range wantBillPeriods {
		if st.Bills[i].Period != p {
			t.Fatalf("bills[%d] period = %s, want %s (full list %+v) — " +
				"next-year january must follow previous-year december",
				i, st.Bills[i].Period, p, st.Bills)
		}
	}
	// 每张摘要的应付、已付、余额、付清状态与截止时刻与当前账单逐字段一致。
	for _, p := range wantBillPeriods {
		b, err := s.GetBill("u", p)
		if err != nil {
			t.Fatalf("get bill %s: %v", p, err)
		}
		sum := statusBillSummary(t, st, p)
		if sum.TotalDue != b.TotalDue || sum.Paid != b.Paid || sum.Balance != b.Balance ||
			sum.Settled != b.Settled || !sum.DueAt.Equal(b.DueAt) {
			t.Fatalf("summary %s = %+v, want due %d paid %d balance %d settled %v dueAt %v",
				p, sum, b.TotalDue, b.Paid, b.Balance, b.Settled, b.DueAt)
		}
		if !sum.DueAt.Equal(p.End().AddDate(0, 0, 7)) {
			t.Fatalf("summary %s due at = %v, want period end + 7 days", p, sum.DueAt)
		}
	}
	// 固定两张摘要的具体数值，进一步防止金额串月。
	decSum := statusBillSummary(t, st, dec(2025))
	if decSum.TotalDue != 1320 || decSum.Paid != 1320 || decSum.Balance != 0 || !decSum.Settled ||
		!decSum.DueAt.Equal(utc(2026, 1, 8, 0, 0)) {
		t.Fatalf("december summary = %+v, want 1320 fully settled due 2026-01-08", decSum)
	}
	janSum := statusBillSummary(t, st, jan(2026))
	if janSum.TotalDue != 1100 || janSum.Paid != 400 || janSum.Balance != 700 || janSum.Settled ||
		!janSum.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("january summary = %+v, want due 1100 paid 400 balance 700 not settled due 2026-02-08", janSum)
	}

	// 查询不补报用量、不生成账单：3、4 月仍无账单，2、5 月仍无任何记录；
	// 再次查询得到完全相同的两份列表，顺序稳定。
	for _, p := range []Month{feb(2026), mar(2026), apr(2026), may(2026)} {
		if _, err := s.GetBill("u", p); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("bill %s after status = %v, want ErrBillNotFound", p, err)
		}
	}
	again, err := s.Status("u")
	if err != nil {
		t.Fatalf("second status: %v", err)
	}
	if len(again.MonthlyUsage) != len(wantUsage) || len(again.Bills) != len(wantBillPeriods) {
		t.Fatalf("second status lists changed: %+v / %+v", again.MonthlyUsage, again.Bills)
	}
	for i, want := range wantUsage {
		if again.MonthlyUsage[i] != want {
			t.Fatalf("second status usage[%d] = %+v, want %+v", i, again.MonthlyUsage[i], want)
		}
	}
	for i, p := range wantBillPeriods {
		if again.Bills[i].Period != p {
			t.Fatalf("second status bills[%d] = %s, want %s", i, again.Bills[i].Period, p)
		}
	}
}

// TestStatusEmptyAccountListsStayEmptyAfterZeroUsageQuery 验证没有任何用量或
// 账单的已有账户两份列表都为空；先单独查询一个尚无记录月份得到零用量，再
// 查询账户状态也不会凭空多出该月份。
func TestStatusEmptyAccountListsStayEmptyAfterZeroUsageQuery(t *testing.T) {
	s, _ := newTestService(utc(2026, 5, 2, 10, 0))
	mustPlan(t, s, statusHistoryPlan)
	mustAccount(t, s, "empty")
	mustSubscribe(t, s, "empty", statusHistoryPlan.ID, utc(2026, 1, 1, 0, 0))

	st, err := s.Status("empty")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.MonthlyUsage) != 0 || len(st.Bills) != 0 {
		t.Fatalf("empty account lists = %+v / %+v, want both empty", st.MonthlyUsage, st.Bills)
	}

	// 单月查询一个尚无记录、且已结束的月份：零用量，但不落任何记录。
	if u, err := s.MonthlyUsage("empty", mar(2026)); err != nil || u.Total != 0 {
		t.Fatalf("march usage on empty account = %+v %v, want zero", u, err)
	}
	st, err = s.Status("empty")
	if err != nil {
		t.Fatalf("status after zero usage query: %v", err)
	}
	if len(st.MonthlyUsage) != 0 || len(st.Bills) != 0 {
		t.Fatalf("zero usage query fabricated history: %+v / %+v", st.MonthlyUsage, st.Bills)
	}
	// 即使该月已被订阅覆盖且已结束，没有实际账单就查不到，状态查询也不代出。
	if _, err := s.GetBill("empty", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("march bill on empty account = %v, want ErrBillNotFound", err)
	}
}

// TestStatusUnknownAccountNotFound 验证查询不存在的账户仍返回账户不存在错误，
// 状态查询与单月用量查询皆然。
func TestStatusUnknownAccountNotFound(t *testing.T) {
	s, _ := newTestService(utc(2026, 5, 2, 10, 0))
	if _, err := s.Status("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("status on unknown account = %v, want ErrAccountNotFound", err)
	}
	if _, err := s.MonthlyUsage("ghost", mar(2026)); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("monthly usage on unknown account = %v, want ErrAccountNotFound", err)
	}
}
