package meter

import (
	"errors"
	"testing"
)

// 本文件回归“事件标识只在所属账户内去重”与欠费停用拒收交叉时的账户独立性。
//
// 同一服务中的两个账户各自持有独立的事件记录与用量累计：一个账户因到期
// 欠费被停用、另一个账户正常使用时，即使两者复用同一个全新事件标识，也必须
// 按各自账户的欠费状态与接收记录分别裁决：
//   - 停用账户的新事件返回 ErrSuspended，拒收不消耗事件标识（恢复后以原内容
//     提交仍是首次接收），也不增加任何账期用量；
//   - 正常账户的事件必须作为首次上报接收（Accepted=true），既不能把停用
//     账户的拒收当成自己的重报，也不能反过来让停用账户命中正常账户的接收
//     记录而返回 ErrEventConflict；
//   - 停用账户结清恢复后，用同一标识、同一发生时刻与数量补报此前被拒事件，
//     即使另一账户已用该标识接收了数量不同的事件，本账户仍作为首次接收成功，
//     不发生跨账户冲突；
//   - 此后两账户各自原样重报自己已接收的事件，都成功但 Accepted=false，
//     归属账期不变且不再累计；
//   - 为停用账户登记的付款只结清该账户自己的到期账单，另一账户的余额、
//     付清状态与用量均不受影响。
//
// 场景：账户甲（a）、乙（b）都自 2026-01-01 00:00 UTC 起持续订阅同一套餐
// （月费 1000 分，超额单价与税率均为零）。一月账单各 1000 分，截止时刻均为
// 2026-02-08 00:00 UTC；乙已在二月初结清，甲分文未付。当前时刻恰为截止时刻：
// 两份订阅都有效，仅甲欠费停用；二月均未出账、无二月用量。两者随后以同一个
// 新标识上报发生在 2026-02-07 00:00 UTC 的用量（甲 3 单位、乙 7 单位）。

// suspendedEventPairFixture 构造上述两个账户：一月账单均已生成、乙已结清、
// 甲仍欠 1000 分，时钟停在 2026-02-08 00:00 UTC（恰好到达付款截止时刻）。
func suspendedEventPairFixture(t *testing.T) *Service {
	t.Helper()
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))
	mustPlan(t, s, Plan{ID: "p", MonthlyFee: 1000, IncludedUnits: 0, OveragePrice: 0, TaxRateBasisPoints: 0})
	mustAccount(t, s, "a")
	mustAccount(t, s, "b")
	mustSubscribe(t, s, "a", "p", utc(2026, 1, 1, 0, 0))
	mustSubscribe(t, s, "b", "p", utc(2026, 1, 1, 0, 0))

	// 二月初一月账期已结束：为两账户出账，乙当场结清，甲不付。
	clk.t = utc(2026, 2, 1, 0, 0)
	ba := mustBill(t, s, "a", jan(2026))
	if ba.TotalDue != 1000 || ba.Balance != 1000 || ba.Settled {
		t.Fatalf("a jan bill initial: %+v", ba)
	}
	bb := mustBill(t, s, "b", jan(2026))
	if bb.TotalDue != 1000 || bb.Balance != 1000 || bb.Settled {
		t.Fatalf("b jan bill initial: %+v", bb)
	}
	pr, err := s.RecordPayment("b", "pay-b-jan", jan(2026), 1000)
	if err != nil || !pr.Registered || pr.BillBalance != 0 || !pr.Settled {
		t.Fatalf("b pay jan bill: %+v %v", pr, err)
	}

	// 恰好到达一月账单付款截止时刻。
	clk.t = utc(2026, 2, 8, 0, 0)
	return s
}

// 两账户复用的事件：同一标识、同一发生时刻（2026-02-07 00:00 UTC），数量不同。
func sharedPairEvents() (Event, Event) {
	at := utc(2026, 2, 7, 0, 0)
	return Event{AccountID: "a", EventID: "evt-shared", At: at, Quantity: 3},
		Event{AccountID: "b", EventID: "evt-shared", At: at, Quantity: 7}
}

// assertPairStartingPoint 校验截止时刻的起点状态：两份订阅都有效，仅甲停用；
// 一月账单甲欠 1000、乙已结清；两账户都没有一、二月用量，二月均未出账。
func assertPairStartingPoint(t *testing.T, s *Service) {
	t.Helper()
	wantAccountFlags(t, s, "a", true)
	wantAccountFlags(t, s, "b", false)

	ba, err := s.GetBill("a", jan(2026))
	if err != nil || ba.TotalDue != 1000 || ba.Paid != 0 || ba.Balance != 1000 || ba.Settled {
		t.Fatalf("a jan bill at due time: %+v %v", ba, err)
	}
	if !ba.DueAt.Equal(utc(2026, 2, 8, 0, 0)) {
		t.Fatalf("a jan due at = %v, want 2026-02-08 00:00", ba.DueAt)
	}
	bs := statusBillSummary(t, mustStatus(t, s, "b"), jan(2026))
	if bs.TotalDue != 1000 || bs.Paid != 1000 || bs.Balance != 0 || !bs.Settled {
		t.Fatalf("b jan summary at due time: %+v, want paid 1000 settled", bs)
	}

	for _, acct := range []string{"a", "b"} {
		if u, err := s.MonthlyUsage(acct, jan(2026)); err != nil || u.Total != 0 {
			t.Fatalf("%s jan usage = %d: %v", acct, u.Total, err)
		}
		if u, err := s.MonthlyUsage(acct, feb(2026)); err != nil || u.Total != 0 {
			t.Fatalf("%s feb usage before events = %d: %v", acct, u.Total, err)
		}
		// 二月均尚未出账。
		if _, err := s.GetBill(acct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("%s feb bill = %v, want ErrBillNotFound", acct, err)
		}
	}
}

// mustStatus 返回账户状态，失败即终止。
func mustStatus(t *testing.T, s *Service, acct string) AccountStatus {
	t.Helper()
	st, err := s.Status(acct)
	if err != nil {
		t.Fatalf("status %q: %v", acct, err)
	}
	return st
}

// assertNoMonthUsage 断言指定账期没有任何用量：MonthlyUsage 为 0，且 Status
// 的用量列表中该账期只能缺失或同样为 0（零用量且未出账的月份不会被列出）。
func assertNoMonthUsage(t *testing.T, s *Service, acct string, m Month) {
	t.Helper()
	if u, err := s.MonthlyUsage(acct, m); err != nil || u.Total != 0 {
		t.Fatalf("%s %s monthly usage = %d: %v, want 0", acct, m, u.Total, err)
	}
	st := mustStatus(t, s, acct)
	for _, u := range st.MonthlyUsage {
		if u.Period == m && u.Total != 0 {
			t.Fatalf("%s %s status usage = %d, want 0 or absent", acct, m, u.Total)
		}
	}
}

// submitRejectedForA 以甲的身份提交共享事件，必须返回 ErrSuspended
// （而不是因乙已接收同标识事件而返回 ErrEventConflict），且不增加任何用量。
func submitRejectedForA(t *testing.T, s *Service, ea Event) {
	t.Helper()
	if _, err := s.RecordEvent(ea); !errors.Is(err, ErrSuspended) {
		t.Fatalf("a event while suspended: %v, want ErrSuspended", err)
	}
	assertNoMonthUsage(t, s, "a", feb(2026))
}

// submitAcceptedForB 以乙的身份提交共享事件，必须作为首次上报接收，
// 不能因甲使用同一标识被拒而被判成重报或冲突。
func submitAcceptedForB(t *testing.T, s *Service, eb Event) {
	t.Helper()
	r, err := s.RecordEvent(eb)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("b event accepted as first: r=%+v err=%v", r, err)
	}
	assertMonthTotal(t, s, "b", feb(2026), 7)
}

// TestEventIDReusedAcrossAccountsSuspension 是主回归：无论先提交哪个账户，
// 甲被停用拒收、乙首次接收的结果都相同；甲付款恢复后以原内容补报成功，
// 两账户随后各自的原样重报都只返回 Accepted=false。
func TestEventIDReusedAcrossAccountsSuspension(t *testing.T) {
	ea, eb := sharedPairEvents()

	// 两种提交次序都跑一遍：被拒与接收互不影响，结果与先后无关。
	for _, tc := range []struct {
		name  string
		first string // 先提交共享事件的账户
	}{
		{name: "suspended-a-first", first: "a"},
		{name: "active-b-first", first: "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := suspendedEventPairFixture(t)
			assertPairStartingPoint(t, s)

			// 第一个账户按各自状态裁决。
			if tc.first == "a" {
				submitRejectedForA(t, s, ea)
			} else {
				submitAcceptedForB(t, s, eb)
			}
			// 第二个账户同样只看自己的记录：甲不命中乙的接收、乙不继承甲的拒收。
			if tc.first == "a" {
				submitAcceptedForB(t, s, eb)
			} else {
				submitRejectedForA(t, s, ea)
			}

			// 首轮结束：甲拒收未占用量，乙首次接收累计 7。
			assertNoMonthUsage(t, s, "a", feb(2026))
			assertMonthTotal(t, s, "b", feb(2026), 7)
			wantAccountFlags(t, s, "a", true)
			wantAccountFlags(t, s, "b", false)

			// 恢复前再次提交甲的事件仍是停用拒收（再次证明前次拒收未占用标识，
			// 且乙账户里的同标识记录不会让甲得到重报或冲突结果）。
			submitRejectedForA(t, s, ea)
			assertNoMonthUsage(t, s, "a", feb(2026))
			assertMonthTotal(t, s, "b", feb(2026), 7)

			// 为甲登记 1000 分本地付款，只结清甲自己的一月账单：甲立即恢复。
			pr, err := s.RecordPayment("a", "pay-a-jan", jan(2026), 1000)
			if err != nil || !pr.Registered || pr.BillBalance != 0 || !pr.Settled {
				t.Fatalf("a pay jan bill: %+v %v", pr, err)
			}
			wantAccountFlags(t, s, "a", false)
			wantAccountFlags(t, s, "b", false)

			// 甲以原标识、原发生时刻、原数量补报刚才被拒的事件：首次接收，
			// 归入二月并累计 3。乙已用相同标识接收数量 7，不得让甲冲突。
			r, err := s.RecordEvent(ea)
			if err != nil || !r.Accepted || r.Period != feb(2026) {
				t.Fatalf("a resubmit after recovery: r=%+v err=%v", r, err)
			}
			assertMonthTotal(t, s, "a", feb(2026), 3)
			assertMonthTotal(t, s, "b", feb(2026), 7)

			// 分别原样重报两账户各自已接收的事件：都成功但 Accepted=false，
			// 归属仍为二月，不再累计。
			if ra, err := s.RecordEvent(ea); err != nil || ra.Accepted || ra.Period != feb(2026) {
				t.Fatalf("a identical replay: r=%+v err=%v", ra, err)
			}
			if rb, err := s.RecordEvent(eb); err != nil || rb.Accepted || rb.Period != feb(2026) {
				t.Fatalf("b identical replay: r=%+v err=%v", rb, err)
			}
			assertPairFinalState(t, s)
		})
	}
}

// assertPairFinalState 校验全部交互结束后两账户各自独立的最终记账结果：
// 二月累计甲 3、乙 7（MonthlyUsage 与 Status 两处一致）；一月账单均为
// 已付 1000、余额 0、已结清；两账户订阅均有效、均未停用；二月仍未出账；
// 一月没有用量。
func assertPairFinalState(t *testing.T, s *Service) {
	t.Helper()
	assertMonthTotal(t, s, "a", feb(2026), 3)
	assertMonthTotal(t, s, "b", feb(2026), 7)
	for _, acct := range []string{"a", "b"} {
		if u, err := s.MonthlyUsage(acct, jan(2026)); err != nil || u.Total != 0 {
			t.Fatalf("%s jan usage changed: %d", acct, u.Total)
		}
		st := mustStatus(t, s, acct)
		if !st.Subscribed || st.Suspended {
			t.Fatalf("%s final flags: subscribed=%v suspended=%v", acct, st.Subscribed, st.Suspended)
		}
		summary := statusBillSummary(t, st, jan(2026))
		if summary.TotalDue != 1000 || summary.Paid != 1000 || summary.Balance != 0 || !summary.Settled {
			t.Fatalf("%s jan final summary = %+v, want paid 1000 settled", acct, summary)
		}
		bill, err := s.GetBill(acct, jan(2026))
		if err != nil || bill.Paid != 1000 || bill.Balance != 0 || !bill.Settled {
			t.Fatalf("%s jan final bill = %+v %v", acct, bill, err)
		}
		// 付款与用量操作都不应凭空产生二月账单。
		if _, err := s.GetBill(acct, feb(2026)); !errors.Is(err, ErrBillNotFound) {
			t.Fatalf("%s feb bill = %v, want ErrBillNotFound", acct, err)
		}
	}
}

// TestSuspendedRejectionDoesNotShadowOtherAccount 锁定一个更窄的跨账户边界：
// 乙先以共享标识成功接收后，甲（停用中）以同标识、不同数量提交，必须按甲
// 自身欠费状态返回 ErrSuspended，而不是按账户内冲突返回 ErrEventConflict；
// 甲恢复后以自己的数量首次接收，乙的记录与累计保持不变。
func TestSuspendedRejectionDoesNotShadowOtherAccount(t *testing.T) {
	s := suspendedEventPairFixture(t)
	ea, eb := sharedPairEvents()

	// 乙先成功接收 7。
	submitAcceptedForB(t, s, eb)

	// 甲以同标识、数量 3 提交：停用拒收优先，且不是事件冲突。
	if _, err := s.RecordEvent(ea); !errors.Is(err, ErrSuspended) {
		t.Fatalf("a after b accepted: %v, want ErrSuspended (not ErrEventConflict)", err)
	}

	// 甲恢复并补报：按甲账户内的首次接收处理，与乙的数量 7 无关。
	if _, err := s.RecordPayment("a", "pay-a-jan", jan(2026), 1000); err != nil {
		t.Fatalf("a payment: %v", err)
	}
	r, err := s.RecordEvent(ea)
	if err != nil || !r.Accepted || r.Period != feb(2026) {
		t.Fatalf("a first acceptance despite b's record: r=%+v err=%v", r, err)
	}

	// 反过来，乙以甲的数量 3 重报同标识：乙账户内该标识数量是 7，
	// 仍按乙自己的记录判冲突，与甲刚接收的 3 无关。
	ebWrongQty := eb
	ebWrongQty.Quantity = ea.Quantity
	if _, err := s.RecordEvent(ebWrongQty); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("b replay with a's quantity: %v, want ErrEventConflict", err)
	}
	// 甲以乙的数量 7 提交同标识：甲账户内记录数量是 3，同样按甲自己的冲突裁决。
	eaWrongQty := ea
	eaWrongQty.Quantity = eb.Quantity
	if _, err := s.RecordEvent(eaWrongQty); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("a replay with b's quantity: %v, want ErrEventConflict", err)
	}
	assertPairFinalState(t, s)
}
