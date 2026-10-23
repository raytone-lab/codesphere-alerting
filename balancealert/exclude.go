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
	Trigger     RefreshTrigger `json:"trigger"`
	WindowDays  int            `json:"window_days"`
	Total       int            `json:"total"`
	Reasons     map[string]int `json:"reasons"`
	RefreshedAt time.Time      `json:"refreshed_at"`
	DurationMS  int64          `json:"duration_ms"`
}

// RefreshTrigger 是刷新触发方。用命名类型而不是裸 string：调度口只接受这三个
// 值，拼写错误应当在编译期就暴露，而不是变成一条永远匹配不上的 if。
type RefreshTrigger string

// 刷新触发方。MANUAL 刻意不投递运营告警：点按钮的人就在页面上，再推一条钉钉
// 只是重复告知；告警留给没人盯着的定时任务。
const (
	TriggerStartup RefreshTrigger = "STARTUP"
	TriggerCron    RefreshTrigger = "CRON"
	TriggerManual  RefreshTrigger = "MANUAL"
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
func RefreshCreditExcludes(n9e *ctx.Context, trigger RefreshTrigger) (ExcludeRefreshResult, error) {
	settings, err := models.BalanceAlertSettingsGet(n9e)
	if err != nil {
		// 连参数都读不出来时也要记账，否则「刷新已经坏了」只留在日志里，页面上
		// 永远显示上一个成功时间。没有 settings 就没有收件人，只能页面兜底。
		// 这类失败仍会累加计数：等参数恢复可读的第一次失败就会把这段停机一起报出来。
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
	prev, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		// 读不出状态就绝不整份回写：Put 会把手里的零值写回去（LastSuccessAt 变 nil），
		// Ready() 随之变假，一次读抖动就被放大成「停止圈人」。
		logger.Errorf("balancealert exclude refresh (%s): read state: %v", trigger, err)
		if refreshErr == nil {
			// 名单已经换新，但健康状态没落库：下一轮仍会 fail closed，必须当错误报出去。
			return out, fmt.Errorf("read exclude refresh state: %w", err)
		}
		return out, refreshErr
	}
	state := prev
	state.LastAttemptAt = &now
	state.LastTrigger = string(trigger)
	state.WindowDays = window
	state.DurationMS = time.Since(started).Milliseconds()

	if refreshErr != nil {
		state.LastError = refreshErr.Error()
		// 手动刷新只更新「尝试过」，不推进连续失败计数：运营连点两次按钮不该把
		// 定时任务的失败计数顶到告警阈值，否则下一条钉钉会声称「连续失败 3 次」，
		// 而实际只有一次定时刷新失败过。错误仍然落在 last_error 上，页面照常显示。
		if trigger != TriggerManual {
			state.ConsecutiveFailures = prev.ConsecutiveFailures + 1
		}
		logger.Errorf("balancealert exclude refresh (%s) failed: %v (consecutive=%d, last_success=%v)",
			trigger, refreshErr, state.ConsecutiveFailures, prev.LastSuccessAt)
		// 只有定时任务/启动补刷喊人；手动刷新的错误回显在页面上。
		// 告警开关关闭时不投递、也不推进记账，开关打开后仍会补报。
		if trigger != TriggerManual && settings.AlertEnabled() && state.ShouldNotifyFailure() {
			notifyFailureOwed(settings, &state, now)
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
	// 判据是本次成功的时刻：上一次成功早于通告时刻才是常态（先成功、后连续失败），
	// 用 prev.LastSuccessAt 去比会把正常恢复吞掉。
	// 发送成功才清账；开关关着或投递失败时保留计数，交给补报去还这笔账。
	if settings.AlertEnabled() && prev.NotifiedIncidentEndedAt(now) {
		notifyRecoveryOwed(settings, &state, incidentFailures(prev), out)
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
// 让参数页不再显示一个看似正常的上次成功时间。计数照常累加：等参数恢复可读后的
// 第一次失败就会把这段停机一起报出来，不会因为「当时没法发消息」而整段消失。
func recordExcludeSettingsFailure(n9e *ctx.Context, trigger RefreshTrigger, cause error) {
	excludeMu.Lock()
	defer excludeMu.Unlock()
	prev, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		// 状态也读不出来时不能整份回写：零值会把 LastSuccessAt 抹掉，反而制造一次
		// 假 fail closed。这类叠加故障只留在日志里，等配置可读后再记账。
		logger.Errorf("balancealert exclude refresh (%s) cannot read settings (%v) nor state: %v", trigger, cause, err)
		return
	}
	now := time.Now()
	prev.LastAttemptAt = &now
	prev.LastTrigger = string(trigger)
	prev.LastError = fmt.Sprintf("read settings: %v", cause)
	if trigger != TriggerManual {
		prev.ConsecutiveFailures++
	}
	logger.Errorf("balancealert exclude refresh (%s) cannot read settings: %v (consecutive=%d)", trigger, cause, prev.ConsecutiveFailures)
	if err := models.BalanceAlertExcludeStatePut(n9e, prev, "balancealert"); err != nil {
		logger.Errorf("balancealert exclude state: %v", err)
	}
}

// notifyFailureOwed 投递一次失败通知，**送达才推进记账**。
//
// 没送到的消息不算通知过：否则恢复通知会声称「已经告诉你们坏了」，而其实谁也没
// 收到。投递失败时保留待办，下一次失败或开关重新打开会再试。
//
// 投递与落库不是原子的（消息先出去、状态后写）：Put 失败时会重复一条，这是刻意
// 选择的方向——宁可重复，也不能让「已经送达」被记成「没送达」。
func notifyFailureOwed(settings models.BalanceAlertSettings, state *models.BalanceAlertExcludeState, at time.Time) bool {
	if !notifyExcludeRefreshFailure(settings, *state) {
		return false
	}
	state.LastNotifiedFailures = state.ConsecutiveFailures
	state.LastNotifiedAt = &at
	return true
}

// notifyRecoveryOwed 投递一次恢复通知并清账。failures 是群里最后看见的那个数字，
// 不是此刻的连续失败次数：补报场景下两者可能已经不同，说错了等于报了一条假消息。
func notifyRecoveryOwed(settings models.BalanceAlertSettings, state *models.BalanceAlertExcludeState, failures int, res ExcludeRefreshResult) bool {
	if !notifyExcludeRefreshRecovery(settings, failures, res) {
		return false
	}
	state.LastNotifiedFailures = 0
	state.LastNotifiedAt = nil
	return true
}

// incidentFailures 取「这次故障影响到的次数」：已通告次数与当前连续失败次数中较大者。
//
// 通告后若故障继续到第 3、4 次（刻意不重复发），恢复时应当说 4 次而不是 2 次；
// 而开关关闭期间先恢复又再失败时，已通告的 2 次比新的 1 次更能说明上一段故障。
func incidentFailures(s models.BalanceAlertExcludeState) int {
	if s.ConsecutiveFailures > s.LastNotifiedFailures {
		return s.ConsecutiveFailures
	}
	return s.LastNotifiedFailures
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
// 两组待办都要还，而且顺序固定：
//  1. 已经喊过「坏了」但收尾被开关耽误的恢复通知（NotifiedIncidentEnded）；
//  2. 还没通知过的失败（ShouldNotifyFailure）。
//
// 顺序不能反：恢复通知会把 LastNotifiedFailures 清零，紧随其后的新故障才有资格
// 按「连续第 2 次」重新排队。少了第 1 步，群里会永远停在「坏了」；把它和失败
// 通知做成二选一，则会让「关闭期间恢复、随后又失败」两笔账一起丢掉。
func NotifyPendingExcludeOps(n9e *ctx.Context) {
	settings, err := models.BalanceAlertSettingsGet(n9e)
	if err != nil || !settings.AlertEnabled() {
		return
	}
	excludeMu.Lock()
	defer excludeMu.Unlock()
	state, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		logger.Errorf("balancealert exclude pending notice: read state: %v", err)
		return
	}

	changed := false
	if state.NotifiedIncidentEnded() {
		res := ExcludeRefreshResult{Total: state.Total, Reasons: state.Reasons, WindowDays: state.WindowDays}
		if state.LastSuccessAt != nil {
			res.RefreshedAt = *state.LastSuccessAt
		}
		changed = notifyRecoveryOwed(settings, &state, incidentFailures(state), res)
	}
	if state.ShouldNotifyFailure() {
		changed = notifyFailureOwed(settings, &state, time.Now()) || changed
	}
	if !changed {
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
