package balancealert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestExcludeStateRecoveryBooking(t *testing.T) {
	notifiedAt := time.Date(2026, 10, 20, 8, 0, 0, 0, locShanghai)
	successAfter := notifiedAt.Add(2 * time.Hour)
	successBefore := notifiedAt.Add(-1 * time.Hour)

	// 已通告的故障被之后的一次成功终止：恢复通知还欠着。
	ended := models.BalanceAlertExcludeState{
		LastSuccessAt:        &successAfter,
		LastNotifiedAt:       &notifiedAt,
		LastNotifiedFailures: 2,
		ConsecutiveFailures:  1, // 之后又失败了一次，但上一段故障确实结束过
	}
	if !ended.NotifiedIncidentEnded() {
		t.Fatal("a success after the notified failure must close that incident")
	}
	if got := incidentFailures(ended); got != 2 {
		t.Fatalf("recovery must report the announced count, got %d", got)
	}

	// 通告之后没有成功过：故障仍在持续，群里那条「坏了」还是最新的。
	ongoing := models.BalanceAlertExcludeState{
		LastSuccessAt:        &successBefore,
		LastNotifiedAt:       &notifiedAt,
		LastNotifiedFailures: 2,
		ConsecutiveFailures:  3,
	}
	if ongoing.NotifiedIncidentEnded() {
		t.Fatal("an ongoing incident must not claim recovery")
	}
	if got := incidentFailures(ongoing); got != 3 {
		t.Fatalf("recovery must report the full outage, got %d", got)
	}

	// 旧版本状态只记了次数：没有更细证据时，恢复态（无条件可败）保守认定已结束，
	// 宁可多补一条恢复，也不让群里永远停在「坏了」。
	legacy := models.BalanceAlertExcludeState{
		LastSuccessAt:        &successAfter,
		LastNotifiedFailures: 2,
	}
	if !legacy.NotifiedIncidentEnded() {
		t.Fatal("state without last_notified_at must not strand the recovery notice")
	}
	// 但旧状态仍在失败中时不能声称恢复：那条「坏了」还是最新的，等下一次成功收尾。
	legacyFailing := legacy
	legacyFailing.ConsecutiveFailures = 3
	if legacyFailing.NotifiedIncidentEnded() {
		t.Fatal("legacy state that is still failing must not claim recovery")
	}
	// 从未成功刷新过，就谈不上「恢复终止过故障」。
	never := models.BalanceAlertExcludeState{LastNotifiedFailures: 2, ConsecutiveFailures: 3}
	if never.NotifiedIncidentEnded() {
		t.Fatal("a never-succeeded incident cannot have ended by success")
	}

	// 刷新成功路径的判据是「本次成功时刻」，不是上一次成功时刻：先成功、后连续
	// 失败再成功才是常态，若拿 prev.LastSuccessAt 去比，正常恢复会被静默吞掉。
	normal := models.BalanceAlertExcludeState{
		LastSuccessAt:        &successBefore,
		LastNotifiedAt:       &notifiedAt,
		LastNotifiedFailures: 2,
		ConsecutiveFailures:  3,
	}
	if !normal.NotifiedIncidentEndedAt(notifiedAt.Add(24 * time.Hour)) {
		t.Fatal("a refresh succeeding after the notification must send the recovery notice")
	}
	if normal.NotifiedIncidentEnded() {
		t.Fatal("judging by the previous success would hide the normal recovery — use the new success time")
	}
}

// TestNotifyPendingExcludeOpsSettlesBothBooks 锁定开关恢复后的补报顺序：
// 先还「已恢复」的账，再看新故障有没有到告警阈值。两者混成一个 switch 时，
// 「关闭期间恢复、随后又失败」会把两份消息一起丢掉。
func TestNotifyPendingExcludeOpsSettlesBothBooks(t *testing.T) {
	n9e := testRunnerCtx(t)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		bodies = append(bodies, payload.Text.Content)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	s := models.DefaultBalanceAlertSettings()
	s.PilotReceivers = []string{srv.URL + "/robot/send?access_token=test"}
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}

	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}

	notifiedAt := time.Date(2026, 10, 20, 8, 0, 0, 0, locShanghai)
	successAfter := notifiedAt.Add(2 * time.Hour)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt:        &successAfter,
		LastAttemptAt:        &successAfter,
		LastNotifiedAt:       &notifiedAt,
		LastNotifiedFailures: 2,
		ConsecutiveFailures:  1,
		Total:                7,
	}, "root"); err != nil {
		t.Fatal(err)
	}

	NotifyPendingExcludeOps(n9e)
	got := sent()
	if len(got) != 1 {
		t.Fatalf("expected exactly the owed recovery notice, got %v", got)
	}
	if !strings.Contains(got[0], "已恢复") || !strings.Contains(got[0], "连续失败 2 次") {
		t.Fatalf("recovery notice must close the announced 2-failure incident, got %q", got[0])
	}
	state, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastNotifiedFailures != 0 || state.LastNotifiedAt != nil {
		t.Fatalf("delivered recovery must clear the books: %+v", state)
	}

	// 第二遍没有待办：不能把同一条恢复反复发。
	NotifyPendingExcludeOps(n9e)
	if again := sent(); len(again) != 1 {
		t.Fatalf("second replay must stay silent, got %v", again)
	}
}

// TestNotifyPendingExcludeOpsStaysSilentWhileOngoing 锁定「故障还在持续」时不喊恢复。
func TestNotifyPendingExcludeOpsStaysSilentWhileOngoing(t *testing.T) {
	n9e := testRunnerCtx(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()

	s := models.DefaultBalanceAlertSettings()
	s.PilotReceivers = []string{srv.URL + "/robot/send?access_token=test"}
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}

	notifiedAt := time.Date(2026, 10, 20, 8, 0, 0, 0, locShanghai)
	successBefore := notifiedAt.Add(-time.Hour)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt:        &successBefore,
		LastNotifiedAt:       &notifiedAt,
		LastNotifiedFailures: 2,
		ConsecutiveFailures:  3,
	}, "root"); err != nil {
		t.Fatal(err)
	}

	NotifyPendingExcludeOps(n9e)
	if hits != 0 {
		t.Fatalf("an ongoing, already-announced incident must stay silent, got %d messages", hits)
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
