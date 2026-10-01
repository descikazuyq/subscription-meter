package meter

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

// fakeClock 是可手动推进的时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newMeterAt(t *testing.T, at time.Time) *Meter {
	t.Helper()
	fc := &fakeClock{t: at}
	return NewMeterWithClock(fc.now)
}

func jan1() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
func feb1() time.Time { return time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) }
func mar1() time.Time { return time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) }

// 开通一个标准套餐并返回系统。
func setupMeter(t *testing.T) *Meter {
	t.Helper()
	m := newMeterAt(t, feb1())
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreatePlan(Plan{ID: "p1", Name: "标准", MonthlyFee: 1000, IncludedUsage: 100, OveragePrice: 10, TaxRateBP: 500}); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAccountPlanSubscription(t *testing.T) {
	m := newMeterAt(t, jan1())

	// 账户唯一。
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateAccount("a1"); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("期望 ErrAccountExists，得到 %v", err)
	}
	if err := m.CreateAccount(""); !errors.Is(err, ErrIDEmpty) {
		t.Fatalf("期望 ErrIDEmpty，得到 %v", err)
	}

	// 套餐校验：负数与非法税率。
	badPlans := []Plan{
		{ID: "bad1", MonthlyFee: -1, IncludedUsage: 10, OveragePrice: 1, TaxRateBP: 100},
		{ID: "bad2", MonthlyFee: 1, IncludedUsage: -1, OveragePrice: 1, TaxRateBP: 100},
		{ID: "bad3", MonthlyFee: 1, IncludedUsage: 10, OveragePrice: -1, TaxRateBP: 100},
		{ID: "bad4", MonthlyFee: 1, IncludedUsage: 10, OveragePrice: 1, TaxRateBP: -1},
		{ID: "bad5", MonthlyFee: 1, IncludedUsage: 10, OveragePrice: 1, TaxRateBP: 10001},
	}
	for _, p := range badPlans {
		err := m.CreatePlan(p)
		if errors.Is(err, ErrNegativeAmount) || errors.Is(err, ErrInvalidTaxRate) {
			continue
		}
		t.Fatalf("套餐 %s 应被拒绝，得到 %v", p.ID, err)
	}

	// 合法套餐创建成功且唯一。
	if err := m.CreatePlan(Plan{ID: "p1", MonthlyFee: 1000, IncludedUsage: 100, OveragePrice: 10, TaxRateBP: 500}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreatePlan(Plan{ID: "p1", MonthlyFee: 1, IncludedUsage: 1, OveragePrice: 1, TaxRateBP: 1}); !errors.Is(err, ErrPlanExists) {
		t.Fatalf("期望 ErrPlanExists，得到 %v", err)
	}

	// 修改套餐：不存在则失败。
	if err := m.UpdatePlan(Plan{ID: "nope", MonthlyFee: 1, IncludedUsage: 1, OveragePrice: 1, TaxRateBP: 1}); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("期望 ErrPlanNotFound，得到 %v", err)
	}
	if err := m.UpdatePlan(Plan{ID: "p1", MonthlyFee: 2000, IncludedUsage: 200, OveragePrice: 20, TaxRateBP: 600}); err != nil {
		t.Fatal(err)
	}

	// 开通订阅：缺失引用失败，重复开通失败。
	if err := m.ActivateSubscription("nope", "p1", jan1()); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("期望 ErrAccountNotFound，得到 %v", err)
	}
	if err := m.ActivateSubscription("a1", "nope", jan1()); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("期望 ErrPlanNotFound，得到 %v", err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); !errors.Is(err, ErrSubscriptionExists) {
		t.Fatalf("期望 ErrSubscriptionExists，得到 %v", err)
	}
}

func TestReportUsageDedup(t *testing.T) {
	m := setupMeter(t)

	// 正常上报。
	ev, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 30)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Quantity != 30 {
		t.Fatalf("事件数量应为 30，得到 %d", ev.Quantity)
	}

	// 完全相同的重报返回原结果，不重复累计。
	ev2, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 30)
	if err != nil {
		t.Fatal(err)
	}
	if ev2 != ev {
		t.Fatal("相同重报应返回原事件")
	}

	// 相同标识不同数量 -> 冲突。
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 31); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("期望 ErrEventConflict，得到 %v", err)
	}
	// 相同标识不同时刻 -> 冲突。
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC), 30); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("期望 ErrEventConflict，得到 %v", err)
	}

	// 不同账户可以使用相同事件标识。
	if err := m.CreateAccount("a2"); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a2", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReportUsage("a2", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 7); err != nil {
		t.Fatalf("不同账户相同标识应成功，得到 %v", err)
	}

	// 负数拒绝。
	if _, err := m.ReportUsage("a1", "e2", jan1(), -1); !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("期望 ErrNegativeAmount，得到 %v", err)
	}
	// 早于开通时刻拒绝。
	if _, err := m.ReportUsage("a1", "e3", time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), 1); !errors.Is(err, ErrEventBeforeActivation) {
		t.Fatalf("期望 ErrEventBeforeActivation，得到 %v", err)
	}
	// 晚于当前时刻拒绝（时钟在 2 月 1 日，上报 2 月 2 日）。
	if _, err := m.ReportUsage("a1", "e4", time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC), 1); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("期望 ErrEventInFuture，得到 %v", err)
	}

	// 累计用量不受重报影响。
	if _, err := m.ReportUsage("a1", "e5", time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), 50); err != nil {
		t.Fatal(err)
	}
	got, err := m.MonthlyUsage("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if got != 80 {
		t.Fatalf("1 月累计用量应为 80，得到 %d", got)
	}
}

func TestPeriodAttribution(t *testing.T) {
	// 时钟设到 2 月 2 日，确保 2 月 1 日的事件不被当作未来事件。
	fc := &fakeClock{t: time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)}
	m := NewMeterWithClock(fc.now)
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreatePlan(Plan{ID: "p1", MonthlyFee: 1000, IncludedUsage: 100, OveragePrice: 10, TaxRateBP: 500}); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	// 1 月 31 日与 2 月 1 日属于不同账期（月初属于新一期）。
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReportUsage("a1", "e2", time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC), 20); err != nil {
		t.Fatal(err)
	}
	jan, err := m.MonthlyUsage("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	feb, err := m.MonthlyUsage("a1", 2026, time.February)
	if err != nil {
		t.Fatal(err)
	}
	if jan != 10 || feb != 20 {
		t.Fatalf("账期归属错误：1 月=%d 2 月=%d", jan, feb)
	}
}

func TestBillGeneration(t *testing.T) {
	m := setupMeter(t)
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), 150); err != nil {
		t.Fatal(err)
	}

	// 1 月账单：总用量 150，包含 100，超额 50，超额费用 500，月费 1000，
	// 税额 (1000+500)*500/10000 = 75，总额 1575。
	bill, err := m.GenerateBill("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if bill.TotalUsage != 150 || bill.OverageUsage != 50 || bill.OverageFee != 500 {
		t.Fatalf("用量/超额费用错误：%+v", bill)
	}
	if bill.MonthlyFee != 1000 || bill.Tax != 75 || bill.TotalAmount != 1575 {
		t.Fatalf("费用/税额错误：%+v", bill)
	}
	if bill.Status != BillStatusUnpaid {
		t.Fatalf("新账单应为未付款，得到 %s", bill.Status)
	}
	if !bill.DueAt.Equal(feb1().Add(7 * 24 * time.Hour)) {
		t.Fatalf("付款截止时刻错误：%v", bill.DueAt)
	}

	// 重复出账幂等。
	bill2, err := m.GenerateBill("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if bill2 != bill {
		t.Fatal("重复出账应返回同一张账单")
	}

	// 早于开通月不能出账。
	if _, err := m.GenerateBill("a1", 2025, time.December); !errors.Is(err, ErrPeriodBeforeActivation) {
		t.Fatalf("期望 ErrPeriodBeforeActivation，得到 %v", err)
	}
	// 尚未结束的月份不能出账（时钟在 2 月 1 日，2 月未结束）。
	if _, err := m.GenerateBill("a1", 2026, time.February); !errors.Is(err, ErrPeriodNotEnded) {
		t.Fatalf("期望 ErrPeriodNotEnded，得到 %v", err)
	}

	// 2 月账单：无用量，仍收完整月费并提供完整额度。
	m2 := newMeterAt(t, mar1())
	if err := m2.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m2.CreatePlan(Plan{ID: "p1", MonthlyFee: 1000, IncludedUsage: 100, OveragePrice: 10, TaxRateBP: 500}); err != nil {
		t.Fatal(err)
	}
	if err := m2.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	febBill, err := m2.GenerateBill("a1", 2026, time.February)
	if err != nil {
		t.Fatal(err)
	}
	if febBill.TotalUsage != 0 || febBill.OverageUsage != 0 || febBill.OverageFee != 0 {
		t.Fatalf("2 月用量应为 0：%+v", febBill)
	}
	if febBill.MonthlyFee != 1000 || febBill.Tax != 50 || febBill.TotalAmount != 1050 {
		t.Fatalf("2 月费用错误：%+v", febBill)
	}
}

func TestZeroAmountBillPaid(t *testing.T) {
	m := newMeterAt(t, feb1())
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreatePlan(Plan{ID: "p0", MonthlyFee: 0, IncludedUsage: 0, OveragePrice: 0, TaxRateBP: 0}); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p0", jan1()); err != nil {
		t.Fatal(err)
	}
	bill, err := m.GenerateBill("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if bill.TotalAmount != 0 || bill.Status != BillStatusPaid {
		t.Fatalf("零金额账单应直接付清：%+v", bill)
	}
	// 已付清账单不接受付款。
	if _, err := m.RegisterPayment("a1", "pay1", 2026, time.January, 1); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("期望 ErrPaymentExceedsBalance，得到 %v", err)
	}
}

func TestPlanSnapshot(t *testing.T) {
	m := setupMeter(t)
	// 开通后修改套餐，1 月账单仍按开通时条件计费。
	if err := m.UpdatePlan(Plan{ID: "p1", MonthlyFee: 9999, IncludedUsage: 999, OveragePrice: 99, TaxRateBP: 9999}); err != nil {
		t.Fatal(err)
	}
	bill, err := m.GenerateBill("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if bill.MonthlyFee != 1000 || bill.IncludedUsage != 100 || bill.OveragePrice != 10 || bill.TaxRateBP != 500 {
		t.Fatalf("账单应保持开通时的套餐快照：%+v", bill)
	}
}

func TestClosedPeriod(t *testing.T) {
	m := setupMeter(t)
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GenerateBill("a1", 2026, time.January); err != nil {
		t.Fatal(err)
	}

	// 已出账月份拒绝首次出现的新事件。
	if _, err := m.ReportUsage("a1", "e2", time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC), 5); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("期望 ErrPeriodClosed，得到 %v", err)
	}
	// 已接收事件的相同重报仍成功。
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 10); err != nil {
		t.Fatalf("已出账月份的相同重报应成功，得到 %v", err)
	}
	// 用量不被新事件改变。
	got, err := m.MonthlyUsage("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("出账后用量应固定为 10，得到 %d", got)
	}
}

func TestPayment(t *testing.T) {
	fc := &fakeClock{t: feb1()}
	m := NewMeterWithClock(fc.now)
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreatePlan(Plan{ID: "p1", MonthlyFee: 1000, IncludedUsage: 100, OveragePrice: 10, TaxRateBP: 500}); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), 150); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GenerateBill("a1", 2026, time.January); err != nil {
		t.Fatal(err)
	}
	// 推进到 3 月 1 日，生成 2 月账单，用于付款冲突测试。
	fc.advance(28 * 24 * time.Hour)
	if _, err := m.GenerateBill("a1", 2026, time.February); err != nil {
		t.Fatal(err)
	}

	// 非法付款。
	if _, err := m.RegisterPayment("a1", "p0", 2026, time.January, 0); !errors.Is(err, ErrInvalidPayment) {
		t.Fatalf("期望 ErrInvalidPayment，得到 %v", err)
	}
	if _, err := m.RegisterPayment("a1", "p0", 2026, time.January, -1); !errors.Is(err, ErrInvalidPayment) {
		t.Fatalf("期望 ErrInvalidPayment，得到 %v", err)
	}
	if _, err := m.RegisterPayment("a1", "p0", 2026, time.March, 1); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("期望 ErrBillNotFound，得到 %v", err)
	}
	// 超过余额。
	if _, err := m.RegisterPayment("a1", "p1", 2026, time.January, 2000); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("期望 ErrPaymentExceedsBalance，得到 %v", err)
	}

	// 分次付款。
	pay, err := m.RegisterPayment("a1", "pay1", 2026, time.January, 1000)
	if err != nil {
		t.Fatal(err)
	}
	bal, err := m.BillBalance("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 575 {
		t.Fatalf("余额应为 575，得到 %d", bal)
	}
	if pay.Amount != 1000 {
		t.Fatalf("付款金额错误：%+v", pay)
	}

	// 相同账单与金额的重报返回原结果。
	pay2, err := m.RegisterPayment("a1", "pay1", 2026, time.January, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if pay2 != pay {
		t.Fatal("相同付款重报应返回原结果")
	}
	// 相同标识改金额 -> 冲突。
	if _, err := m.RegisterPayment("a1", "pay1", 2026, time.January, 575); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("期望 ErrPaymentConflict，得到 %v", err)
	}
	// 相同标识改账单 -> 冲突。
	if _, err := m.RegisterPayment("a1", "pay1", 2026, time.February, 1000); !errors.Is(err, ErrPaymentConflict) {
		t.Fatalf("期望 ErrPaymentConflict，得到 %v", err)
	}

	// 付清。
	if _, err := m.RegisterPayment("a1", "pay2", 2026, time.January, 575); err != nil {
		t.Fatal(err)
	}
	bal, err = m.BillBalance("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Fatalf("付清后余额应为 0，得到 %d", bal)
	}
	bills, err := m.ListBills("a1")
	if err != nil {
		t.Fatal(err)
	}
	if bills[0].Status != BillStatusPaid {
		t.Fatalf("账单应已付清：%s", bills[0].Status)
	}
	// 已付清账单再付款超过余额。
	if _, err := m.RegisterPayment("a1", "pay3", 2026, time.January, 1); !errors.Is(err, ErrPaymentExceedsBalance) {
		t.Fatalf("期望 ErrPaymentExceedsBalance，得到 %v", err)
	}
}

func TestArrearsSuspension(t *testing.T) {
	fc := &fakeClock{t: feb1()}
	m := NewMeterWithClock(fc.now)
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreatePlan(Plan{ID: "p1", MonthlyFee: 1000, IncludedUsage: 100, OveragePrice: 10, TaxRateBP: 500}); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GenerateBill("a1", 2026, time.January); err != nil {
		t.Fatal(err)
	}

	// 截止时刻（2 月 8 日）之前不应停用。
	suspended, err := m.IsSuspended("a1")
	if err != nil {
		t.Fatal(err)
	}
	if suspended {
		t.Fatal("截止时刻前不应停用")
	}

	// 推进到 2 月 9 日（截止时刻之后）仍有余额 -> 停用。
	fc.advance(8 * 24 * time.Hour)
	suspended, err = m.IsSuspended("a1")
	if err != nil {
		t.Fatal(err)
	}
	if !suspended {
		t.Fatal("到期未付应停用")
	}

	// 停用期间拒绝新用量。
	if _, err := m.ReportUsage("a1", "e2", time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC), 5); !errors.Is(err, ErrAccountSuspended) {
		t.Fatalf("期望 ErrAccountSuspended，得到 %v", err)
	}
	// 已接收事件的相同重报仍成功。
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 10); err != nil {
		t.Fatalf("停用期间相同重报应成功，得到 %v", err)
	}

	// 付清欠费后恢复。
	if _, err := m.RegisterPayment("a1", "pay1", 2026, time.January, 1050); err != nil {
		t.Fatal(err)
	}
	suspended, err = m.IsSuspended("a1")
	if err != nil {
		t.Fatal(err)
	}
	if suspended {
		t.Fatal("付清后应恢复")
	}
	if _, err := m.ReportUsage("a1", "e2", time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC), 5); err != nil {
		t.Fatalf("恢复后用量应可上报，得到 %v", err)
	}
}

func TestTaxRounding(t *testing.T) {
	m := newMeterAt(t, feb1())
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	// 月费 105 分，税率 10%：税额 10.5 四舍五入为 11。
	if err := m.CreatePlan(Plan{ID: "p1", MonthlyFee: 105, IncludedUsage: 100, OveragePrice: 0, TaxRateBP: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	bill, err := m.GenerateBill("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if bill.Tax != 11 || bill.TotalAmount != 116 {
		t.Fatalf("税额四舍五入错误：tax=%d total=%d", bill.Tax, bill.TotalAmount)
	}
}

func TestOverflow(t *testing.T) {
	m := newMeterAt(t, feb1())
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreatePlan(Plan{ID: "p1", MonthlyFee: 0, IncludedUsage: 0, OveragePrice: math.MaxInt64, TaxRateBP: 0}); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	// 超额费用 = MaxInt64 * MaxInt64 溢出，请求失败且无部分结果。
	if _, err := m.GenerateBill("a1", 2026, time.January); !errors.Is(err, ErrOverflow) {
		t.Fatalf("期望 ErrOverflow，得到 %v", err)
	}
	bills, err := m.ListBills("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(bills) != 0 {
		t.Fatal("溢出失败不应留下账单")
	}

	// 累计用量溢出。
	m2 := newMeterAt(t, feb1())
	if err := m2.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m2.CreatePlan(Plan{ID: "p1", MonthlyFee: 0, IncludedUsage: 0, OveragePrice: 1, TaxRateBP: 0}); err != nil {
		t.Fatal(err)
	}
	if err := m2.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.ReportUsage("a1", "e2", time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC), math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.GenerateBill("a1", 2026, time.January); !errors.Is(err, ErrOverflow) {
		t.Fatalf("期望 ErrOverflow，得到 %v", err)
	}
	bills2, err := m2.ListBills("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(bills2) != 0 {
		t.Fatal("溢出失败不应留下账单")
	}
}

func TestFailedRequestDoesNotConsumeID(t *testing.T) {
	m := setupMeter(t)

	// 用量失败不消耗事件标识。
	if _, err := m.ReportUsage("a1", "ex", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), -1); !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("期望 ErrNegativeAmount，得到 %v", err)
	}
	if _, err := m.ReportUsage("a1", "ex", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 5); err != nil {
		t.Fatalf("失败后相同标识应可成功，得到 %v", err)
	}

	// 付款失败不消耗付款标识。
	if _, err := m.RegisterPayment("a1", "px", 2026, time.January, 0); !errors.Is(err, ErrInvalidPayment) {
		t.Fatalf("期望 ErrInvalidPayment，得到 %v", err)
	}
	if _, err := m.RegisterPayment("a1", "px", 2026, time.January, 100); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("期望 ErrBillNotFound（未出账），得到 %v", err)
	}
	if _, err := m.GenerateBill("a1", 2026, time.January); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RegisterPayment("a1", "px", 2026, time.January, 100); err != nil {
		t.Fatalf("失败后相同付款标识应可成功，得到 %v", err)
	}
}

func TestConcurrency(t *testing.T) {
	m := newMeterAt(t, feb1())
	if err := m.CreateAccount("a1"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreatePlan(Plan{ID: "p1", MonthlyFee: 1000, IncludedUsage: 100, OveragePrice: 10, TaxRateBP: 500}); err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateSubscription("a1", "p1", jan1()); err != nil {
		t.Fatal(err)
	}

	// 并发重复提交同一事件：只累计一次。
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.ReportUsage("a1", "e1", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), 10)
		}()
	}
	wg.Wait()
	got, err := m.MonthlyUsage("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("并发重复提交应只累计一次，得到 %d", got)
	}

	// 并发出账：只生成一张账单。
	bills := make([]*Bill, 100)
	errs := make([]error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bills[i], errs[i] = m.GenerateBill("a1", 2026, time.January)
		}(i)
	}
	wg.Wait()
	for i := 0; i < 100; i++ {
		if errs[i] != nil {
			t.Fatalf("出账失败：%v", errs[i])
		}
		if bills[i] != bills[0] {
			t.Fatal("并发出账应返回同一张账单")
		}
	}
	list, err := m.ListBills("a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应只有一张账单，得到 %d", len(list))
	}

	// 并发重复付款：只扣减一次余额。
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.RegisterPayment("a1", "pay1", 2026, time.January, 100)
		}()
	}
	wg.Wait()
	bal, err := m.BillBalance("a1", 2026, time.January)
	if err != nil {
		t.Fatal(err)
	}
	// 用量仅 10（无超额），账单总额 1050，扣减一次 100 -> 余额 950。
	if bal != 950 {
		t.Fatalf("并发重复付款应只扣减一次，余额应为 950，得到 %d", bal)
	}
}
