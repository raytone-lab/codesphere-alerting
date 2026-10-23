package balancealert

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

// 本文件覆盖 RefreshCreditExcludes 整条链路：成功换名单、失败记账、告警去重、
// 恢复清账、状态不可读时拒绝整份回写。
//
// 只有「读计费库」这一步被替换成桩（scanCreditExcludes），其余全部跑生产代码：
// 失败计数、ShouldNotifyFailure 的阈值、恢复通知的判据、名单的整份替换、状态落库。
// 这些规则之前因为第一步要连真实 datasource 而完全不可测，是语句覆盖率 0 的那一段。

// stubScan 把「读计费库」换成测试桩，并在用例结束时复原。
func stubScan(t *testing.T, fn func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error)) {
	t.Helper()
	prev := scanCreditExcludes
	scanCreditExcludes = fn
	t.Cleanup(func() { scanCreditExcludes = prev })
}

// noticeSink 是钉钉 webhook 的替身：只关心发了几条、内容是什么。
type noticeSink struct {
	mu    sync.Mutex
	lines []string
}

func newNoticeSink(t *testing.T) (*noticeSink, string) {
	t.Helper()
	sink := &noticeSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		sink.mu.Lock()
		sink.lines = append(sink.lines, payload.Text.Content)
		sink.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return sink, srv.URL + "/robot/send?access_token=test"
}

func (s *noticeSink) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func settingsWithSink(t *testing.T, n9e *ctx.Context, webhookURL string) {
	t.Helper()
	s := models.DefaultBalanceAlertSettings()
	s.PilotReceivers = []string{webhookURL}
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
}

func excludeState(t *testing.T, n9e *ctx.Context) models.BalanceAlertExcludeState {
	t.Helper()
	state, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func excludeIDs(t *testing.T, n9e *ctx.Context) map[string]struct{} {
	t.Helper()
	ids, err := models.BalanceAlertCreditExcludeIDs(n9e)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestRefreshCreditExcludesSuccessReplacesWholeList 覆盖成功路径：名单整份换掉、
// 状态记下规模与分布、连续失败清零、窗口取自参数（而不是硬编码 90）。
func TestRefreshCreditExcludesSuccessReplacesWholeList(t *testing.T) {
	n9e := testRunnerCtx(t)
	window := 30
	s := models.DefaultBalanceAlertSettings()
	s.CreditAdvanceWindowDays = &window
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}

	okAt := time.Date(2026, 10, 20, 7, 30, 0, 0, locShanghai)
	// 上一轮是失败的第 3 次：成功之后必须整体清零。
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt: &okAt, ConsecutiveFailures: 3, LastError: "old failure", Total: 1,
	}, "root"); err != nil {
		t.Fatal(err)
	}
	// 旧名单里有一家这一轮不再命中的企业：整份替换必须让它消失。
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "gone", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: okAt},
	}); err != nil {
		t.Fatal(err)
	}

	var gotWindow int
	stubScan(t, func(_ *ctx.Context, _ models.BalanceAlertSettings, windowDays int) (map[string]string, error) {
		gotWindow = windowDays
		return map[string]string{
			"a": models.ExcludeReasonOpenDebt,
			"b": models.ExcludeReasonRecentSettlement,
		}, nil
	})

	res, err := RefreshCreditExcludes(n9e, TriggerCron)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotWindow != 30 {
		t.Fatalf("scan must receive the configured window 30, got %d", gotWindow)
	}
	if res.WindowDays != 30 || res.Trigger != TriggerCron || res.Total != 2 {
		t.Fatalf("result: %+v", res)
	}
	if res.Reasons[models.ExcludeReasonOpenDebt] != 1 ||
		res.Reasons[models.ExcludeReasonRecentSettlement] != 1 {
		t.Fatalf("reason distribution: %+v", res.Reasons)
	}

	ids := excludeIDs(t, n9e)
	if len(ids) != 2 {
		t.Fatalf("list must be swapped wholesale, got %v", ids)
	}
	if _, ok := ids["gone"]; ok {
		t.Fatalf("stale row must not survive a full replace, got %v", ids)
	}

	state := excludeState(t, n9e)
	if state.ConsecutiveFailures != 0 || state.LastError != "" {
		t.Fatalf("success must clear failure bookkeeping: %+v", state)
	}
	// last_success_at 是「刷新成功」的时刻，refreshed_at 是行的时间戳，两者是两次
	// time.Now()，在细粒度时钟上必然相差几微秒——断言相等会让用例随时钟精度变红。
	// 真正要锁的是同一轮、且成功时刻不早于名单落库时刻。
	if state.LastSuccessAt == nil {
		t.Fatal("a successful refresh must stamp last_success_at")
	}
	if state.LastSuccessAt.Before(res.RefreshedAt) {
		t.Fatalf("last_success_at %s must not predate the list stamp %s", state.LastSuccessAt, res.RefreshedAt)
	}
	if gap := state.LastSuccessAt.Sub(res.RefreshedAt); gap > time.Second {
		t.Fatalf("last_success_at is %s after the list stamp; not the same refresh", gap)
	}
	if state.Total != 2 || state.Reasons[models.ExcludeReasonOpenDebt] != 1 {
		t.Fatalf("state must record list size and reasons: %+v", state)
	}
	if state.LastTrigger != string(TriggerCron) || state.WindowDays != 30 {
		t.Fatalf("state must record trigger and window: %+v", state)
	}
	if !state.Ready() {
		t.Fatal("a successful refresh must make the list usable")
	}
}

// TestRefreshCreditExcludesFailureKeepsOldListAndPagesOnce 覆盖「已有成功名单 → 失败」
// 的降级路径：旧名单继续可用、计数累加、第 2 次才喊人一次、第 3 次不再重复。
func TestRefreshCreditExcludesFailureKeepsOldListAndPagesOnce(t *testing.T) {
	n9e := testRunnerCtx(t)
	sink, url := newNoticeSink(t)
	settingsWithSink(t, n9e, url)

	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return nil, errors.New("billing query timeout")
	})

	okAt := time.Date(2026, 10, 20, 7, 30, 0, 0, locShanghai)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt: &okAt, Total: 1,
		Reasons: map[string]int{models.ExcludeReasonOpenDebt: 1},
	}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "old", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: okAt},
	}); err != nil {
		t.Fatal(err)
	}

	for round := 1; round <= 3; round++ {
		if _, err := RefreshCreditExcludes(n9e, TriggerCron); err == nil {
			t.Fatalf("round %d: a failed refresh must be reported to the caller", round)
		}
		state := excludeState(t, n9e)
		if state.ConsecutiveFailures != round {
			t.Fatalf("round %d: consecutive_failures=%d, want %d", round, state.ConsecutiveFailures, round)
		}
		if !state.Ready() {
			t.Fatalf("round %d: an old successful list must keep the run ready", round)
		}
		if state.LastError == "" {
			t.Fatalf("round %d: last_error must be persisted for the params page", round)
		}
		// 只有第 2 次喊人；第 3 次起刻意静默（每天一次失败不该变成每天一条钉钉），
		// 所以累积消息数在第 2 次之后恒为 1。
		wantMessages := 0
		if round >= 2 {
			wantMessages = 1
		}
		if got := len(sink.messages()); got != wantMessages {
			t.Fatalf("round %d: notices=%d, want %d (%v)", round, got, wantMessages, sink.messages())
		}
		// 失败不改变上一份成功名单：清空它会把全部授信企业当预付费。
		ids := excludeIDs(t, n9e)
		if len(ids) != 1 {
			t.Fatalf("round %d: failed refresh must leave the old list intact, got %v", round, ids)
		}
		if _, ok := ids["old"]; !ok {
			t.Fatalf("round %d: old row vanished, got %v", round, ids)
		}
	}

	msgs := sink.messages()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one failure notice, got %v", msgs)
	}
	if !strings.Contains(msgs[0], "沿用旧名单") {
		t.Fatalf("stale-list notice must say the old list is still in use, got %q", msgs[0])
	}
	if !strings.Contains(msgs[0], "连续 2 次") {
		t.Fatalf("notice must name the announced failure count, got %q", msgs[0])
	}
	if !strings.Contains(msgs[0], "billing query timeout") {
		t.Fatalf("notice must carry the error text, got %q", msgs[0])
	}
}

// TestRefreshCreditExcludesNeverSucceededPagesOnFirstFailure 覆盖 fail closed 场景：
// 从未成功刷新过时，第 1 次失败就必须喊人，且文案要说清「当轮已停止圈选」。
func TestRefreshCreditExcludesNeverSucceededPagesOnFirstFailure(t *testing.T) {
	n9e := testRunnerCtx(t)
	sink, url := newNoticeSink(t)
	settingsWithSink(t, n9e, url)
	// 清空状态：testRunnerCtx 默认处于「已就绪」，这里要的是全新环境。
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{}, "root"); err != nil {
		t.Fatal(err)
	}
	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return nil, errors.New("connect billing: connection refused")
	})

	if _, err := RefreshCreditExcludes(n9e, TriggerCron); err == nil {
		t.Fatal("expected the refresh failure")
	}

	msgs := sink.messages()
	if len(msgs) != 1 {
		t.Fatalf("never-succeeded failure must page immediately, got %v", msgs)
	}
	for _, want := range []string{"从未成功刷新", "fail closed", "connection refused"} {
		if !strings.Contains(msgs[0], want) {
			t.Fatalf("notice must contain %q, got %q", want, msgs[0])
		}
	}

	state := excludeState(t, n9e)
	if state.Ready() {
		t.Fatal("a never-succeeded refresh must leave the state unready")
	}
	if state.ConsecutiveFailures != 1 || state.LastNotifiedFailures != 1 {
		t.Fatalf("first failure must be recorded and announced once: %+v", state)
	}
}

// TestRefreshCreditExcludesManualFailureDoesNotAdvanceCounter 锁定手动刷新的边界：
// 页面按钮连点不该把定时任务的失败计数顶到告警阈值，也不该自己推钉钉。
func TestRefreshCreditExcludesManualFailureDoesNotAdvanceCounter(t *testing.T) {
	n9e := testRunnerCtx(t)
	sink, url := newNoticeSink(t)
	settingsWithSink(t, n9e, url)

	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return nil, errors.New("manual refresh failed")
	})

	okAt := time.Date(2026, 10, 20, 7, 30, 0, 0, locShanghai)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt: &okAt, ConsecutiveFailures: 1,
	}, "root"); err != nil {
		t.Fatal(err)
	}

	if _, err := RefreshCreditExcludes(n9e, TriggerManual); err == nil {
		t.Fatal("manual refresh must surface the error to the page")
	}
	state := excludeState(t, n9e)
	if state.ConsecutiveFailures != 1 {
		t.Fatalf("manual failure must not advance the counter, got %d", state.ConsecutiveFailures)
	}
	if state.LastTrigger != string(TriggerManual) {
		t.Fatalf("state must record the manual attempt: %+v", state)
	}
	if state.LastAttemptAt == nil {
		t.Fatal("manual failure must still record last_attempt_at")
	}
	if state.LastError == "" {
		t.Fatal("manual failure must record last_error for the page")
	}
	if got := len(sink.messages()); got != 0 {
		t.Fatalf("manual refresh must stay silent on the webhook, got %d messages", got)
	}

	// 计数仍停在 1：紧接着的第一次定时失败才把它推到 2，并且只喊这一次。
	if _, err := RefreshCreditExcludes(n9e, TriggerCron); err == nil {
		t.Fatal("expected the cron failure")
	}
	state = excludeState(t, n9e)
	if state.ConsecutiveFailures != 2 {
		t.Fatalf("cron failure must advance to 2, got %d", state.ConsecutiveFailures)
	}
	if got := len(sink.messages()); got != 1 {
		t.Fatalf("the second failure is the one that pages, got %d messages", got)
	}
}

// TestRefreshCreditExcludesRecoveryNotifiesAndClearsBooks 覆盖恢复：此前喊过「坏了」，
// 成功后必须补一条「已恢复」，并清掉已通告计数，否则下一条新故障永远排不上队。
func TestRefreshCreditExcludesRecoveryNotifiesAndClearsBooks(t *testing.T) {
	n9e := testRunnerCtx(t)
	sink, url := newNoticeSink(t)
	settingsWithSink(t, n9e, url)

	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return map[string]string{"a": models.ExcludeReasonRecentAdvance}, nil
	})

	okAt := time.Date(2026, 10, 20, 7, 30, 0, 0, locShanghai)
	notifiedAt := time.Date(2026, 10, 21, 7, 30, 0, 0, locShanghai)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt:        &okAt,
		ConsecutiveFailures:  2,
		LastNotifiedFailures: 2,
		LastNotifiedAt:       &notifiedAt,
		LastError:            "old failure",
		Total:                3,
	}, "root"); err != nil {
		t.Fatal(err)
	}

	if _, err := RefreshCreditExcludes(n9e, TriggerCron); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	msgs := sink.messages()
	if len(msgs) != 1 {
		t.Fatalf("recovery must send exactly one notice, got %v", msgs)
	}
	for _, want := range []string{"已恢复", "此前连续失败 2 次", "刷新 1 家"} {
		if !strings.Contains(msgs[0], want) {
			t.Fatalf("recovery notice must contain %q, got %q", want, msgs[0])
		}
	}

	state := excludeState(t, n9e)
	if state.LastNotifiedFailures != 0 || state.LastNotifiedAt != nil {
		t.Fatalf("delivered recovery must clear the announcement books: %+v", state)
	}
	if state.ConsecutiveFailures != 0 || state.Total != 1 {
		t.Fatalf("recovery must reset failures and record the new list size: %+v", state)
	}
}

// TestRefreshCreditExcludesRecordsSettingsReadFailure 覆盖「连参数都读不出来」：
// 不去连计费库，但必须记一次失败并累加计数，等参数恢复后的第一次失败把停机一起报出来。
func TestRefreshCreditExcludesRecordsSettingsReadFailure(t *testing.T) {
	n9e := testRunnerCtx(t)
	okAt := time.Date(2026, 10, 20, 7, 30, 0, 0, locShanghai)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt: &okAt, ConsecutiveFailures: 1,
	}, "root"); err != nil {
		t.Fatal(err)
	}
	// 非法 JSON 让 BalanceAlertSettingsGet 直接失败：状态项是另一个 key，仍可读。
	if err := models.ConfigsSet(n9e, models.BalanceAlertSettingsKey, "{not json"); err != nil {
		t.Fatal(err)
	}

	scanned := false
	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		scanned = true
		return nil, nil
	})

	if _, err := RefreshCreditExcludes(n9e, TriggerCron); err == nil {
		t.Fatal("unreadable settings must fail the refresh")
	}
	if scanned {
		t.Fatal("must not touch the billing DB without settings (no window, no datasource)")
	}

	state := excludeState(t, n9e)
	if state.ConsecutiveFailures != 2 {
		t.Fatalf("settings failure must advance the counter, got %d", state.ConsecutiveFailures)
	}
	if !strings.Contains(state.LastError, "read settings") {
		t.Fatalf("last_error must explain the settings read failure, got %q", state.LastError)
	}
	if state.LastAttemptAt == nil {
		t.Fatal("settings failure must still record last_attempt_at")
	}

	// 手动刷新同样不推进计数。
	if _, err := RefreshCreditExcludes(n9e, TriggerManual); err == nil {
		t.Fatal("expected the manual refresh to fail too")
	}
	if got := excludeState(t, n9e).ConsecutiveFailures; got != 2 {
		t.Fatalf("manual settings failure must not advance the counter, got %d", got)
	}
}

// TestRefreshCreditExcludesNeverRewritesUnreadableState 锁定最危险的一条：
// 状态读不出来时绝不整份回写。Put 会把手里的零值写回去，last_success_at 变 nil，
// 一次读抖动就被放大成「永久停止圈人」。
func TestRefreshCreditExcludesNeverRewritesUnreadableState(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return map[string]string{"a": models.ExcludeReasonOpenDebt}, nil
	})
	if err := models.ConfigsSet(n9e, models.BalanceAlertExcludeStateKey, "{corrupt"); err != nil {
		t.Fatal(err)
	}
	before, err := models.ConfigsGet(n9e, models.BalanceAlertExcludeStateKey)
	if err != nil {
		t.Fatal(err)
	}

	// 名单已经换新，但健康状态没落库：必须当错误报出去，下一轮才能继续 fail closed。
	_, err = RefreshCreditExcludes(n9e, TriggerCron)
	if err == nil || !strings.Contains(err.Error(), "read exclude refresh state") {
		t.Fatalf("expected a state read error, got %v", err)
	}
	if ids := excludeIDs(t, n9e); len(ids) != 1 {
		t.Fatalf("the new list is committed before the state write, got %v", ids)
	}
	after, err := models.ConfigsGet(n9e, models.BalanceAlertExcludeStateKey)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("unreadable state must never be overwritten with a zero value: %q -> %q", before, after)
	}

	// 扫描也失败时，报的是扫描错误，状态依然原样。
	stubScan(t, func(*ctx.Context, models.BalanceAlertSettings, int) (map[string]string, error) {
		return nil, errors.New("billing down")
	})
	_, err = RefreshCreditExcludes(n9e, TriggerCron)
	if err == nil || !strings.Contains(err.Error(), "billing down") {
		t.Fatalf("expected the scan error to win, got %v", err)
	}
	if again, _ := models.ConfigsGet(n9e, models.BalanceAlertExcludeStateKey); again != before {
		t.Fatalf("state must stay untouched, got %q", again)
	}
}

// TestApplyCreditExcludesEmptyListKeepsAllAccounts 覆盖「名单为空但已就绪」：
// 空名单是合法结果（全库都没有授信活跃企业），不能与「从未刷新」混为一谈。
func TestApplyCreditExcludesEmptyListKeepsAllAccounts(t *testing.T) {
	n9e := testRunnerCtx(t)
	if err := models.BalanceAlertCreditExcludeReplace(n9e, nil); err != nil {
		t.Fatal(err)
	}
	kept, skipped, err := applyCreditExcludes(n9e, []Account{{ID: "p1"}, {ID: "p2"}})
	if err != nil {
		t.Fatalf("an empty but ready list must not fail closed: %v", err)
	}
	if skipped != 0 || len(kept) != 2 {
		t.Fatalf("kept=%d skipped=%d, want all accounts kept", len(kept), skipped)
	}
}
