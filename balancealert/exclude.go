package balancealert

import (
	"context"
	"fmt"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/toolkits/pkg/logger"
)

// 排除名单刷新（FR-02/FR-04）。
//
// 跨库两段式，刻意如此：
//  1. 在**计费库**只跑一条 SELECT 聚合，算出授信活跃账户；alerting 对计费库只有
//     只读权限，不在别人的库里建表、不写入、不需要 GRANT。
//  2. 把结果整份替换进 alerting 自己的库（n9e 库），单事务 DELETE + INSERT。
//
// 15 分钟判定轮次只读 alerting 库的名单，再用内存集合过滤，因此既不回扫
// balance_consumption_allocations（G4），也不跨库 join。

// excludeScanSQL 在计费库执行：一次算出三类命中并按 OPEN_DEBT > RECENT_ADVANCE >
// RECENT_SETTLEMENT 取主因。窗口为滚动 N×24 小时，不按自然日截断。
const excludeScanSQL = `
SELECT account_id,
       CASE
         WHEN bool_or(open_debt) THEN '` + models.ExcludeReasonOpenDebt + `'
         WHEN bool_or(recent_advance) THEN '` + models.ExcludeReasonRecentAdvance + `'
         ELSE '` + models.ExcludeReasonRecentSettlement + `'
       END AS reason
FROM (
  SELECT al.billing_account_id AS account_id,
         (al.approved_credit_usd - COALESCE(al.settled_usd, 0) > 0.0001) AS open_debt,
         (al.approved_credit_usd > 0
          AND al.created_at > now() - make_interval(days => ?::int)) AS recent_advance,
         FALSE AS recent_settlement
  FROM balance_consumption_allocations al
  -- 谓词刻意与部分索引 balance_allocations_account_funding_open_idx 完全一致，
  -- 使每日刷新走索引而不是全表扫。unapproved 行不产生任何标记，只为匹配索引。
  WHERE (al.approved_credit_usd > 0 OR al.unapproved_debt_usd > 0)
    AND al.billing_account_id IS NOT NULL
  UNION ALL
  SELECT s.billing_account_id, FALSE, FALSE, TRUE
  FROM enterprise_credit_settlements s
  -- 必须限定 debt_source：真预付费穿 0 后充值同样会产生 NEGATIVE_BALANCE
  -- 核销，不限定会把 TC-05 类企业误排最多一个窗口。
  WHERE s.debt_source = 'ENTERPRISE_CREDIT'
    AND s.occurred_at > now() - make_interval(days => ?::int)
) activity
-- 只有 unapproved_debt 的行两标记皆假，必须过滤；否则 ELSE 分支会把它们
-- 标成 RECENT_SETTLEMENT 并误排除。
WHERE activity.open_debt OR activity.recent_advance OR activity.recent_settlement
GROUP BY account_id
`

type ExcludeRefreshResult struct {
	Trigger     string         `json:"trigger"`
	WindowDays  int            `json:"window_days"`
	Total       int            `json:"total"`
	Reasons     map[string]int `json:"reasons"`
	RefreshedAt time.Time      `json:"refreshed_at"`
	DurationMS  int64          `json:"duration_ms"`
}

// DecideExcludeReason 是 SQL CASE 的 Go 镜像，供单测锁定优先级。
func DecideExcludeReason(openDebt, recentAdvance, recentSettlement bool) string {
	switch {
	case openDebt:
		return models.ExcludeReasonOpenDebt
	case recentAdvance:
		return models.ExcludeReasonRecentAdvance
	case recentSettlement:
		return models.ExcludeReasonRecentSettlement
	default:
		return ""
	}
}

// scanBillingCreditExcludes 只在计费库读：返回「账户 -> 主因」。
func scanBillingCreditExcludes(n9e *ctx.Context, settings models.BalanceAlertSettings, windowDays int) (map[string]string, error) {
	store, err := OpenStore(n9e, settings)
	if err != nil {
		return nil, err
	}
	return store.ScanCreditExcludes(context.Background(), windowDays)
}

// RefreshCreditExcludes 是全量刷新的唯一入口：cron、启动补刷与参数页手动刷新都走它，
// 这样失败计数、日志和运营告警只有一处实现。
func RefreshCreditExcludes(n9e *ctx.Context, trigger string) (ExcludeRefreshResult, error) {
	settings, err := models.BalanceAlertSettingsGet(n9e)
	if err != nil {
		return ExcludeRefreshResult{}, err
	}
	window := settings.CreditAdvanceWindowDays
	if window <= 0 {
		window = models.DefaultCreditAdvanceWindowDays
	}

	out := ExcludeRefreshResult{Trigger: trigger, WindowDays: window, Reasons: map[string]int{}}
	started := time.Now()

	refreshErr := func() error {
		found, err := scanBillingCreditExcludes(n9e, settings, window)
		if err != nil {
			return err
		}
		now := time.Now()
		rows := make([]models.BalanceAlertCreditExclude, 0, len(found))
		reasons := make(map[string]int, 3)
		for id, reason := range found {
			rows = append(rows, models.BalanceAlertCreditExclude{
				BillingAccountID: id,
				Reason:           reason,
				RefreshedAt:      now,
			})
			reasons[reason]++
		}
		if err := models.BalanceAlertCreditExcludeReplace(n9e, rows); err != nil {
			return fmt.Errorf("replace exclude list: %w", err)
		}
		out.Total = len(rows)
		out.Reasons = reasons
		out.RefreshedAt = now
		out.DurationMS = time.Since(started).Milliseconds()
		return nil
	}()

	now := time.Now()
	prev, _ := models.BalanceAlertExcludeStateGet(n9e)
	state := prev
	state.LastAttemptAt = &now
	state.LastTrigger = trigger
	state.WindowDays = window
	state.DurationMS = time.Since(started).Milliseconds()

	if refreshErr != nil {
		state.ConsecutiveFailures = prev.ConsecutiveFailures + 1
		state.LastError = refreshErr.Error()
		logger.Errorf("balancealert exclude refresh (%s) failed: %v (consecutive=%d, last_success=%v)",
			trigger, refreshErr, state.ConsecutiveFailures, prev.LastSuccessAt)
		if err := models.BalanceAlertExcludeStatePut(n9e, state, "balancealert"); err != nil {
			logger.Errorf("balancealert exclude state: %v", err)
		}
		if state.NeedsOpsAlert() && settings.AlertEnabled() {
			notifyExcludeRefreshFailure(settings, state)
		}
		return out, refreshErr
	}

	// 先提交名单、再记成功：反过来的话崩溃窗口会留下「状态说刷新成功、名单还是旧的」。
	state.LastSuccessAt = &now
	state.LastError = ""
	state.ConsecutiveFailures = 0
	state.Total = out.Total
	state.Reasons = out.Reasons
	if err := models.BalanceAlertExcludeStatePut(n9e, state, "balancealert"); err != nil {
		logger.Errorf("balancealert exclude state: %v", err)
	}

	logger.Infof("balancealert exclude refresh (%s): total=%d open_debt=%d recent_advance=%d recent_settlement=%d window=%dd elapsed=%dms",
		trigger, out.Total,
		out.Reasons[models.ExcludeReasonOpenDebt],
		out.Reasons[models.ExcludeReasonRecentAdvance],
		out.Reasons[models.ExcludeReasonRecentSettlement],
		window, out.DurationMS)
	return out, nil
}

// applyCreditExcludes 用名单过滤预付费候选，返回过滤后的账户与排除数。
//
// fail closed：从未成功刷新过名单时拒绝圈人。把空表当「无人需要排除」会把全部
// 授信企业拉进预警池，比不发预警危险得多。
func applyCreditExcludes(n9e *ctx.Context, accounts []Account) ([]Account, int, error) {
	state, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		return nil, 0, fmt.Errorf("read credit exclude refresh state: %w", err)
	}
	if !state.Ready() {
		return nil, 0, fmt.Errorf("credit exclude list never refreshed; fail closed (last attempt=%v last error=%q)",
			state.LastAttemptAt, state.LastError)
	}
	excluded, err := models.BalanceAlertCreditExcludeIDs(n9e)
	if err != nil {
		return nil, 0, fmt.Errorf("load credit exclude list: %w", err)
	}
	if len(excluded) == 0 {
		return accounts, 0, nil
	}
	kept := make([]Account, 0, len(accounts))
	skipped := 0
	for _, a := range accounts {
		if _, ok := excluded[a.ID]; ok {
			skipped++
			continue
		}
		kept = append(kept, a)
	}
	return kept, skipped, nil
}

// notifyExcludeRefreshFailure 尽力而为地喊运营：试点期通常配了 legacy DingTalk
// webhook；只配通知规则的环境由参数页状态与错误日志兜底。
func notifyExcludeRefreshFailure(settings models.BalanceAlertSettings, state models.BalanceAlertExcludeState) {
	lastSuccess := "从未成功"
	if state.LastSuccessAt != nil {
		lastSuccess = state.LastSuccessAt.In(locShanghai).Format("2006-01-02 15:04:05")
	}
	text := fmt.Sprintf("【预付费余额预警】排除名单刷新失败（连续 %d 次）\n上次成功：%s\n当前处置：沿用旧名单；若无成功记录则已停止圈选\n错误：%s",
		state.ConsecutiveFailures, lastSuccess, state.LastError)

	if len(settings.PilotReceivers) == 0 {
		logger.Warningf("balancealert exclude refresh alert has no pilot webhook configured: %s", text)
		return
	}
	hc := DefaultHTTP()
	for _, u := range settings.PilotReceivers {
		signed, err := SignDingTalkRobotURL(u, time.Now())
		if err != nil || signed == "" {
			logger.Warningf("balancealert exclude refresh alert: %v", err)
			continue
		}
		if err := hc.PostJSON(signed, map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": text},
		}); err != nil {
			logger.Warningf("balancealert exclude refresh alert delivery: %v", err)
		}
	}
}
