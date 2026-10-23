package balancealert

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/toolkits/pkg/logger"
)

// excludeMu 串行化对刷新状态（一份 JSON blob）的读改写。
//
// 状态是整份 Put，不是字段级 Update：刷新失败记账与「开关恢复后补报」若并发，
// 后写的一方会把手里的旧快照覆盖回去（丢成功时间 / 丢失败计数）。进程内用
// 互斥兜住；多副本之间仍是最后写入者胜，这一点与刷新本身一致，已记录在案。
var excludeMu sync.Mutex

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
//
// 三段条件是**互相独立的排除条件**，不是彼此的冗余校验。尤其是 settlements 那一段：
// 企业可能在最后一次垫付超窗之后才做对账代充（信用核销），此时垫付条件不命中，
// 但它当下仍按信用账期结算、并会在窗口内继续产生新垫付。删掉这一段会让刚结清账的
// 企业立刻回到预警池、收到误报。因此排除期的起算点是「最后一次垫付或最后一次核销
// 中较晚者」，实际排除期可能长于「最后垫付 + N 天」。见 ADR 0002 与 TC-14。
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

// 刷新触发方。MANUAL 刻意不投递运营告警：点按钮的人就在页面上，再推一条钉钉
// 只是重复告知；告警留给没人盯着的定时任务。
const (
	TriggerStartup = "STARTUP"
	TriggerCron    = "CRON"
	TriggerManual  = "MANUAL"
)

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
		// 连参数都读不出来时也要记账，否则「刷新已经坏了」只留在日志里，页面上
		// 永远显示上一个成功时间。没有 settings 就没有收件人，只能页面兜底。
		recordExcludeSettingsFailure(n9e, trigger, err)
		return ExcludeRefreshResult{Trigger: trigger}, err
	}
	window := settings.WindowDays()

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
	excludeMu.Lock()
	defer excludeMu.Unlock()
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
		// 只有定时任务/启动补刷喊人；手动刷新的错误回显在页面上。
		// 告警开关关闭时不投递、也不推进 LastNotifiedFailures，开关打开后仍会补报。
		// 投递失败（没配 webhook 或 HTTP 失败）同样不推进：没送到的消息不算通知过。
		if trigger != TriggerManual && settings.AlertEnabled() && state.ShouldNotifyFailure() {
			if notifyExcludeRefreshFailure(settings, state) {
				state.LastNotifiedFailures = state.ConsecutiveFailures
			}
		}
		if err := models.BalanceAlertExcludeStatePut(n9e, state, "balancealert"); err != nil {
			logger.Errorf("balancealert exclude state: %v", err)
		}
		return out, refreshErr
	}

	// 先提交名单、再记成功：反过来的话崩溃窗口会留下「状态说刷新成功、名单还是旧的」。
	state.LastSuccessAt = &now
	state.LastError = ""
	state.ConsecutiveFailures = 0
	state.Total = out.Total
	state.Reasons = out.Reasons
	// 恢复通知只在「上一次失败已经报过警」时补发，避免把偶发抖动也变成消息。
	// 发送成功才清零；开关关着或投递失败时保留计数，交给补报去还这笔账。
	if prev.NeedsRecoveryNotice() &&
		settings.AlertEnabled() &&
		notifyExcludeRefreshRecovery(settings, prev.ConsecutiveFailures, out) {
		state.LastNotifiedFailures = 0
	}
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

// recordExcludeSettingsFailure 在连参数都读不出来时记一次失败。
//
// 读不到 settings 就没有收件人、也没有窗口，只能把「尝试过、失败了」落到状态里，
// 让参数页不再显示一个看似正常的上次成功时间。
func recordExcludeSettingsFailure(n9e *ctx.Context, trigger string, cause error) {
	excludeMu.Lock()
	defer excludeMu.Unlock()
	prev, _ := models.BalanceAlertExcludeStateGet(n9e)
	now := time.Now()
	prev.LastAttemptAt = &now
	prev.LastTrigger = trigger
	prev.LastError = fmt.Sprintf("read settings: %v", cause)
	prev.ConsecutiveFailures++
	logger.Errorf("balancealert exclude refresh (%s) cannot read settings: %v", trigger, cause)
	if err := models.BalanceAlertExcludeStatePut(n9e, prev, "balancealert"); err != nil {
		logger.Errorf("balancealert exclude state: %v", err)
	}
}

// notifyExcludeRefreshFailure 尽力而为地喊运营：试点期通常配了 legacy DingTalk
// webhook；只配通知规则的环境由参数页状态与错误日志兜底。返回是否真的送到。
//
// 两种失败分开说，因为处置完全不同：从未成功意味着当轮已经停止圈选（整条链路
// 不可用），已有成功名单只是沿用旧数据（功能降级）。用同一句话会让前者看起来
// 像后者，值班的人就不会去救它。
func notifyExcludeRefreshFailure(settings models.BalanceAlertSettings, state models.BalanceAlertExcludeState) bool {
	if !state.Ready() {
		text := fmt.Sprintf("【预付费余额预警】排除名单从未成功刷新，已停止圈选（连续失败 %d 次）\n当前处置：fail closed，不会发出任何余额预警\n错误：%s",
			state.ConsecutiveFailures, state.LastError)
		return sendExcludeOpsNotice(settings, text)
	}
	lastSuccess := "从未成功"
	if state.LastSuccessAt != nil {
		lastSuccess = state.LastSuccessAt.In(locShanghai).Format("2006-01-02 15:04:05")
	}
	text := fmt.Sprintf("【预付费余额预警】排除名单刷新失败（连续 %d 次）\n上次成功：%s\n当前处置：沿用旧名单，预警继续\n错误：%s",
		state.ConsecutiveFailures, lastSuccess, state.LastError)
	return sendExcludeOpsNotice(settings, text)
}

// notifyExcludeRefreshRecovery 与失败告警成对：失败只在第 2 次报过一次，恢复时
// 必须补一条，否则群里只看见「坏了」看不见「好了」。
//
// 刻意不列 reason 中文译名：译名只应存在运营页一处，通知里只报总数。
func notifyExcludeRefreshRecovery(settings models.BalanceAlertSettings, failures int, res ExcludeRefreshResult) bool {
	text := fmt.Sprintf("【预付费余额预警】排除名单刷新已恢复（此前连续失败 %d 次）\n本次成功：%s 刷新 %d 家",
		failures, res.RefreshedAt.In(locShanghai).Format("2006-01-02 15:04:05"), res.Total)
	return sendExcludeOpsNotice(settings, text)
}

// NotifyPendingExcludeOps 在告警总开关从「停用」切回「启用」时补报。
//
// 两种待办都要还：还没通知过的失败（ShouldNotifyFailure），以及已经喊过「坏了」
// 但恢复发生在开关关闭期间的恢复通知（ConsecutiveFailures 已归零、
// LastNotifiedFailures 仍非零）。少了后者，群里会永远停在「坏了」。
func NotifyPendingExcludeOps(n9e *ctx.Context) {
	settings, err := models.BalanceAlertSettingsGet(n9e)
	if err != nil || !settings.AlertEnabled() {
		return
	}
	excludeMu.Lock()
	defer excludeMu.Unlock()
	state, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		return
	}

	var delivered bool
	switch {
	case state.ShouldNotifyFailure():
		delivered = notifyExcludeRefreshFailure(settings, state)
		if delivered {
			state.LastNotifiedFailures = state.ConsecutiveFailures
		}
	case state.NeedsRecoveryNotice() && state.ConsecutiveFailures == 0:
		res := ExcludeRefreshResult{Total: state.Total, Reasons: state.Reasons, WindowDays: state.WindowDays}
		if state.LastSuccessAt != nil {
			res.RefreshedAt = *state.LastSuccessAt
		}
		delivered = notifyExcludeRefreshRecovery(settings, state.LastNotifiedFailures, res)
		if delivered {
			state.LastNotifiedFailures = 0
		}
	default:
		return
	}
	if !delivered {
		return
	}
	if err := models.BalanceAlertExcludeStatePut(n9e, state, "balancealert"); err != nil {
		logger.Errorf("balancealert exclude state: %v", err)
	}
}

// NotifyPendingExcludeOpsAsync 用于请求路径（配置保存后补报）。
//
// 补报是尽力而为：不能因为一次 webhook 网络错误或状态写入失败，把一次成功的
// 配置保存变成 500，也不能让 panic 掀掉整个进程。
func NotifyPendingExcludeOpsAsync(n9e *ctx.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("balancealert exclude pending notice panic: %v", r)
			}
		}()
		NotifyPendingExcludeOps(n9e)
	}()
}

// sendExcludeOpsNotice 是失败/恢复共用的投递实现：逐条 webhook 尽力发送，
// 单条失败只记日志，不影响刷新结果与状态落库。返回是否至少送到一条。
func sendExcludeOpsNotice(settings models.BalanceAlertSettings, text string) bool {
	if len(settings.PilotReceivers) == 0 {
		logger.Warningf("balancealert exclude refresh alert has no pilot webhook configured: %s", text)
		return false
	}
	hc := DefaultHTTP()
	delivered := false
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
			continue
		}
		delivered = true
	}
	return delivered
}
