package balancealert

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
)

func TestExcludeScanSQLGuards(t *testing.T) {
	needles := []string{
		// RECENT_ADVANCE 必须先限定 approved_credit_usd > 0，否则窗口内任何
		// 余额消费都会命中，真预付费被整体误排。
		"al.approved_credit_usd > 0\n          AND al.created_at > now() - make_interval(days => ?::int)",
		// 核销条件只认信用核销。这是三段独立条件之一（不是冗余保护）：企业可能在
		// 最后一次垫付超窗之后才做对账代充，删掉它会把刚结清的企业拉回预警池。
		"s.debt_source = 'ENTERPRISE_CREDIT'",
		// 债务阈值用灰尘量级，不用 > 0。
		"al.approved_credit_usd - COALESCE(al.settled_usd, 0) > 0.0001",
		// 匹配部分索引谓词，避免每日刷新全表扫。
		"(al.approved_credit_usd > 0 OR al.unapproved_debt_usd > 0)",
		// 过滤无标记行，否则 ELSE 会把未批准欠款误标成 RECENT_SETTLEMENT。
		"WHERE activity.open_debt OR activity.recent_advance OR activity.recent_settlement",
		"GROUP BY account_id",
	}
	for _, n := range needles {
		if !strings.Contains(excludeScanSQL, n) {
			t.Errorf("excludeScanSQL missing %q", n)
		}
	}
	if strings.Contains(excludeScanSQL, "$1") {
		t.Error("use GORM ? placeholders, not native $1")
	}
	// 计费库只读：扫描 SQL 不得出现任何写操作。
	for _, banned := range []string{"INSERT ", "UPDATE ", "DELETE ", "TRUNCATE "} {
		if strings.Contains(strings.ToUpper(excludeScanSQL), banned) {
			t.Errorf("excludeScanSQL must be read-only, found %q", banned)
		}
	}
}

func TestExcludeStateReadyAndAlert(t *testing.T) {
	var zero models.BalanceAlertExcludeState
	if zero.Ready() {
		t.Fatal("empty state must not be ready")
	}
	// 从未成功刷新：整条链路不可用，第 1 次失败就必须喊人，且之后每次都要喊。
	first := models.BalanceAlertExcludeState{ConsecutiveFailures: 1}
	if !first.ShouldNotifyFailure() {
		t.Fatal("never-succeeded must page ops on the first failure")
	}
	if (models.BalanceAlertExcludeState{ConsecutiveFailures: 2, LastNotifiedFailures: 2}).ShouldNotifyFailure() {
		t.Fatal("never-succeeded state already notified for this attempt must not page twice")
	}
	if !(models.BalanceAlertExcludeState{ConsecutiveFailures: 5, LastNotifiedFailures: 4}).ShouldNotifyFailure() {
		t.Fatal("never-succeeded must keep paging on every new failure")
	}

	at := time.Date(2026, 10, 22, 7, 30, 0, 0, locShanghai)
	one := models.BalanceAlertExcludeState{LastSuccessAt: &at, ConsecutiveFailures: 1}
	if !one.Ready() {
		t.Fatal("state with last success must be ready")
	}
	if one.ShouldNotifyFailure() {
		t.Fatal("single failure should not page ops while an old list is still in use")
	}

	two := one
	two.ConsecutiveFailures = 2
	if !two.ShouldNotifyFailure() {
		t.Fatal("two consecutive failures must page ops once")
	}
	// 已经就这次故障报过：LastNotifiedFailures 非零后不再重复。
	two.LastNotifiedFailures = 2
	if two.ShouldNotifyFailure() {
		t.Fatal("already-notified stale-list failure must not page again")
	}

	// 第 3 次起不再重复发：刷新每天一次，持续故障会变成每天一条重复钉钉。
	three := two
	three.ConsecutiveFailures = 3
	if three.ShouldNotifyFailure() {
		t.Fatal("failures beyond the first notice must not page ops again")
	}
	if !three.NeedsRecoveryNotice() {
		t.Fatal("state that already paged must ask for a recovery notice")
	}
	if one.NeedsRecoveryNotice() {
		t.Fatal("a single failure never alerted, so recovery must stay silent")
	}

	// 告警开关关闭时不会推进 LastNotifiedFailures，因此重新打开后仍判定为待通知。
	pending := one
	pending.ConsecutiveFailures = 4
	if !pending.ShouldNotifyFailure() {
		t.Fatal("failure that happened while alerts were disabled must stay pending")
	}

	// 恢复发生在开关关闭期间：失败已归零但恢复通知还没送出，补报要能识别出这笔账。
	silentRecovery := models.BalanceAlertExcludeState{
		LastSuccessAt:        &at,
		ConsecutiveFailures:  0,
		LastNotifiedFailures: 2,
	}
	if silentRecovery.ShouldNotifyFailure() {
		t.Fatal("a recovered state must not report a failure")
	}
	if !silentRecovery.NeedsRecoveryNotice() {
		t.Fatal("suppressed recovery must remain pending until the switch is back on")
	}
}

func TestSendExcludeOpsNoticeWithoutWebhook(t *testing.T) {
	// 没配 webhook 时必须报「没送到」：否则 LastNotifiedFailures 会被推进，
	// 恢复通知就会声称「已告诉你们」——而其实谁也没收到。
	if sendExcludeOpsNotice(models.BalanceAlertSettings{}, "x") {
		t.Fatal("no webhook configured must not count as delivered")
	}
}

func TestBillingCtxCarriesDeadline(t *testing.T) {
	ctx, cancel := billingCtx(context.Background())
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("cross-db billing queries must carry a deadline")
	}

	// 调用方传 nil 也不能 panic：后台任务里 std 可能没被赋值。
	ctx, cancel = billingCtx(nil)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("nil parent must still get a deadline")
	}
}

func TestApplyCreditExcludesFiltersListedAccounts(t *testing.T) {
	n9e := testRunnerCtx(t)
	at := time.Date(2026, 10, 22, 7, 30, 0, 0, locShanghai)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{LastSuccessAt: &at}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "ba-credit", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: at},
	}); err != nil {
		t.Fatal(err)
	}

	kept, skipped, err := applyCreditExcludes(n9e, []Account{{ID: "ba-credit"}, {ID: "ba-prepaid"}})
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 || len(kept) != 1 || kept[0].ID != "ba-prepaid" {
		t.Fatalf("filter: kept=%+v skipped=%d", kept, skipped)
	}
}

func TestRunnerFailsClosedWithoutExcludeList(t *testing.T) {
	n9e := testRunnerCtx(t)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{}, "root"); err != nil {
		t.Fatal(err)
	}
	s := models.DefaultBalanceAlertSettings()
	s.SendMode = SendModeOff
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}

	httpClient := &fakeHTTP{}
	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "未刷新鲜", Balance: 1, HasVoucher: true},
		}},
		HTTP:   httpClient,
		Sender: HTTPSender{HTTP: httpClient},
		Now:    func() time.Time { return time.Date(2026, 10, 22, 10, 0, 0, 0, locShanghai) },
	}
	stats, err := r.run(context.Background())
	if err == nil {
		t.Fatal("expected fail-closed error when exclude list was never refreshed")
	}
	if !stats.ExcludeBlocked {
		t.Fatalf("expected exclude_blocked: %+v", stats)
	}
	if stats.Prepaid != 0 || stats.Evaluated != 0 || stats.Sent != 0 {
		t.Fatalf("must not circle or send while blocked: %+v", stats)
	}
	lst, err := models.BalanceAlertRecordGets(n9e, "", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lst) != 0 {
		t.Fatalf("must not persist records while blocked: %+v", lst)
	}
}

func TestCreditExcludeReplaceIsWholeListSwap(t *testing.T) {
	n9e := testRunnerCtx(t)
	at := time.Date(2026, 10, 22, 7, 30, 0, 0, locShanghai)
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "a", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: at},
		{BillingAccountID: "b", Reason: models.ExcludeReasonRecentAdvance, RefreshedAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	// 第二次刷新只留 b：整份替换必须收敛，不能残留上一轮的行。
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "b", Reason: models.ExcludeReasonRecentSettlement, RefreshedAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := models.BalanceAlertCreditExcludeIDs(n9e)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected only b, got %v", ids)
	}
	if _, ok := ids["b"]; !ok {
		t.Fatalf("expected b, got %v", ids)
	}

	counts, total, err := models.BalanceAlertCreditExcludeReasonCounts(n9e)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || counts[models.ExcludeReasonRecentSettlement] != 1 {
		t.Fatalf("counts=%v total=%d", counts, total)
	}

	// 空名单也是合法结果（全库都没有授信活跃企业），必须能整份清空。
	if err := models.BalanceAlertCreditExcludeReplace(n9e, nil); err != nil {
		t.Fatal(err)
	}
	ids, err = models.BalanceAlertCreditExcludeIDs(n9e)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected empty list, got %v", ids)
	}
}
