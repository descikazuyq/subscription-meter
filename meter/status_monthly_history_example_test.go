package meter_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/descikazuyq/subscription-meter/meter"
)

// ExampleService_statusMonthlyHistory 演示如何区分“某月累计为零”和“历史
// 列表没有这个月”：Status 的 MonthlyUsage 只包含已经接收过事件或已经生成
// 账单的月份，每月一条、按年月先后排列；Bills 只包含实际生成的账单；而
// MonthlyUsage 单月查询对没有记录的合法月份也返回零累计，却不因此新增历史
// 条目。示例只使用 meter 的公开入口，并通过 NewServiceWithClock 把当前时刻
// 固定在 2026-04-01 00:00 UTC，因此无论实际运行日期为何，输出都固定。
//
// 账户 acct-hist 的订阅自 2026-01-01 00:00 UTC 起持续有效，套餐月费
// 100 分、超额单价与税率均为零。一月只接收一条数量为零的事件；二月没有
// 任何事件和账单；三月没有事件但已生成账单（应付与余额都是 100 分）。
// 查询状态时用量列表按一月、三月排列且两项累计均为零，账单列表只有三月；
// 单独查询二月得到零累计后再次查询状态，二月仍不出现。一个没有任何事件和
// 账单的已有账户两份列表都为空；查询不存在的账户返回 ErrAccountNotFound，
// 不能把查询失败展示成空历史。
func ExampleService_statusMonthlyHistory() {
	const accountID = "acct-hist"
	feb := meter.MonthOf(mustParseTime("2026-02-01T00:00:00Z"))
	mar := meter.MonthOf(mustParseTime("2026-03-01T00:00:00Z"))

	// 把当前时刻固定在 2026-04-01 00:00 UTC：一月、二月、三月账期都已
	// 结束，复制后在任何真实日期运行结果都相同。
	s := meter.NewServiceWithClock(func() time.Time {
		return mustParseTime("2026-04-01T00:00:00Z")
	})

	if err := s.CreateAccount(accountID); err != nil {
		panic(err)
	}
	// 套餐月费 100 分，超额单价与税率均为零：任何月份出账应付都是 100 分。
	if err := s.CreatePlan(meter.Plan{
		ID: "plan100", MonthlyFee: 100, IncludedUnits: 0,
		OveragePrice: 0, TaxRateBasisPoints: 0,
	}); err != nil {
		panic(err)
	}
	// 订阅自 2026-01-01 00:00 UTC 起持续有效，不取消、不换套餐。
	if err := s.Subscribe(accountID, "plan100", mustParseTime("2026-01-01T00:00:00Z")); err != nil {
		panic(err)
	}

	// 一月只接收一条数量为零的事件：合法事件，接收后一月留下一条累计
	// 为零的用量记录。
	ev, err := s.RecordEvent(meter.Event{
		AccountID: accountID, EventID: "e-jan-zero",
		At: mustParseTime("2026-01-10T00:00:00Z"), Quantity: 0,
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("event e-jan-zero accepted=%t period=%s\n", ev.Accepted, ev.Period)

	// 三月没有任何事件，但账期已结束且被订阅覆盖，可以出账：月费 100 分
	// 完整计入，无超额、无税，应付与余额都是 100 分。已出账的月份即使
	// 没有事件也会以零用量出现在用量列表中。
	bill, err := s.CreateBill(accountID, mar)
	if err != nil {
		panic(err)
	}
	fmt.Printf("bill mar     period=%s totalUsage=%d totalDue=%d balance=%d settled=%t dueAt=%s\n",
		bill.Period, bill.TotalUsage, bill.TotalDue, bill.Balance, bill.Settled,
		bill.DueAt.Format(time.RFC3339))

	// 查询状态：用量列表只有一月（接收过零数量事件）与三月（已出账），
	// 两项累计都是零；二月既无事件也无账单，不因订阅持续有效而补进列表。
	// 账单列表只有三月一张实际生成的账单：有用量记录不代表已经出账。
	st, err := s.Status(accountID)
	if err != nil {
		panic(err)
	}
	for i, u := range st.MonthlyUsage {
		fmt.Printf("usage[%d]     period=%s total=%d\n", i, u.Period, u.Total)
	}
	for i, b := range st.Bills {
		fmt.Printf("bills[%d]     period=%s totalDue=%d paid=%d balance=%d settled=%t\n",
			i, b.Period, b.TotalDue, b.Paid, b.Balance, b.Settled)
	}

	// 单独查询没有任何记录的二月：仍返回该月份与零累计，但这次查询不会
	// 把二月写进历史，也不会生成账单。只看这个零值无法判断二月是否接收
	// 过零数量事件、或是否已经出账。
	usage, err := s.MonthlyUsage(accountID, feb)
	if err != nil {
		panic(err)
	}
	fmt.Printf("query feb    period=%s total=%d\n", usage.Period, usage.Total)
	_, err = s.GetBill(accountID, feb)
	fmt.Printf("get bill feb errBillNotFound=%t\n", errors.Is(err, meter.ErrBillNotFound))

	// 再次查询状态：二月仍不在用量列表中，账单列表仍只有三月。
	st, err = s.Status(accountID)
	if err != nil {
		panic(err)
	}
	febInUsage := false
	for _, u := range st.MonthlyUsage {
		if u.Period == feb {
			febInUsage = true
		}
	}
	fmt.Printf("status again usageMonths=%d billMonths=%d febInUsage=%t\n",
		len(st.MonthlyUsage), len(st.Bills), febInUsage)

	// 一个没有任何事件和账单的已有账户：两份历史列表都为空。
	if err := s.CreateAccount("acct-empty"); err != nil {
		panic(err)
	}
	empty, err := s.Status("acct-empty")
	if err != nil {
		panic(err)
	}
	fmt.Printf("empty        usageMonths=%d billMonths=%d\n",
		len(empty.MonthlyUsage), len(empty.Bills))

	// 查询不存在的账户：返回 ErrAccountNotFound。查询失败不能展示成空
	// 历史——空列表只属于“账户存在但没有任何记录”的情况。
	_, err = s.Status("ghost")
	fmt.Printf("ghost status errAccountNotFound=%t\n", errors.Is(err, meter.ErrAccountNotFound))
	_, err = s.MonthlyUsage("ghost", feb)
	fmt.Printf("ghost usage  errAccountNotFound=%t\n", errors.Is(err, meter.ErrAccountNotFound))

	// Output:
	// event e-jan-zero accepted=true period=2026-01
	// bill mar     period=2026-03 totalUsage=0 totalDue=100 balance=100 settled=false dueAt=2026-04-08T00:00:00Z
	// usage[0]     period=2026-01 total=0
	// usage[1]     period=2026-03 total=0
	// bills[0]     period=2026-03 totalDue=100 paid=0 balance=100 settled=false
	// query feb    period=2026-02 total=0
	// get bill feb errBillNotFound=true
	// status again usageMonths=2 billMonths=1 febInUsage=false
	// empty        usageMonths=0 billMonths=0
	// ghost status errAccountNotFound=true
	// ghost usage  errAccountNotFound=true
}
