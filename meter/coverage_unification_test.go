package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归“订阅覆盖范围判断”对用量接收与月度出账必须共用同一依据：
//
//	账户一月一日开通套餐甲，一月内登记按月取消，订阅于二月一日零点终止；
//	二月整月空档；三月二日提前登记三月十日中午按套餐丙重新开通。
//
// 据此锁定题述主线：
//   - 二月（终止所在月）既不接收用量，月末结束后也不能出账；
//   - 三月十日开通之前，三月的事件即使已经发生也被拒绝（未来登记不给资格）；
//   - 三月十日中午（开通瞬间）起事件计入新订阅，归入三月；
//   - 三月结束后可按新订阅丙的完整月费与额度出整月账单——
//     同一个三月“部分时刻不接收用量、当月却可以出账”的差别必须保留；
//   - 已出账不扩大覆盖：四月补报三月十日开通前的事件仍按空档拒绝；
//   - 旧订阅一月账单仍按甲条件，不被三月的新订阅覆盖。

// TestCoverageSharedByUsageAndBillingMidMonthReopen 是上述主线的确定性回归。
func TestCoverageSharedByUsageAndBillingMidMonthReopen(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 15, 12, 0))
	// 甲：月费 1000、额度 10、超额单价 10；丙：月费 3000、额度 5、超额单价 20。
	mustPlan(t, s, planDef("a", 1000, 10, 10, 0))
	mustPlan(t, s, planDef("c", 3000, 5, 20, 0))
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "a", utc(2026, 1, 1, 0, 0))

	// 一月接收 12 单位（在甲额度内超额 2）。
	clk.t = utc(2026, 1, 20, 12, 0)
	if r, err := s.RecordEvent(Event{AccountID: "u", EventID: "jan-12", At: utc(2026, 1, 20, 0, 0), Quantity: 12}); err != nil ||
		!r.Accepted || r.Period != jan(2026) {
		t.Fatalf("january usage: %+v %v", r, err)
	}
	mustCancel(t, s, "u") // 终止时刻 2026-02-01 00:00。

	// 三月二日：订阅已于二月一日终止，提前登记三月十日中午重新开通。
	clk.t = utc(2026, 3, 2, 9, 0)
	reopenAt := utc(2026, 3, 10, 12, 0)
	mustSubscribe(t, s, "u", "c", reopenAt)

	// 二月是旧订阅终止所在月：事件拒绝、月末结束后也不能出账。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "feb-gap", At: utc(2026, 2, 15, 0, 0), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("february usage in termination month: %v, want ErrEventBeforeSubscription", err)
	}
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("february bill in termination month: %v, want ErrBillBeforeSubscription", err)
	}

	// 三月五日处于空档，新订阅尚未生效：已发生的三月事件仍被拒绝。
	clk.t = utc(2026, 3, 5, 10, 0)
	if st, _ := s.Status("u"); st.Subscribed {
		t.Fatalf("subscribed before reopen instant: %+v", st)
	}
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mar-before", At: utc(2026, 3, 5, 9, 0), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("march usage before activation: %v, want ErrEventBeforeSubscription", err)
	}
	// 指向开通瞬间的事件在此刻仍是未来事件，同样不获资格。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mar-at-open", At: reopenAt, Quantity: 1}); !errors.Is(err, ErrEventInFuture) {
		t.Fatalf("event at future reopen instant: %v, want ErrEventInFuture", err)
	}

	// 开通前一分钟：开通当天上午的事件已发生却早于开通，依旧拒绝；时刻精确。
	clk.t = reopenAt.Add(-time.Minute)
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mar-morning", At: utc(2026, 3, 10, 10, 0), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("march morning usage before activation: %v, want ErrEventBeforeSubscription", err)
	}

	// 到达开通瞬间：订阅生效，开通瞬间事件计入新订阅，归入三月。
	clk.t = reopenAt
	st, _ := s.Status("u")
	if !st.Subscribed || st.CurrentTerms.PlanID != "c" {
		t.Fatalf("status at reopen instant = %+v, want subscribed with plan c", st)
	}
	openEvent := Event{AccountID: "u", EventID: "mar-at-open", At: reopenAt, Quantity: 8}
	r, err := s.RecordEvent(openEvent)
	if err != nil || !r.Accepted || r.Period != mar(2026) {
		t.Fatalf("event at reopen instant: %+v %v, want accepted into march", r, err)
	}
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 8 {
		t.Fatalf("march usage at reopen = %d, want 8", u.Total)
	}
	// 开通前的事件在开通后仍不属于任何订阅期间：拒绝结果不随重新开通改变。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mar-before2", At: utc(2026, 3, 10, 11, 59), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("pre-activation event after reopen: %v, want ErrEventBeforeSubscription", err)
	}

	// 四月一日：三月已结束。二月仍不能出账；三月按新订阅丙出完整月账单——
	// 用量 8、额度 5、超额 3*20=60、月费 3000、税 0、应付 3060。
	clk.t = utc(2026, 4, 1, 0, 0)
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("february bill after reopen: %v, want ErrBillBeforeSubscription", err)
	}
	marBill, err := s.CreateBill("u", mar(2026))
	if err != nil {
		t.Fatalf("create march bill: %v", err)
	}
	wantC := PlanTerms{PlanID: "c", MonthlyFee: 3000, IncludedUnits: 5, OveragePrice: 20, TaxRateBasisPoints: 0}
	if marBill.Terms != wantC || marBill.TotalUsage != 8 || marBill.OverageUnits != 3 ||
		marBill.MonthlyFee != 3000 || marBill.OverageFee != 60 || marBill.Tax != 0 || marBill.TotalDue != 3060 ||
		marBill.Balance != 3060 || marBill.Settled {
		t.Fatalf("march bill = %+v, want plan c full terms usage 8 due 3060", marBill)
	}
	if !marBill.DueAt.Equal(utc(2026, 4, 8, 0, 0)) {
		t.Fatalf("march due at = %v, want 2026-04-08", marBill.DueAt)
	}

	// 已出账不扩大覆盖：补报三月十日开通之前的事件，先按“不在订阅期间”拒绝，
	// 而不是已出账错误——覆盖判断不以月份是否出账为转移。
	if _, err := s.RecordEvent(Event{AccountID: "u", EventID: "mar-pre-bill", At: utc(2026, 3, 10, 11, 0), Quantity: 1}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("backfill pre-activation event after billed: %v, want ErrEventBeforeSubscription", err)
	}

	// 旧订阅一月仍按甲条件出账，不被三月的新订阅覆盖：
	// 用量 12、额度 10、超额 2*10=20、月费 1000、应付 1020。
	janBill := mustBill(t, s, "u", jan(2026))
	wantA := PlanTerms{PlanID: "a", MonthlyFee: 1000, IncludedUnits: 10, OveragePrice: 10, TaxRateBasisPoints: 0}
	if janBill.Terms != wantA || janBill.TotalUsage != 12 || janBill.OverageUnits != 2 ||
		janBill.MonthlyFee != 1000 || janBill.OverageFee != 20 || janBill.TotalDue != 1020 {
		t.Fatalf("january bill = %+v, want plan a usage 12 due 1020", janBill)
	}

	// 开通瞬间事件的 +08:00 表示（本地 3/10 20:00 == UTC 12:00）是同一瞬间：
	// 重报成功但不再次累计，三月用量仍为 8。
	zoned := Event{
		AccountID: "u", EventID: "mar-at-open",
		At:       time.Date(2026, 3, 10, 20, 0, 0, 0, zonePlus8),
		Quantity: 8,
	}
	if !zoned.At.Equal(reopenAt) {
		t.Fatalf("test setup: zoned instant %v != %v", zoned.At.UTC(), reopenAt)
	}
	if rr, err := s.RecordEvent(zoned); err != nil || rr.Accepted || rr.Period != mar(2026) {
		t.Fatalf("zoned replay of reopen instant: %+v %v", rr, err)
	}
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 8 {
		t.Fatalf("march usage after zoned replay = %d, want 8", u.Total)
	}
}
