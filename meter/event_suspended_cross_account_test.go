package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为用量接收补充跨账户自动回归保障：同一服务中一个账户因到期欠费被
// 停用、另一个账户正常使用时，即使两个账户复用同一个事件标识，也必须严格
// 按各自账户的欠费状态与接收记录分别裁决。
//
// 事件标识与付款标识一样只在“所属账户内”去重：
//   - 甲因一月账单到期未结清被停用，其新事件返回 ErrSuspended；拒收不占用
//     事件标识、不累计用量，也不会在乙的账户里制造出一条“已接收记录”。
//   - 乙已结清、订阅有效，用同一标识（数量不同）上报必须作为乙的首次上报
//     成功接收，既不能因为甲刚被同一标识拒绝而报冲突，也不能被当成重报。
//   - 乙接收成功同样不影响甲：甲仍处于停用状态时原样再报仍是 ErrSuspended，
//     而不是命中乙记录后的 ErrEventConflict 或 Accepted=false 重报。
//   - 甲结清一月账单解除停用后，以原标识、原发生时刻、原数量提交此前被拒
//     的事件，应按首次接收处理；乙已用同一标识（数量不同）成功接收，不能
//     让甲返回事件冲突。
//
// 场景：账户甲、乙都自 2026-01-01 00:00 UTC 起持续订阅同一套餐（月费
// 1000 分，超额单价与税率均为零，无其他欠款）。一月账单均已生成，金额
// 都是 1000 分、付款截止时刻都是 2026-02-08 00:00 UTC；乙已结清，甲仍欠
// 1000 分。时钟恰好停在 2026-02-08 00:00（截止时刻本身即到期），二月均
// 尚未出账、此前没有二月用量。

const (
	crossAcctJia = "jia"
	crossAcctYi  = "yi"
	// 两账户复用的同一事件标识。
	crossAcctEventID = "evt-shared-feb"
)

var (
	crossAcctDueAt     time.Time = utc(2026, 2, 8, 0, 0)
	crossAcctEventAt   time.Time = utc(2026, 2, 7, 0, 0)
	crossAcctSubscribe time.Time = utc(2026, 1, 1, 0, 0)
)

// suspendedCrossAccountFixture 建立甲、乙两个账户及其一月账单：
// 时钟拨到 2026-02-08 00:00（恰好到达一月账单付款截止时刻）后为两账户
// 各出一张 1000 分的一月账单，随后乙全额结清、甲分文未付。结束时甲欠费
// 停用、乙正常，两者订阅都仍有效，二月均未出账、无用量。
func suspendedCrossAccountFixture(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	s, clk := newTestService(crossAcctSubscribe)
	mustPlan(t, s, Plan{ID: "plan1000", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, crossAcctJia)
	mustAccount(t, s, crossAcctYi)
	if err := s.Subscribe(crossAcctJia, "plan1000", crossAcctSubscribe); err != nil {
		t.Fatalf("subscribe jia: %v", err)
	}
	if err := s.Subscribe(crossAcctYi, "plan1000", crossAcctSubscribe); err != nil {
		t.Fatalf("subscribe yi: %v", err)
	}

	clk.t = crossAcctDueAt
	for _, acct := range []string{crossAcctJia, crossAcctYi} {
		b, err := s.CreateBill(acct, jan(2026))
		if err != nil {
			t.Fatalf("create bill %q: %v", acct, err)
		}
		if b.TotalDue != 1000 || b.Paid != 0 || b.Balance != 1000 || b.Settled || b.Tax != 0 {
			t.Fatalf("bill %q initial state: %+v", acct, b)
		}
		if !b.DueAt.Equal(crossAcctDueAt) {
			t.Fatalf("bill %q due at = %v, want %v", acct, b.DueAt, crossAcctDueAt)
		}
	}

	// 乙全额结清一月账单：截止时刻已到也不会停用；甲仍欠 1000 分。
	r, err := s.RecordPayment(crossAcctYi, "pay-yi-jan", jan(2026), 1000)
	if err != nil || !r.Registered || r.BillBalance != 0 || !r.Settled {
		t.Fatalf("yi payment: r=%+v err=%v", r, err)
	}
	return s, clk
}

// crossAcctJiaEvent 是甲被拒、随后又在结清后首次接收的那条二月事件（3 单位）。
func crossAcctJiaEvent() Event {
	return Event{AccountID: crossAcctJia, EventID: crossAcctEventID, At: crossAcctEventAt, Quantity: 3}
}

// crossAcctYiEvent 是乙用同一标识首次接收的二月事件（7 单位，数量与甲不同）。
func crossAcctYiEvent() Event {
	return Event{AccountID: crossAcctYi, EventID: crossAcctEventID, At: crossAcctEventAt, Quantity: 7}
}

// wantCrossAccountFlags 校验订阅有效性与欠费停用状态彼此独立：两账户订阅
// 始终有效且当前套餐条件不变，停用与否只取决于各自的到期账单。
func wantCrossAccountFlags(t *testing.T, s *Service, jiaSuspended, yiSuspended bool) {
	t.Helper()
	for _, w := range []struct {
		acct      string
		suspended bool
	}{
		{crossAcctJia, jiaSuspended},
		{crossAcctYi, yiSuspended},
	} {
		st, err := s.Status(w.acct)
		if err != nil {
			t.Fatalf("status %q: %v", w.acct, err)
		}
		if st.Suspended != w.suspended {
			t.Fatalf("account %q suspended = %v, want %v; bills: %+v", w.acct, st.Suspended, w.suspended, st.Bills)
		}
		if !st.Subscribed {
			t.Fatalf("account %q subscription must remain active: %+v", w.acct, st)
		}
		if st.CurrentTerms.PlanID != "plan1000" || st.CurrentTerms.MonthlyFee != 1000 {
			t.Fatalf("account %q current terms changed: %+v", w.acct, st.CurrentTerms)
		}
	}
}

// wantCrossAccountFebUsage 同时校验两账户的二月累计用量（MonthlyUsage）。
func wantCrossAccountFebUsage(t *testing.T, s *Service, jiaTotal, yiTotal int64) {
	t.Helper()
	for _, w := range []struct {
		acct  string
		total int64
	}{
		{crossAcctJia, jiaTotal},
		{crossAcctYi, yiTotal},
	} {
		u, err := s.MonthlyUsage(w.acct, feb(2026))
		if err != nil {
			t.Fatalf("monthly usage %q: %v", w.acct, err)
		}
		if u.Period != feb(2026) || u.Total != w.total {
			t.Fatalf("account %q feb usage = %+v, want total %d", w.acct, u, w.total)
		}
	}
}

// wantCrossAccountJanBill 校验一个账户一月账单的当前付款状态，以及出账时即
// 固定的应付金额、用量明细与截止时刻不被任何跨账户操作改变。
func wantCrossAccountJanBill(t *testing.T, s *Service, acct string, paid, balance int64, settled bool) {
	t.Helper()
	b, err := s.GetBill(acct, jan(2026))
	if err != nil {
		t.Fatalf("get bill %q: %v", acct, err)
	}
	if b.TotalDue != 1000 || b.Paid != paid || b.Balance != balance || b.Settled != settled {
		t.Fatalf("bill %q state: due=%d paid=%d balance=%d settled=%v, want paid=%d balance=%d settled=%v",
			acct, b.TotalDue, b.Paid, b.Balance, b.Settled, paid, balance, settled)
	}
	if b.MonthlyFee != 1000 || b.OverageFee != 0 || b.Tax != 0 || b.TotalUsage != 0 {
		t.Fatalf("bill %q fixed detail changed: %+v", acct, b)
	}
	if !b.DueAt.Equal(crossAcctDueAt) {
		t.Fatalf("bill %q due at = %v, want %v", acct, b.DueAt, crossAcctDueAt)
	}
}

// runCrossAccountScenario 按给定的首报顺序跑完整场景。jiaFirst 为 true 时
// 先提交甲（被拒）再提交乙（接收），否则反过来：两种顺序结果必须一致。
func runCrossAccountScenario(t *testing.T, jiaFirst bool) {
	s, clk := suspendedCrossAccountFixture(t)

	// 起点：两账户订阅都有效；只有甲欠费停用，一月账单余额分别为 1000 与 0；
	// 二月均无用量，且二月尚未出账。
	wantCrossAccountFlags(t, s, true, false)
	wantCrossAccountJanBill(t, s, crossAcctJia, 0, 1000, false)
	wantCrossAccountJanBill(t, s, crossAcctYi, 1000, 0, true)
	wantCrossAccountFebUsage(t, s, 0, 0)
	for _, acct := range []string{crossAcctJia, crossAcctYi} {
		if _, err := s.GetBill(acct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("account %q feb bill should not exist: %v", acct, err)
		}
	}

	jiaEvent := crossAcctJiaEvent()
	yiEvent := crossAcctYiEvent()

	// 按指定顺序提交甲、乙使用同一标识的二月事件。
	first, second := jiaEvent, yiEvent
	if !jiaFirst {
		first, second = yiEvent, jiaEvent
	}
	submitFirst := func() {
		if first.AccountID == crossAcctJia {
			if _, err := s.RecordEvent(first); !errors.Is(err, ErrSuspended) {
				t.Fatalf("jia event must be rejected with ErrSuspended: %v", err)
			}
		} else {
			r, err := s.RecordEvent(first)
			if err != nil || !r.Accepted || r.Period != feb(2026) {
				t.Fatalf("yi event must be first accepted: r=%+v err=%v", r, err)
			}
		}
	}
	submitSecond := func() {
		if second.AccountID == crossAcctJia {
			if _, err := s.RecordEvent(second); !errors.Is(err, ErrSuspended) {
				t.Fatalf("jia event must be rejected with ErrSuspended regardless of order: %v", err)
			}
		} else {
			r, err := s.RecordEvent(second)
			if err != nil || !r.Accepted || r.Period != feb(2026) {
				t.Fatalf("yi event must be first accepted regardless of order: r=%+v err=%v", r, err)
			}
		}
	}
	submitFirst()
	submitSecond()

	// 甲的拒收不累计：二月累计仍为零；乙首次接收 7 单位，归入 2026-02。
	wantCrossAccountFebUsage(t, s, 0, 7)
	// 停用状态不因上报改变：甲仍停用、乙正常。
	wantCrossAccountFlags(t, s, true, false)

	// 甲仍停用期间原样再报：必须仍是 ErrSuspended。乙的同标识成功记录不能
	// 泄漏到甲的账户里——既不能按乙的数量判 ErrEventConflict，也不能按
	// 重报返回 Accepted=false。
	if _, err := s.RecordEvent(jiaEvent); !errors.Is(err, ErrSuspended) {
		t.Fatalf("jia retry while still suspended: %v", err)
	}
	// 乙原样重报自己的事件：按乙自己的接收记录幂等返回（Accepted=false），
	// 不再次累计，也不改变甲仍被停用的事实。
	if r, err := s.RecordEvent(yiEvent); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("yi identical replay: r=%+v err=%v", r, err)
	}
	wantCrossAccountFebUsage(t, s, 0, 7)
	wantCrossAccountFlags(t, s, true, false)

	// 为甲登记 1000 分本地付款，结清甲的一月账单：只影响甲自己。
	pay, err := s.RecordPayment(crossAcctJia, "pay-jia-jan", jan(2026), 1000)
	if err != nil || !pay.Registered || pay.BillBalance != 0 || !pay.Settled {
		t.Fatalf("jia payment: r=%+v err=%v", pay, err)
	}
	// 甲解除欠费停用，乙本来就未停用；两账户订阅都仍有效。
	wantCrossAccountFlags(t, s, false, false)
	// 付款只结清甲的一月账单；乙的余额、付清状态保持原样，不被甲的付款影响。
	wantCrossAccountJanBill(t, s, crossAcctJia, 1000, 0, true)
	wantCrossAccountJanBill(t, s, crossAcctYi, 1000, 0, true)

	// 甲以原标识、原发生时刻、原数量提交刚才被拒的事件：拒收没有占用标识，
	// 本次作为首次接收成功，归入 2026-02。乙虽已用同一标识但数量不同，
	// 去重域是账户本身，不能因此对甲返回 ErrEventConflict。
	r, err := s.RecordEvent(jiaEvent)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("jia event accepted after payment: r=%+v err=%v", r, err)
	}
	// 甲二月累计变为 3（自己的数量），乙的 7 不因甲的成功而变化。
	wantCrossAccountFebUsage(t, s, 3, 7)

	// 甲的首次接收也不能影响乙：乙再原样重报仍是自己记录的幂等返回。
	if r, err := s.RecordEvent(yiEvent); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("yi replay after jia accepted: r=%+v err=%v", r, err)
	}
	wantCrossAccountFebUsage(t, s, 3, 7)

	// 分别原样重报两者各自成功接收的事件：都成功但 Accepted=false，归属仍为
	// 2026-02，不再累计。
	if r, err := s.RecordEvent(jiaEvent); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("jia final replay: r=%+v err=%v", r, err)
	}
	if r, err := s.RecordEvent(yiEvent); err != nil || r.Accepted || r.Period != feb(2026) {
		t.Fatalf("yi final replay: r=%+v err=%v", r, err)
	}
	wantCrossAccountFebUsage(t, s, 3, 7)

	// 账户状态中的二月用量同样分别为 3 与 7；Status 的账单摘要也各自独立。
	for _, w := range []struct {
		acct       string
		febTotal   int64
		janBalance int64
		janSettled bool
	}{
		{crossAcctJia, 3, 0, true},
		{crossAcctYi, 7, 0, true},
	} {
		st, err := s.Status(w.acct)
		if err != nil {
			t.Fatalf("status %q: %v", w.acct, err)
		}
		if got := statusUsageTotal(t, st, feb(2026)); got != w.febTotal {
			t.Fatalf("account %q status feb usage = %d, want %d", w.acct, got, w.febTotal)
		}
		if len(st.Bills) != 1 {
			t.Fatalf("account %q status bills = %+v", w.acct, st.Bills)
		}
		bs := statusBillSummary(t, st, jan(2026))
		if bs.TotalDue != 1000 || bs.Balance != w.janBalance || bs.Settled != w.janSettled {
			t.Fatalf("account %q status jan bill = %+v", w.acct, bs)
		}
		if !bs.DueAt.Equal(crossAcctDueAt) {
			t.Fatalf("account %q jan due at = %v, want %v", w.acct, bs.DueAt, crossAcctDueAt)
		}
	}

	// 时钟没有被任何操作推动：截止时刻语义始终按注入时钟裁决。
	if !clk.t.Equal(crossAcctDueAt) {
		t.Fatalf("clock moved unexpectedly: %v", clk.t)
	}
	// 二月始终未出账；两账户一月账单保持各自的付款结果。
	for _, acct := range []string{crossAcctJia, crossAcctYi} {
		if _, err := s.GetBill(acct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("account %q feb bill should still not exist: %v", acct, err)
		}
	}
	wantCrossAccountJanBill(t, s, crossAcctJia, 1000, 0, true)
	wantCrossAccountJanBill(t, s, crossAcctYi, 1000, 0, true)
	wantCrossAccountFlags(t, s, false, false)
}

// TestSuspendedAndActiveAccountsSharedEventID 覆盖完整场景，并通过两种提交
// 顺序保证结果与“先提交哪个账户”无关。
func TestSuspendedAndActiveAccountsSharedEventID(t *testing.T) {
	t.Run("jia rejected first", func(t *testing.T) { runCrossAccountScenario(t, true) })
	t.Run("yi accepted first", func(t *testing.T) { runCrossAccountScenario(t, false) })
}

// TestSharedEventIDAcrossSuspendedAccountsConcurrent 并发版本：甲停用、乙正常
// 时两者在同一瞬间用同一标识首次上报。每个账户的“去重判定 → 校验 → 落库”
// 都在各自账户的临界区裁决，甲必须 ErrSuspended 且不留记录，乙必须首次
// 接收成功；不能因为并发碰撞把任一方判成跨账户冲突或重报。
func TestSharedEventIDAcrossSuspendedAccountsConcurrent(t *testing.T) {
	const rounds = 50
	for iter := 0; iter < rounds; iter++ {
		s, _ := suspendedCrossAccountFixture(t)

		type outcome struct {
			r   EventResult
			err error
		}
		resJia := make(chan outcome, 1)
		resYi := make(chan outcome, 1)
		start := make(chan struct{})
		go func() {
			<-start
			r, err := s.RecordEvent(crossAcctJiaEvent())
			resJia <- outcome{r, err}
		}()
		go func() {
			<-start
			r, err := s.RecordEvent(crossAcctYiEvent())
			resYi <- outcome{r, err}
		}()
		close(start)
		oj := <-resJia
		oy := <-resYi

		if !errors.Is(oj.err, ErrSuspended) {
			t.Fatalf("iter %d concurrent jia: r=%+v err=%v, want ErrSuspended", iter, oj.r, oj.err)
		}
		if oy.err != nil || !oy.r.Accepted || oy.r.Period != feb(2026) {
			t.Fatalf("iter %d concurrent yi: r=%+v err=%v, want first accepted into 2026-02", iter, oy.r, oy.err)
		}
		wantCrossAccountFebUsage(t, s, 0, 7)
		wantCrossAccountFlags(t, s, true, false)
	}
}

// TestSharedEventIDAcceptedConcurrentlyAfterPayment 并发版本的恢复段：甲先
// 结清一月账单解除停用，随后两者并发地用同一标识首次上报（数量不同）。
// 两笔都必须在各自账户内作为首次上报接收，互不冲突、互不累计到对方。
func TestSharedEventIDAcceptedConcurrentlyAfterPayment(t *testing.T) {
	const rounds = 50
	for iter := 0; iter < rounds; iter++ {
		s, _ := suspendedCrossAccountFixture(t)
		if r, err := s.RecordPayment(crossAcctJia, "pay-jia-jan", jan(2026), 1000); err != nil ||
			!r.Registered || r.BillBalance != 0 || !r.Settled {
			t.Fatalf("iter %d jia payment: r=%+v err=%v", iter, r, err)
		}
		wantCrossAccountFlags(t, s, false, false)

		type outcome struct {
			r   EventResult
			err error
		}
		resJia := make(chan outcome, 1)
		resYi := make(chan outcome, 1)
		start := make(chan struct{})
		go func() {
			<-start
			r, err := s.RecordEvent(crossAcctJiaEvent())
			resJia <- outcome{r, err}
		}()
		go func() {
			<-start
			r, err := s.RecordEvent(crossAcctYiEvent())
			resYi <- outcome{r, err}
		}()
		close(start)
		oj := <-resJia
		oy := <-resYi

		if oj.err != nil || !oj.r.Accepted || oj.r.Period != feb(2026) {
			t.Fatalf("iter %d concurrent jia after payment: r=%+v err=%v", iter, oj.r, oj.err)
		}
		if oy.err != nil || !oy.r.Accepted || oy.r.Period != feb(2026) {
			t.Fatalf("iter %d concurrent yi after jia payment: r=%+v err=%v", iter, oy.r, oy.err)
		}
		wantCrossAccountFebUsage(t, s, 3, 7)
		wantCrossAccountJanBill(t, s, crossAcctJia, 1000, 0, true)
		wantCrossAccountJanBill(t, s, crossAcctYi, 1000, 0, true)
		wantCrossAccountFlags(t, s, false, false)

		// 再并发原样重报：两者都只命中各自的接收记录，Accepted=false，不累计。
		rj2 := make(chan outcome, 1)
		ry2 := make(chan outcome, 1)
		start2 := make(chan struct{})
		go func() {
			<-start2
			r, err := s.RecordEvent(crossAcctJiaEvent())
			rj2 <- outcome{r, err}
		}()
		go func() {
			<-start2
			r, err := s.RecordEvent(crossAcctYiEvent())
			ry2 <- outcome{r, err}
		}()
		close(start2)
		oj2 := <-rj2
		oy2 := <-ry2
		if oj2.err != nil || oj2.r.Accepted || oj2.r.Period != feb(2026) {
			t.Fatalf("iter %d concurrent jia replay: r=%+v err=%v", iter, oj2.r, oj2.err)
		}
		if oy2.err != nil || oy2.r.Accepted || oy2.r.Period != feb(2026) {
			t.Fatalf("iter %d concurrent yi replay: r=%+v err=%v", iter, oy2.r, oy2.err)
		}
		wantCrossAccountFebUsage(t, s, 3, 7)
	}
}
