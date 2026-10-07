package meter

import (
	"errors"
	"testing"
	"time"
)

// 本文件回归“取消订阅后提前登记重新开通”场景下当前账期预计费用查询
// （EstimateCurrentBill）的行为：
//
//   - 能否返回预估只取决于当前时刻是否已有生效订阅：重新开通时刻到达前一律
//     返回 ErrSubscriptionNotActivated，不因账户留有旧订阅、旧用量、旧账单而
//     沿用旧套餐出预估，也不因进入开通所在月或到了开通当天零点而提前成功；
//   - 恰好到达登记的开通时刻后直接查询即成功，不依赖先上报用量或正式出账；
//     预估只采用新订阅在登记时保存的当月套餐条件快照，等待期间对目标套餐
//     定义的修改不得混入；
//   - 预估是纯读操作：不生成正式账单、不关闭当月、不改写历史账单的金额与
//     付款状态；等待期间失败的查询也不删除已登记的订阅。
//
// 时间线（全部 UTC）：
//
//	2026-01-01 开通 plan-old：月费 1000、包含 10、超额单价 100、税率 10%。
//	2026-01-10 一月用量 15；2026-01-15 登记按月取消，2026-02-01 00:00 终止。
//	2026-02-01 一月出账：应付 1650（1000 + 5×100 + 税 150），当日本地付款结清。
//	2026-02-10 空档中提前登记 2026-03-20 12:00 重新开通 plan-new，
//	           登记时定义为 2000/20/200/600（税率 6%）。
//	2026-02-11 把 plan-new 当前定义改为 5000/5/900/2500：
//	           只能影响以后新保存的条件，不得改写已登记订阅的快照。
//	2026-02-15、2026-03-01、2026-03-20 00:00 查询均返回
//	           ErrSubscriptionNotActivated，且查询不删除已登记订阅。
//	2026-03-20 12:00 恰好到达开通时刻：直接预估成功，账期三月，条件为登记时
//	           保存的 2000/20/200/600；尚无三月用量，税 120，预计应付 2120；
//	           一月累计的 15 不带入三月。
//	2026-03-21 接收 25 单位三月用量：超额 5、超额费 1000、税 180、
//	           预计应付 3180；全程不产生三月账单、不改写一月账单与付款记录。

// TestEstimateCurrentBillFutureResubscribe 端到端回归上述主线。
func TestEstimateCurrentBillFutureResubscribe(t *testing.T) {
	s, clk := newTestService(utc(2026, 1, 1, 0, 0))

	oldDef := planDef("plan-old", 1000, 10, 100, 1000)
	newDefAtRegister := planDef("plan-new", 2000, 20, 200, 600)
	newDefUpdated := planDef("plan-new", 5000, 5, 900, 2500)
	oldTerms := termsOf(oldDef)
	savedTerms := termsOf(newDefAtRegister)
	updatedTerms := termsOf(newDefUpdated)

	mustPlan(t, s, oldDef)
	mustPlan(t, s, newDefAtRegister)
	mustAccount(t, s, "u")
	mustSubscribe(t, s, "u", "plan-old", utc(2026, 1, 1, 0, 0))

	// 一月用量 15：超出旧套餐包含的 10 单位。
	clk.t = utc(2026, 1, 10, 0, 0)
	if r, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-jan", At: utc(2026, 1, 10, 0, 0), Quantity: 15,
	}); err != nil || !r.Accepted || r.Period != jan(2026) {
		t.Fatalf("january usage: %+v %v", r, err)
	}

	// 一月中旬登记按月取消：2026-02-01 00:00 终止。
	clk.t = utc(2026, 1, 15, 12, 0)
	cancelRes := mustCancel(t, s, "u")
	if !cancelRes.Cancelled || !cancelRes.Cancellation.EndAt.Equal(utc(2026, 2, 1, 0, 0)) {
		t.Fatalf("cancel result = %+v, want end 2026-02-01", cancelRes)
	}

	// 终止时刻到达、一月账期结束：出账 1650 并本地付款结清。
	clk.t = utc(2026, 2, 1, 0, 0)
	janBill, err := s.CreateBill("u", jan(2026))
	if err != nil {
		t.Fatalf("create january bill: %v", err)
	}
	if janBill.Terms != oldTerms || janBill.TotalUsage != 15 ||
		janBill.MonthlyFee != 1000 || janBill.OverageFee != 500 ||
		janBill.Tax != 150 || janBill.TotalDue != 1650 {
		t.Fatalf("january bill = %+v, want plan-old 15/1000/500/150/1650", janBill)
	}
	if pay, err := s.RecordPayment("u", "pay-jan", jan(2026), 1650); err != nil ||
		!pay.Registered || pay.BillBalance != 0 || !pay.Settled {
		t.Fatalf("settle january bill: %+v %v", pay, err)
	}

	// 空档中提前登记 2026-03-20 12:00 重新开通 plan-new：
	// 保存登记时刻的完整条件快照 2000/20/200/600。
	clk.t = utc(2026, 2, 10, 0, 0)
	reopenAt := utc(2026, 3, 20, 12, 0)
	mustSubscribe(t, s, "u", "plan-new", reopenAt)

	// 等待开通期间修改目标套餐定义：只允许影响以后新保存的条件。
	clk.t = utc(2026, 2, 11, 0, 0)
	if err := s.UpdatePlan(newDefUpdated); err != nil {
		t.Fatalf("update plan-new: %v", err)
	}

	// 重新开通时刻到达前的每一次查询都必须返回 ErrSubscriptionNotActivated：
	// 不输出旧套餐预估，也不因进入三月或到了开通当天零点而提前成功；
	// 账户里留有旧订阅历史、旧用量与旧账单不改变这一判定。
	for _, at := range []struct {
		name string
		t    time.Time
	}{
		{"february gap", utc(2026, 2, 15, 0, 0)},
		{"march first", utc(2026, 3, 1, 0, 0)},
		{"activation day midnight", utc(2026, 3, 20, 0, 0)},
		{"one minute before", utc(2026, 3, 20, 11, 59)},
	} {
		clk.t = at.t
		if _, err := s.EstimateCurrentBill("u"); !errors.Is(err, ErrSubscriptionNotActivated) {
			t.Fatalf("%s: estimate = %v, want ErrSubscriptionNotActivated", at.name, err)
		}
		// 等待期间查询不显示生效订阅，也不显示任何套餐条件。
		if st, _ := s.Status("u"); st.Subscribed || st.CurrentTerms != (PlanTerms{}) {
			t.Fatalf("%s: status = %+v, want unsubscribed zero terms", at.name, st)
		}
	}
	// 失败的查询没有删除提前登记的订阅，也没有产生任何账单。
	if _, err := s.GetBill("u", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("march bill exists before activation: %v", err)
	}

	// 恰好到达登记的开通时刻：直接查询即成功，不依赖先上报用量或正式出账。
	clk.t = reopenAt
	e, err := s.EstimateCurrentBill("u")
	if err != nil {
		t.Fatalf("estimate at reopen instant: %v", err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "u", Estimated: true, Period: mar(2026),
		Terms:             savedTerms,
		TotalUsage:        0,
		IncludedUnits:     20,
		OverageUnits:      0,
		MonthlyFee:        2000,
		OverageFee:        0,
		Tax:               120, // 2000 × 6%
		EstimatedTotalDue: 2120,
	})
	// 条件必须来自重新开通登记时的快照：既不是旧套餐，也不是修改后的定义。
	if e.Terms == oldTerms {
		t.Fatalf("estimate reused old plan terms %+v", oldTerms)
	}
	if e.Terms == updatedTerms {
		t.Fatalf("estimate used updated plan definition %+v", updatedTerms)
	}

	// 预估是纯读操作：三月没有落账单、当月未被关闭，一月账单原样。
	if _, err := s.GetBill("u", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("estimate saved a march bill: %v", err)
	}
	if _, err := s.CreateBill("u", mar(2026)); !errors.Is(err, ErrBillMonthNotEnded) {
		t.Fatalf("estimate closed march: %v", err)
	}
	if gotJan, err := s.GetBill("u", jan(2026)); err != nil ||
		gotJan.TotalDue != 1650 || gotJan.Paid != 1650 || gotJan.Balance != 0 || !gotJan.Settled {
		t.Fatalf("january bill changed by estimate: %+v %v", gotJan, err)
	}

	// 新订阅生效后接收 25 单位三月用量：超额 25-20=5、超额费 5×200=1000、
	// 税 (2000+1000)×6%=180、预计应付 3180；一月累计的 15 不带入三月。
	clk.t = utc(2026, 3, 21, 0, 0)
	if r, err := s.RecordEvent(Event{
		AccountID: "u", EventID: "e-mar", At: utc(2026, 3, 21, 0, 0), Quantity: 25,
	}); err != nil || !r.Accepted || r.Period != mar(2026) {
		t.Fatalf("march usage: %+v %v", r, err)
	}
	e, err = s.EstimateCurrentBill("u")
	if err != nil {
		t.Fatal(err)
	}
	wantEstimate(t, e, EstimatedBill{
		AccountID: "u", Estimated: true, Period: mar(2026),
		Terms:             savedTerms,
		TotalUsage:        25,
		IncludedUnits:     20,
		OverageUnits:      5,
		MonthlyFee:        2000,
		OverageFee:        1000,
		Tax:               180,
		EstimatedTotalDue: 3180,
	})

	// 接收用量后的预估仍然不落账、不关闭当月、不改写历史。
	if _, err := s.GetBill("u", mar(2026)); !errors.Is(err, ErrBillNotFound) {
		t.Fatalf("estimate with usage saved a bill: %v", err)
	}
	if gotJan, _ := s.GetBill("u", jan(2026)); gotJan.TotalDue != 1650 ||
		gotJan.Paid != 1650 || gotJan.Balance != 0 || !gotJan.Settled {
		t.Fatalf("january bill changed: %+v", gotJan)
	}

	// 已有取消后的历史数据仍按原规则可查：一月累计用量 15、一月账单、
	// 原付款的原样重报（返回首次登记时的历史结果，不再次登记）。
	if u, _ := s.MonthlyUsage("u", jan(2026)); u.Total != 15 {
		t.Fatalf("january usage = %d, want 15", u.Total)
	}
	replay, err := s.RecordPayment("u", "pay-jan", jan(2026), 1650)
	if err != nil || replay.Registered || replay.BillBalance != 0 || !replay.Settled {
		t.Fatalf("january payment replay = %+v %v, want not registered 0/settled", replay, err)
	}
	if gotJan, _ := s.GetBill("u", jan(2026)); gotJan.Paid != 1650 {
		t.Fatalf("january paid changed after replay: %+v", gotJan)
	}
}
