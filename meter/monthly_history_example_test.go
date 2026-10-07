package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_monthlyHistory 演示账户状态中两份月度历史列表的收录规则，
// 以及单月用量查询 MonthlyUsage 与它们的关系：怎样区分“某月累计为零”和
// “历史列表里根本没有这个月”。示例只使用 meter 的公开入口，并通过
// NewServiceWithClock 把当前时刻固定在 2026-04-01 00:00 UTC，因此无论实际
// 运行日期为何，输出都固定，不必等到某个真实日期。
//
// 账户 acct-hist 的订阅自 2026-01-01 00:00 UTC 起持续有效，套餐月费 100 分、
// 超额单价与税率均为零。一月只接收一条数量为零的事件（该月仍会进入用量
// 列表，累计为零）；二月既没有事件也没有账单；三月没有任何事件，但账期
// 结束后生成账单（零用量按月费出账，应付与余额均为 100 分）。于是：
//
//   - Status.MonthlyUsage 只包含一月、三月，每月一条，按年月先后排列，
//     两条累计都是零；二月不会因为订阅一直有效而被自动补进列表。
//   - Status.Bills 只包含实际生成的三月账单；有用量记录不代表已经出账。
//   - 对二月单独调用 MonthlyUsage 仍返回该月份与零累计，但不会因此新增
//     历史条目，也不会生成账单；只看这个零值无法判断该月是否接收过零
//     数量事件或是否已经出账——三种情况在单月查询里都是零。
//
// 示例还对照了一个没有任何事件和账单的已有账户（两份列表都为空，零用量
// 查询不补条目），以及查询不存在账户时两个入口都返回 ErrAccountNotFound
// （查询失败不能被展示成空历史）。
func ExampleService_monthlyHistory() {
	const accountID = "acct-hist"
	feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))
	mar := meter.MonthOf(mustParseTime("2026-03-01T00:00:00Z"))

	// 把“当前时刻”固定在 2026-04-01 00:00 UTC：一至三月均已结束，
	// 三月可以立即出账；复制后在任何真实日期运行结果都相同。
	s := meter.NewServiceWithClock(func() time.Time {
		return mustParseTime("2026-04-01T00:00:00Z")
	})

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐：月费 100 分，超额单价与税率均为零，零用量账单应付就是 100 分。
	if err := s.CreatePlan(meter.Plan{
		ID:                 "plan-flat",
		MonthlyFee:         100,
		IncludedUnits:      0,
		OveragePrice:       0,
		TaxRateBasisPoints: 0,
	}); err != nil {
		panic(err)
	}
	// 订阅自 2026-01-01 00:00 UTC 起持续有效，不取消、不换套餐。
	if err := s.Subscribe(accountID, "plan-flat",
		mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 一月只接收一条数量为零的事件：数量零是合法值，事件被首次接收，
	// 一月因此进入用量历史，但当月累计仍为零。
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID,
		EventID:   "e-jan-zero",
		At:        mustParseTime("2026-01-10T00:00:00Z"),
		Quantity:  0,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event jan   accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 二月不接收任何事件，也不生成账单。

	// 三月没有任何事件，但 2026-03 账期已结束且被订阅覆盖，直接为它出账：
	// 零用量仍按完整月费计费，应付与余额都是 100 分，出账即让三月进入历史。
	marBill, err := s.CreateBill(accountID, mar)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill mar    period=%s totalUsage=%d totalDue=%d balance=%d settled=%t\n",
		marBill.Period, marBill.TotalUsage, marBill.TotalDue, marBill.Balance, marBill.Settled)

	// 账户状态：用量列表只收录“接收过事件或已生成账单”的月份，每月一条，
	// 按年月先后排列——这里只有一月（零数量事件）与三月（已出账），两条
	// 累计均为零；二月既无事件也无账单，不会因订阅一直有效而被补齐。
	// 账单列表只含实际生成的账单，即三月一张；一月有用量记录但没有出账，
	// 不会出现在账单列表中。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	for _, u := range st.MonthlyUsage {
		fmt.Printf("status usage period=%s total=%d\n", u.Period, u.Total)
	}
	for _, b := range st.Bills {
		fmt.Printf("status bill  period=%s totalDue=%d balance=%d settled=%t\n",
			b.Period, b.TotalDue, b.Balance, b.Settled)
	}

	// 单独查询二月：合法账期、没有任何记录，仍返回该月份与零累计。
	// 注意这个零值不新增历史条目，也不生成账单；仅凭它无法区分“接收过
	// 零数量事件”“已经出账但无用量”“什么都没发生”，三者单月查询都是零。
	u, err := s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("feb query   period=%s total=%d\n", u.Period, u.Total)

	// 再查账户状态：二月仍不出现在用量列表，账单列表也仍只有三月，
	// 两份列表与单月查询前完全相同。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	usagePeriods := make([]meter.Month, 0, len(st.MonthlyUsage))
	for _, x := range st.MonthlyUsage {
		usagePeriods = append(usagePeriods, x.Period)
	}
	billPeriods := make([]meter.Month, 0, len(st.Bills))
	for _, b := range st.Bills {
		billPeriods = append(billPeriods, b.Period)
	}
	fmt.Printf("after query  usagePeriods=%v billPeriods=%v\n", usagePeriods, billPeriods)

	// 对照账户：已创建且订阅同样自一月起有效，但没有任何事件和账单，
	// 两份历史列表都为空。
	if err := s.CreateAccount("acct-empty"); err != nil {
		panic(err)
	}
	if err := s.Subscribe("acct-empty", "plan-flat",
		mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}
	empty, err := s.Status("acct-empty")
	if err != nil {
		panic(err)
	}
	fmt.Printf("empty status usageEntries=%d billEntries=%d\n",
		len(empty.MonthlyUsage), len(empty.Bills))

	// 对这个空账户单独查询一个没有记录的合法月份：返回零累计，但查询
	// 不补条目；随后再查状态，两份列表仍然都为空。
	u, err = s.MonthlyUsage("acct-empty", feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("empty query  period=%s total=%d\n", u.Period, u.Total)
	empty, err = s.Status("acct-empty")
	if err != nil {
		panic(err)
	}
	fmt.Printf("empty after  usageEntries=%d billEntries=%d\n",
		len(empty.MonthlyUsage), len(empty.Bills))

	// 查询不存在的账户：状态与单月用量查询都返回 ErrAccountNotFound。
	// 不能把查询失败当成“没有历史”而展示成空列表。
	_, err = s.Status("acct-missing")
	fmt.Printf("missing status errAccountNotFound=%t\n",
		errors.Is(err, meter.ErrAccountNotFound))
	_, err = s.MonthlyUsage("acct-missing", feb)
	fmt.Printf("missing usage  errAccountNotFound=%t\n",
		errors.Is(err, meter.ErrAccountNotFound))

	// Output:
	// event jan   accepted=true period=2026-01
	// bill mar    period=2026-03 totalUsage=0 totalDue=100 balance=100 settled=false
	// status usage period=2026-01 total=0
	// status usage period=2026-03 total=0
	// status bill  period=2026-03 totalDue=100 balance=100 settled=false
	// feb query   period=2026-02 total=0
	// after query  usagePeriods=[2026-01 2026-03] billPeriods=[2026-03]
	// empty status usageEntries=0 billEntries=0
	// empty query  period=2026-02 total=0
	// empty after  usageEntries=0 billEntries=0
	// missing status errAccountNotFound=true
	// missing usage  errAccountNotFound=true
}
