package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“当前账期预计费用查询（EstimateCurrentBill）在取消订阅后提前登记
// 重新开通”这一场景提供回归保障：能否返回预估只取决于当前时刻是否已有实际生效
// 的订阅，预估只采用当前订阅开通时保存的当月套餐条件快照。账户里留着的旧订阅
// 段、旧账期用量、旧账单与付款记录都不能让等待期间的查询沿用旧套餐提前成功，
// 等待期间修改的套餐定义也只能影响以后新保存的条件。
//
// 时间线（全部 UTC）：
//
//	2026-01-01 开通旧套餐 plan-old：月费 1000、包含 10、超额单价 100、税率 10%。
//	2026-01-10 一月累计用量 15；2026-01-15 登记按月取消，2026-02-01 00:00 终止。
//	2026-02-01 终止时刻到达：一月账期恰好结束，正式出账应付 1650，当日本地全额
//	           付款结清；账户此后没有生效订阅，但保留一月的订阅段、用量、账单与
//	           付款记录。
//	2026-02-15 提前登记 2026-03-20 12:00 重新开通另一套餐 plan-new，登记保存
//	           当时定义：月费 2000、包含 20、超额单价 200、税率 6%。
//	           登记后把 plan-new 当前定义改为 5000/5/900/25%：只影响以后新保存
//	           的条件，本次重新开通锁定的快照不变。
//	等待开通期间（二月中、进入三月的 2026-03-01 00:00、开通当天 2026-03-20
//	00:00 与开通前一分钟）预估一律返回 ErrSubscriptionNotActivated：不返回旧套餐
//	           的 1650 预估，失败的查询也不删除提前登记的订阅。
//	2026-03-20 12:00 恰好到达登记时刻：直接预估即成功，无需先上报用量或正式出账；
//	           结果明确为预估、账期为三月，四项条件均为登记时保存的 2000/20/200/
//	           600，三月尚无用量，完整月费 2000、税 120、预计应付 2120；一月的
//	           15 单位不带入三月。
//	2026-03-21 新订阅生效后接收三月用量 25：累计 25、超额 5、超额费 1000、
//	           税 180、预计应付 3180。预估不生成正式账单、不关闭三月，也不改写
//	           一月账单的金额与付款状态；一月的历史用量、账单查询与付款重报仍按
//	           原规则可用，二月空档仍不可出账。

// TestEstimateAfterCancelThenFutureResubscribeUsesSavedTerms 端到端回归上述主线。
func TestEstimateAfterCancelThenFutureResubscribeUsesSavedTerms(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))

	oldDef := planDef("plan-old", 1000, 10, 100, 1000)
	newDefAtRegister := planDef("plan-new", 2000, 20, 200, 600)
	newDefUpdated := planDef("plan-new", 5000, 5, 900, 2500)
	oldTerms := termsOf(oldDef)
	newSavedTerms := termsOf(newDefAtRegister)
	updatedTerms := termsOf(newDefUpdated)

	mustPlan(t, s, oldDef)
	mustPlan(t, s, newDefAtRegister)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "plan-old", utc(2026, 1, 1, 0, 0))

	// 一月用量 15：超出包含的 10，超额 5×100=500，与 10% 税合计应付 1650。
	clk.t = utc(2026, 1, 10, 0, 0)
	if r, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-jan", At: utc(2026, 1, 10, 0, 0), Quantity: 15,
	}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("january usage: %+v %v", r, err)
	}

	// 一月中旬登记按月取消：终止时刻 2026-02-01 00:00。
	clk.t = utc(2026, 1, 15, 12, 0)
	cancelRes := mustCancel(t, s, "u")
	if !cancelRes.Cancelled || !cancelRes.Cancellation.EndAt.Equal(utc(2026, 2, 1, 0, 0)) {
		t.Fatalf("cancel result = %+v, want end 2026-02-01 00:00", cancelRes)
	}

	// 2026-02-01 终止时刻到达：旧订阅移入历史，账户没有生效订阅；一月账期恰好
	// 结束，正式出账并在当日通过本地付款全额结清，避免三月接收用量时欠费停用。
	clk.t = utc(2026, 2, 1, 0, 0)
	if st, _ := s.Status("u"); st.Subscribed || st.CurrentTerms != (PlanTerms{}) {
		t.Fatalf("status at old end = %+v, want unsubscribed zero terms", st)
	}
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create january bill: %v", err)
	}
	if janBill.Terms != oldTerms || janBill.TotalUsage != 15 ||
		janBill.IncludedUnits != 10 || janBill.OverageUnits != 5 ||
		janBill.MonthlyFee != 1000 || janBill.OverageFee != 500 ||
		janBill.Tax != 150 || janBill.TotalDue != 1650 ||
		janBill.Paid != 0 || janBill.Balance != 1650 || janBill.Settled {
		t.Fatalf("january bill = %+v, want plan-old 15/5/500/150/1650 unpaid", janBill)
	}
	pay, err := s.RecordPayment("u", "pay-jan", jan(2026), 1650)
	if err != nil || !pay.Registered || pay.BillBalance != 0 || !pay.Settled {
		t.Fatalf("settle january bill: %+v %v", pay, err)
	}

	// 二月十五日提前登记三月二十日中午重新开通 plan-new：保存登记当时的完整条件。
	reopenAt := utc(2026, 3, 20, 12, 0)
	clk.t = utc(2026, 2, 15, 10, 0)
	mustSubscribe(t, s, "u", "plan-new", reopenAt)

	// 登记之后才修改 plan-new 的当前定义：只影响以后新保存的快照，本次重新开通
	// 已经锁定的 2000/20/200/600 不能被改写成 5000/5/900/2500。
	if err := s.UpdatePlan(newDefUpdated); err != nil {
		t.Fatalf("update plan-new: %v", err)
	}

	// waitingCheck 断言等待开通期间的一次预估：必须返回 ErrSubscriptionNotActivated
	// 且不输出任何（旧套餐或新套餐的）预估金额；账户状态仍为未生效；提前登记的
	// 订阅没有被失败查询删除——再次开通仍按已有订阅拒绝；空档事件不获得接收资格。
	waitingCheck := func(step string) {
		t.Helper()
		if e, err := s.EstimateCurrentBill("u"); !errors.Is(err, ErrSubscriptionNotActivated) {
			t.Fatalf("%s: estimate = %+v, %v, want ErrSubscriptionNotActivated", step, e, err)
		} else if e != (EstimatedBill{}) {
			t.Fatalf("%s: failed estimate leaked values %+v", step, e)
		}
		st, err := s.Status("u")
		if err != nil {
			t.Fatalf("%s: status: %v", step, err)
		}
		assertNotSubscribed(t, st)
		if st.Suspended {
			t.Fatalf("%s: account suspended despite settled january bill: %+v", step, st)
		}
		// 未生效期间的预估不能沿用旧套餐：状态里只有一月的历史用量与已付清账单，
		// 没有任何“当前条件”，更没有三月记录。
		if st.CurrentTerms == oldTerms {
			t.Fatalf("%s: old terms surfaced as current: %+v", step, st.CurrentTerms)
		}
		// 提前登记仍在：再次开通（任意时刻）返回 ErrSubscriptionExists，而不是
		// 新开一份或 ErrSubscriptionNotFound。
		if err := s.Subscribe("u", "plan-new", reopenAt); !errors.Is(err, ErrSubscriptionExists) {
			t.Fatalf("%s: registration lost, re-subscribe err = %v, want ErrSubscriptionExists", step, err)
		}
	}

	// 登记当天（仍在二月）：旧订阅已终止、新订阅尚未生效。
	waitingCheck("february waiting")
	// 二月底仍处空档。
	clk.t = utc(2026, 2, 28, 23, 59)
	waitingCheck("late february")
	// 已经进入三月也不能提前成功：账期翻页不等于订阅生效。
	clk.t = utc(2026, 3, 1, 0, 0)
	waitingCheck("march first midnight")
	// 三月一日空档提交一条二月/三月过去事件：不能借未来登记获得接收资格。
	if _, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-mar-gap", At: utc(2026, 3, 1, 0, 0), Quantity: 7,
	}); !errors.Is(err, ErrEventBeforeSubscription) {
		t.Fatalf("march gap event before reopen = %v, want ErrEventBeforeSubscription", err)
	}
	if u, _ := s.MonthlyUsage("u", mar(2026)); u.Total != 0 {
		t.Fatalf("march usage during waiting = %d, want 0", u.Total)
	}
	if u, _ := s.MonthlyUsage("u", feb(2026)); u.Total != 0 {
		t.Fatalf("february usage during waiting = %d, want 0", u.Total)
	}
	// 开通当天零点：未到登记的具体时刻（12:00），依旧尚未生效。
	clk.t = utc(2026, 3, 20, 0, 0)
	waitingCheck("activation day midnight")
	// 开通前一分钟仍失败。
	clk.t = reopenAt.Add(-time.Minute)
	waitingCheck("one minute before activation")

	// 恰好到达登记时刻：直接预估即成功，不依赖先上报用量或正式出账。
	clk.t = reopenAt
	e, err := s.EstimateCurrentBill("u")
	if err != nil {
		t.Fatalf("estimate exactly at reopen instant: %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "u", Estimated: true, Period: mar(2026),
		Terms: planTerms("plan-new", 2000, 20, 200, 600),
		// 三月尚无任何用量：一月累计的 15 单位不带入三月。
		TotalUsage: 0, IncludedUnits: 20, OverageUnits: 0,
		// 月中（3/20）开通仍按整月收完整月费、给完整额度，不按天折算。
		MonthlyFee: 2000, OverageFee: 0,
		// 2000 × 6% = 120，应付 2120。
		Tax: 120, EstimatedTotalDue: 2120,
	})
	if e.Terms == updatedTerms {
		t.Fatalf("estimate used updated plan definition %+v, want registered snapshot", updatedTerms)
	}

	// 新订阅生效后接收 25 单位三月用量（事件时刻不早于开通时刻）。
	clk.t = utc(2026, 3, 21, 0, 0)
	if r, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-mar", At: utc(2026, 3, 20, 12, 0), Quantity: 25,
	}); err != nil || !r.Accepted || r.Period != mar(2026) {
		t.Fatalf("march usage after reopen: %+v %v", r, err)
	}
	e, err = s.EstimateCurrentBill("u")
	if err != nil {
		t.Fatalf("estimate with march usage: %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "u", Estimated: true, Period: mar(2026),
		Terms:         planTerms("plan-new", 2000, 20, 200, 600),
		TotalUsage:    25,
		IncludedUnits: 20,
		OverageUnits:  5,
		MonthlyFee:    2000,
		// 超额 5 × 登记单价 200 = 1000，而不是修改后的 900。
		OverageFee: 1000,
		// (2000 + 1000) × 6% = 180，应付 3180。
		Tax: 180, EstimatedTotalDue: 3180,
	})

	// 预估始终是纯读：不生成三月正式账单，也不关闭三月。
	if _, err := s.GetBill("u", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("estimate created a march bill: %v", err)
	}
	if _, err := s.CreateBill("u", mar(2026)); !errors.Is(err, ErrBillMonthNotEnded) {
		t.Fatalf("march closed by estimate: %v", err)
	}

	// 一月账单的金额与付款状态不被重新开通、改价或三月预估改写。
	gotJan, err := s.GetBill("u", jan(2026))
	if err != nil {
		t.Fatalf("get january bill: %v", err)
	}
	if gotJan.Terms != janBill.Terms || gotJan.TotalUsage != janBill.TotalUsage ||
		gotJan.IncludedUnits != janBill.IncludedUnits || gotJan.OverageUnits != janBill.OverageUnits ||
		gotJan.MonthlyFee != janBill.MonthlyFee || gotJan.OverageFee != janBill.OverageFee ||
		gotJan.Tax != janBill.Tax || gotJan.TotalDue != janBill.TotalDue ||
		gotJan.Paid != 1650 || gotJan.Balance != 0 || !gotJan.Settled ||
		!gotJan.DueAt.Equal(janBill.DueAt) {
		t.Fatalf("january bill changed:\ninitial=%+v\nget    =%+v", janBill, gotJan)
	}

	// 历史用量、账单查询与付款记录仍按原规则可用：一月累计 15 保持可查，
	// 三月累计独立为 25；一月付款原样重报不再次登记，返回首次登记时的历史结果。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 15 {
		t.Fatalf("january historical usage = %d, want 15", u.Total)
	}
	replay, err := s.RecordPayment("u", "pay-jan", jan(2026), 1650)
	if err != nil {
		t.Fatalf("replay january payment: %v", err)
	}
	if replay.Registered || replay.Payment != (Payment{
		AccountID: "u", PaymentID: "pay-jan", Period: jan(2026), Amount: 1650,
	}) || replay.BillBalance != 0 || !replay.Settled {
		t.Fatalf("january payment replay = %+v, want not registered historical 0/settled", replay)
	}

	// 二月空档仍不可出账、查询不到账单；状态中的用量与账单列表只含一月与三月。
	if _, err := s.CreateBill("u", feb(2026)); !errors.Is(err, ErrBillBeforeSubscription) {
		t.Fatalf("february gap bill = %v, want ErrBillBeforeSubscription", err)
	}
	if _, err := s.GetBill("u", feb(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("february bill queryable = %v, want ErrBillNotFound", err)
	}
	st, _ := s.Status("u")
	if !st.Subscribed || st.CurrentTerms != newSavedTerms {
		t.Fatalf("status after march usage = %+v, want subscribed with registered snapshot %+v", st, newSavedTerms)
	}
	if len(st.MonthlyUsage) != 2 ||
		st.MonthlyUsage[0] != (Usage{Period: jan(2026), Total: 15}) ||
		st.MonthlyUsage[1] != (Usage{Period: mar(2026), Total: 25}) {
		t.Fatalf("monthly usage = %+v, want [2026-01=15 2026-03=25]", st.MonthlyUsage)
	}
	if len(st.Bills) != 1 || st.Bills[0].Period != jan(2026) ||
		st.Bills[0].TotalDue != 1650 || st.Bills[0].Paid != 1650 ||
		st.Bills[0].Balance != 0 || !st.Bills[0].Settled {
		t.Fatalf("bills = %+v, want only settled january 1650", st.Bills)
	}
}
