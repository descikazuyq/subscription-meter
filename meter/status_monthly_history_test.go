package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为 Status 的月度历史列表补充回归保障：MonthlyUsage 与 Bills 都按账期
// （年月）先后展示保存下来的记录，与用量上报、账单生成的先后顺序无关。覆盖
// 跨年（2025-12 与 2026-01）且月份不连续（二月完全无记录）的情形：
//   - 先上报较晚月份的用量再补报较早月份、先出较晚月份账单再补旧账单，
//     两份列表仍按账期排列，下一年的一月不得排到上一年的十二月之前；
//   - 同月既有用量又有账单只在 MonthlyUsage 出现一次；没有事件但已出账的
//     月份保留零用量记录；只接收过零数量事件的月份同样保留记录；
//     完全无记录的月份不为补齐而凭空出现；
//   - 列表中的用量与单月 MonthlyUsage 查询一致，账单摘要的应付、已付、
//     余额、付清状态与截止时刻与 GetBill 的当前账单一致；
//   - 无任何记录的账户两份列表都为空；单月零用量查询不会让该月凭空进入
//     状态列表；不存在的账户仍返回 ErrAccountNotFound；状态查询本身
//     不补报用量、不生成账单。

func dec(y int) Month { return Month{Year: y, Month: time.December} }

// wantStatusUsage 断言状态中的用量列表与期望的账期序列逐项一致
// （账期先后与每月总量都相同，且每个月只出现一次）。
func wantStatusUsage(t *testing.T, st AccountStatus, want []Usage) {
	t.Helper()
	if len(st.MonthlyUsage) != len(want) {
		t.Fatalf("monthly usage = %+v, want %+v", st.MonthlyUsage, want)
	}
	for i, w := range want {
		if st.MonthlyUsage[i] != w {
			t.Fatalf("monthly usage[%d] = %+v, want %+v (full list %+v)",
				i, st.MonthlyUsage[i], w, st.MonthlyUsage)
		}
	}
}

// wantUsageOrder 是跨年不连续夹具期望的用量列表：
// 十二月 7、一月 0（无事件但已出账）、三月 11（未出账）、四月 0（零数量事件）；
// 完全无记录的二月不出现。
func wantUsageOrder() []Usage {
	return []Usage{
		{Period: dec(2025), Total: 7},
		{Period: jan(2026), Total: 0},
		{Period: mar(2026), Total: 11},
		{Period: apr(2026), Total: 0},
	}
}

// setupCrossYearHistoryFixture 建立跨年且月份不连续的账户历史，结束时时钟
// 停在 2026-05-10，相关月份（2025-12 至 2026-04）均已结束，订阅自
// 2025-12-01 起持续有效、覆盖这些月份。
//
//	套餐 a：月费 1000、额度 5、超额单价 100、税率 0。
//	2025-12：接收 4+3=7 单位，已出账（应付 1000+2*100=1200），已付 400。
//	2026-01：无事件，已出账（应付 1000），已付清。
//	2026-02：既无事件也无账单。
//	2026-03：接收 5+6=11 单位，尚未出账。
//	2026-04：只接收过一条数量为零的事件。
//
// 用量上报顺序为三月→十二月→四月→三月→十二月，出账顺序为一月→十二月，
// 均不按月份先后；用量都在所属月份出账前被成功接收。
func setupCrossYearHistoryFixture(t *testing.T) *Service {
	t.Helper()
	s, _ := newTestService(utc(2026, 5, 10, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 5, 100, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2025, 12, 1, 0, 0))

	// 用量上报不按月份顺序：先较晚的三月，再补较早的十二月。
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-mar-2", At: utc(2026, 3, 25, 0, 0), Quantity: 5})
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-dec-1", At: utc(2025, 12, 10, 0, 0), Quantity: 4})
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-apr-zero", At: utc(2026, 4, 15, 0, 0), Quantity: 0})
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-mar-1", At: utc(2026, 3, 5, 0, 0), Quantity: 6})
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-dec-2", At: utc(2025, 12, 20, 0, 0), Quantity: 3})

	// 出账同样不按月份顺序：先出 2026 年一月，再补 2025 年十二月。
	janBill := mustBill(t, s, "u", jan(2026))
	if janBill.TotalDue != 1000 {
		t.Fatalf("jan bill total = %d, want 1000", janBill.TotalDue)
	}
	decBill := mustBill(t, s, "u", dec(2025))
	if decBill.TotalDue != 1200 {
		t.Fatalf("dec bill total = %d, want 1200", decBill.TotalDue)
	}

	// 十二月账单部分付款 400（余额 800、未付清）；一月账单全额付清。
	if r, err := s.RecordPayment("u", "pay-dec", dec(2025), 400); err != nil ||
		r.BillBalance != 800 || r.Settled {
		t.Fatalf("dec payment = %+v %v, want balance 800 not settled", r, err)
	}
	if r, err := s.RecordPayment("u", "pay-jan", jan(2026), 1000); err != nil ||
		r.BillBalance != 0 || !r.Settled {
		t.Fatalf("jan payment = %+v %v, want settled", r, err)
	}
	return s
}

// TestStatusMonthlyHistoryOrderedByPeriodNotByArrival 验证月度历史列表按账期
// 先后排列，与上报、出账顺序无关：MonthlyUsage 依次为十二月 7、一月 0、
// 三月 11、四月 0，每个月只出现一次；Bills 只含十二月与一月两张实际账单，
// 上一年的十二月排在前，不为三月、四月生成摘要。
func TestStatusMonthlyHistoryOrderedByPeriodNotByArrival(t *testing.T) {
	s := setupCrossYearHistoryFixture(t)

	st, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Subscribed {
		t.Fatalf("Subscribed = false, status = %+v", st)
	}
	wantStatusUsage(t, st, wantUsageOrder())

	// 账单列表只包含十二月与一月，按年月先后排列：
	// 下一年的二月不得排到上一年的十二月之前，也不为三月、四月生成摘要。
	if len(st.Bills) != 2 ||
		st.Bills[0].Period != dec(2025) || st.Bills[1].Period != jan(2026) {
		t.Fatalf("bills = %+v, want dec 2025 then jan 2026 only", st.Bills)
	}

	// 列表中的用量与对应月份的单月用量查询一致。
	for _, u := range st.MonthlyUsage {
		single, err := s.MonthlyUsage("u", u.Period)
		if err != nil || single.Total != u.Total {
			t.Fatalf("MonthlyUsage %s = %+v %v, want total %d from status list",
				u.Period, single, err, u.Total)
		}
	}

	// 账单摘要的应付、已付、余额、付清状态与截止时刻与当前账单一致，
	// 不因列表顺序而把数值放到别的月份。
	for _, p := range []Month{dec(2025), jan(2026)} {
		summary := statusBillSummary(t, st, p)
		bill, err := s.GetBill("u", p)
		if err != nil {
			t.Fatalf("get bill %s: %v", p, err)
		}
		if summary.TotalDue != bill.TotalDue || summary.Paid != bill.Paid ||
			summary.Balance != bill.Balance || summary.Settled != bill.Settled ||
			!summary.DueAt.Equal(bill.DueAt) {
			t.Fatalf("summary %s = %+v, want matching bill %+v", p, summary, bill)
		}
	}
	// 具体数值：十二月应付 1200、已付 400、余额 800、未付清、截止 2026-01-08；
	// 一月应付 1000、已付清、截止 2026-02-08。
	decSummary := statusBillSummary(t, st, dec(2025))
	if decSummary.TotalDue != 1200 || decSummary.Paid != 400 || decSummary.Balance != 800 ||
		decSummary.Settled || !decSummary.DueAt.Equal(utc(2026, 1, 8, 0, 0)) {
		t.Fatalf("dec summary = %+v, want due 1200 paid 400 balance 800 unsettled due 2026-01-08", decSummary)
	}
	janSummary := statusBillSummary(t, st, jan(2026))
	if janSummary.TotalDue != 1000 || janSummary.Paid != 1000 || janSummary.Balance != 0 ||
		!janSummary.Settled || !janSummary.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("jan summary = %+v, want due 1000 paid 1000 settled due 2026-02-08", janSummary)
	}

	// 单独查询一个尚无记录的月份（二月）得到零用量后，再查看账户状态，
	// 也不应凭空多出该月份；三月、四月依旧没有账单。
	if u, err := s.MonthlyUsage("u", feb(2026)); err != nil || u.Total != 0 {
		t.Fatalf("feb MonthlyUsage = %+v %v, want total 0", u, err)
	}
	st, err = s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	wantStatusUsage(t, st, wantUsageOrder())
	if len(st.Bills) != 2 ||
		st.Bills[0].Period != dec(2025) || st.Bills[1].Period != jan(2026) {
		t.Fatalf("bills after zero-usage query = %+v, want dec 2025 then jan 2026 only", st.Bills)
	}

	// 状态查询本身不补报用量、不生成账单：三月、四月仍查询不到账单。
	for _, p := range []Month{mar(2026), apr(2026)} {
		if _, err := s.GetBill("u", p); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("status query generated bill for %s: %v", p, err)
		}
	}
}

// TestStatusMonthlyHistoryRepeatedQueriesKeepOrder 验证多次查询得到的月度
// 历史列表次序稳定：反复查询、并在两次查询之间补报未出账月份的用量，
// 已保存的月份既不重复也不重排。
func TestStatusMonthlyHistoryRepeatedQueriesKeepOrder(t *testing.T) {
	s := setupCrossYearHistoryFixture(t)

	first, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	wantStatusUsage(t, first, wantUsageOrder())

	// 先结清十二月账单的到期欠费（余额 800）解除停用，再补报三月
	// （尚未出账）的用量：三月总量变为 12，列表次序不变。
	if r, err := s.RecordPayment("u", "pay-dec-2", dec(2025), 800); err != nil || !r.Settled {
		t.Fatalf("settle dec bill = %+v %v", r, err)
	}
	mustRecordEvent(t, s, Event{AccountID: "u", EventID: "e-mar-3", At: utc(2026, 3, 28, 0, 0), Quantity: 1})

	second, err := s.Status("u")
	if err != nil {
		t.Fatal(err)
	}
	want := wantUsageOrder()
	want[2].Total = 12
	wantStatusUsage(t, second, want)
	if len(second.Bills) != 2 ||
		second.Bills[0].Period != dec(2025) || second.Bills[1].Period != jan(2026) {
		t.Fatalf("bills after late march usage = %+v, want dec 2025 then jan 2026 only", second.Bills)
	}
}

// TestStatusMonthlyHistoryEmptyForAccountWithoutRecords 验证没有任何用量与
// 账单的账户两份列表都为空；查询某月零用量不会让该月进入列表；不存在的
// 账户仍返回账户不存在的错误。
func TestStatusMonthlyHistoryEmptyForAccountWithoutRecords(t *testing.T) {
	s, _ := newTestService(utc(2026, 5, 10, 12, 0))
	mustPlan(t, s, planDef("a", 1000, 5, 100, 0))
	mustAccount(t, s, "v")
	mustSubscribe(t, s, "v", "a", utc(2025, 12, 1, 0, 0))

	st, err := s.Status("v")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyUsage) != 0 || len(st.Bills) != 0 {
		t.Fatalf("status without records = %+v, want both lists empty", st)
	}

	// 单独查询一个尚无记录的月份得到零用量，状态列表仍为空。
	if u, err := s.MonthlyUsage("v", jan(2026)); err != nil || u.Total != 0 {
		t.Fatalf("jan MonthlyUsage = %+v %v, want total 0", u, err)
	}
	st, err = s.Status("v")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.MonthlyUsage) != 0 || len(st.Bills) != 0 {
		t.Fatalf("status after zero-usage query = %+v, want both lists empty", st)
	}

	// 不存在的账户仍返回账户不存在的错误。
	if _, err := s.Status("ghost"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("status of missing account: %v", err)
	}
}
