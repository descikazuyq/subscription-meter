package meter

import (
	"errors"
	"testing"
)

// 大金额计税回归保障：税前费用直接乘税率会超出 int64，但税额与应付总额
// 仍落在非负 int64 内时，出账必须按分精确完成，不得误报溢出或损失精度。

// subscribeJan 开通 1 月订阅并把时钟拨到 2 月，使 1 月账期可出账。
func subscribeJan(t *testing.T, s *Service, clk *fakeClock, accountID, planID string) {
	t.Helper()
	if err := s.Subscribe(accountID, planID, utc(2026, 1, 1, 0, 0)); err != nil {
		t.Fatalf("subscribe %q: %v", accountID, err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)
}

func TestBillLargeAmountTaxExact(t *testing.T) {
	// 税前 8000000000000000005 分、税率 1000（10%）：
	// 直接相乘 8e21 远超 int64，但税额 800000000000000001、
	// 应付总额 8800000000000000006 均合法，必须正常出账。
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 8000000000000000005, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	subscribeJan(t, s, clk, "a", "p")

	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatalf("large amount bill must succeed: %v", err)
	}
	want := Bill{
		AccountID:     "a",
		Period:        jan(2026),
		Terms:         PlanTerms{PlanID: "p", MonthlyFee: 8000000000000000005, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 1000},
		TotalUsage:    0,
		IncludedUnits: 0,
		OverageUnits:  0,
		MonthlyFee:    8000000000000000005,
		OverageFee:    0,
		Tax:           800000000000000001,
		TotalDue:      8800000000000000006,
		Paid:          0,
		Balance:       8800000000000000006,
		Settled:       false,
		DueAt:         utc(2026, 2, 8, 0, 0),
	}
	if b != want {
		t.Fatalf("bill:\n got %+v\nwant %+v", b, want)
	}

	// 查询同一账期得到同一张完整账单，金额与初始余额一致。
	got, err := s.GetBill("a", jan(2026))
	if err != nil || got != want {
		t.Fatalf("get bill: %+v %v", got, err)
	}
	st, err := s.Status("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Bills) != 1 || st.Bills[0].TotalDue != want.TotalDue || st.Bills[0].Balance != want.TotalDue || st.Bills[0].Paid != 0 || st.Bills[0].Settled {
		t.Fatalf("status summary: %+v", st.Bills)
	}
}

func TestBillLargeAmountHalfCentBoundary(t *testing.T) {
	// 大金额下恰好半分向上、不足半分舍去、超过半分进一分。
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	cases := []struct {
		account   string
		subtotal  int64
		wantTax   int64
		wantTotal int64
	}{
		{"down", 8000000000000000004, 800000000000000000, 8800000000000000004}, // .4 舍去
		{"half", 8000000000000000005, 800000000000000001, 8800000000000000006}, // 恰好 .5 向上
		{"up", 8000000000000000006, 800000000000000001, 8800000000000000007},   // .6 进一
	}
	for _, c := range cases {
		mustPlan(t, s, Plan{ID: "p-" + c.account, MonthlyFee: c.subtotal, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 1000})
		mustAccount(t, s, c.account)
		subscribeJan(t, s, clk, c.account, "p-"+c.account)
	}
	for _, c := range cases {
		b, err := s.CreateBill(c.account, jan(2026))
		if err != nil {
			t.Fatalf("%s: bill must succeed: %v", c.account, err)
		}
		if b.Tax != c.wantTax || b.TotalDue != c.wantTotal || b.Balance != c.wantTotal || b.Paid != 0 {
			t.Fatalf("%s: tax=%d due=%d balance=%d paid=%d, want tax=%d due=%d",
				c.account, b.Tax, b.TotalDue, b.Balance, b.Paid, c.wantTax, c.wantTotal)
		}
	}
}

func TestBillLargeAmountMonthlyPlusOverageTaxedOnce(t *testing.T) {
	// 月费与超额费用分别贡献税前金额，税额只对二者之和计算一次：
	// 各项 .3 分别舍入都为 0，相加得 800000000000000000，是错误结果；
	// 合并后 .6 进一，税额应为 800000000000000001。
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 4000000000000000003, IncludedUnits: 0, OveragePrice: 4000000000000000003, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "a")
	subscribeJan(t, s, clk, "a", "p")

	// 开通后修改套餐单价：1 月账单仍按开通时锁定的单价计超额。
	if err := s.UpdatePlan(Plan{ID: "p", MonthlyFee: 1, IncludedUnits: 0, OveragePrice: 1, TaxRateBasisPoints: 0}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 1, 15, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)

	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatalf("bill must succeed: %v", err)
	}
	if b.TotalUsage != 1 || b.OverageUnits != 1 || b.OverageFee != 4000000000000000003 {
		t.Fatalf("overage detail: %+v", b)
	}
	if b.MonthlyFee != 4000000000000000003 {
		t.Fatalf("monthly fee: %d", b.MonthlyFee)
	}
	// 税前 8000000000000000006，税额 800000000000000001，总额 8800000000000000007。
	if b.Tax != 800000000000000001 || b.TotalDue != 8800000000000000007 {
		t.Fatalf("tax computed per-component instead of once: tax=%d due=%d", b.Tax, b.TotalDue)
	}
	if b.Paid != 0 || b.Balance != 8800000000000000007 || b.Settled {
		t.Fatalf("initial payment state: paid=%d balance=%d settled=%v", b.Paid, b.Balance, b.Settled)
	}
}

func TestBillLargeAmountZeroTaxRateMaxInt64(t *testing.T) {
	// 税率为零时，合法金额达到 int64 上限也应正常出账，税额为零。
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: maxInt64, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	subscribeJan(t, s, clk, "a", "p")

	b, err := s.CreateBill("a", jan(2026))
	if err != nil {
		t.Fatalf("max int64 with zero rate must succeed: %v", err)
	}
	if b.Tax != 0 || b.TotalDue != maxInt64 || b.Balance != maxInt64 || b.Paid != 0 || b.Settled {
		t.Fatalf("bill: %+v", b)
	}
}

func TestBillLargeAmountOverflowStillRejected(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))

	// 费用之和本身溢出：月费 maxInt64 + 超额 1 分。
	mustPlan(t, s, Plan{ID: "p-sum", MonthlyFee: maxInt64, IncludedUnits: 0, OveragePrice: 1, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	subscribeJan(t, s, clk, "a", "p-sum")

	// 税前合法但含税总额溢出：9000000000000000000 + 税额 900000000000000000 超上限。
	mustPlan(t, s, Plan{ID: "p-total", MonthlyFee: 9000000000000000000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 1000})
	mustAccount(t, s, "b")
	subscribeJan(t, s, clk, "b", "p-total")

	clk.t = utc(2026, 1, 15, 12, 0)
	if _, err := s.RecordEvent(Event{AccountID: "a", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordEvent(Event{AccountID: "b", EventID: "e1", At: utc(2026, 1, 10, 0, 0), Quantity: 7}); err != nil {
		t.Fatal(err)
	}
	clk.t = utc(2026, 2, 1, 0, 0)

	for _, account := range []string{"a", "b"} {
		if _, err := s.CreateBill(account, jan(2026)); !errors.Is(err, ErrOverflow) {
			t.Fatalf("%s: expected ErrOverflow, got %v", account, err)
		}
		// 出账失败不留账单，重试仍是溢出。
		if _, err := s.GetBill(account, jan(2026)); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("%s: bill left behind: %v", account, err)
		}
		if _, err := s.CreateBill(account, jan(2026)); !errors.Is(err, ErrOverflow) {
			t.Fatalf("%s: retry expected ErrOverflow, got %v", account, err)
		}
	}
	// 已记录的月用量保持原值。
	if u, _ := s.MonthlyUsage("a", jan(2026)); u.Total != 1 {
		t.Fatalf("a usage = %d, want 1", u.Total)
	}
	if u, _ := s.MonthlyUsage("b", jan(2026)); u.Total != 7 {
		t.Fatalf("b usage = %d, want 7", u.Total)
	}
}
