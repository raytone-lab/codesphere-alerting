package models

import (
	"encoding/json"
	"time"

	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/pkg/errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 排除原因，与物化行 reason 一一对应。
// 同一企业多条件命中时只落主因：OPEN_DEBT > RECENT_ADVANCE > RECENT_SETTLEMENT。
const (
	ExcludeReasonOpenDebt         = "OPEN_DEBT"
	ExcludeReasonRecentAdvance    = "RECENT_ADVANCE"
	ExcludeReasonRecentSettlement = "RECENT_SETTLEMENT"
)

// ExcludeNoticeCustomer 是客户侧唯一可见的排除说明。
//
// reason 的三个细分类别只留在运营侧：对客户来说「未清垫付」「窗口内核销」既不
// 可操作也是困惑源。客户只需要知道「按信用账期结算、不发余额预警」。
const ExcludeNoticeCustomer = "当前按信用账期结算，不发送余额预警"

// BalanceAlertCreditExclude 是预付费圈选的「授信活跃」排除名单。
//
// 归属 alerting 自己的库（n9e 库），由 AutoMigrate 建表，随 alerting 一起升级：
// 计费库只被**只读**查询，不需要为一张派生缓存协调跨仓发布顺序或 GRANT。
// 无外键、无金额列：整份可重算，被误删只会在下一次刷新前放大告警面，不会改账。
type BalanceAlertCreditExclude struct {
	BillingAccountID string `json:"billing_account_id" gorm:"column:billing_account_id;type:varchar(128);primaryKey"`
	Reason           string `json:"reason" gorm:"column:reason;type:varchar(32);not null"`
	// default:now() 与 PRD 的 DDL 对齐：代码始终显式赋值，加默认值只为让新建库
	// 的结构与文档一致，不再出现「文档说有默认值、实际建表没有」的偏差。
	// 模型 tag 用 CURRENT_TIMESTAMP 而不是 now()：前者在 Postgres 与测试用的
	// sqlite 上都是合法默认值，now() 会让 sqlite 建表直接语法错误。
	RefreshedAt time.Time `json:"refreshed_at" gorm:"column:refreshed_at;not null;default:CURRENT_TIMESTAMP"`
}

func (BalanceAlertCreditExclude) TableName() string {
	return "balance_alert_credit_excludes"
}

// BalanceAlertCreditExcludeIDs 返回排除名单的账户集合，供判定轮次在内存里过滤。
// 名单是几百到几千行的量级，一次 Pluck 比在计费库做大表反连接更稳定。
func BalanceAlertCreditExcludeIDs(ctx *ctx.Context) (map[string]struct{}, error) {
	var ids []string
	if err := DB(ctx).Model(&BalanceAlertCreditExclude{}).Pluck("billing_account_id", &ids).Error; err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

// BalanceAlertCreditExcludeGet 查单个账户是否在名单内，并返回其排除原因。
//
// 自助页只需要「是/否」加一句客户可读文案，不必把整份名单拉进内存；判定轮次仍走
// BalanceAlertCreditExcludeIDs 的批量路径。
func BalanceAlertCreditExcludeGet(ctx *ctx.Context, accountID string) (string, bool, error) {
	if accountID == "" {
		return "", false, nil
	}
	var row BalanceAlertCreditExclude
	err := DB(ctx).Where("billing_account_id = ?", accountID).Limit(1).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.Reason, true, nil
}

// BalanceAlertCreditExcludeReplace 在**单个事务**内整份替换名单。
//
// PostgreSQL 的 MVCC 保证并发读者只会看到「完整旧名单」或「完整新名单」；
// 先提交 DELETE 再 INSERT 会露出空表窗口，那一刻全部授信企业都会被当预付费。
// 不加跨实例锁：整份替换幂等，多副本并发刷最终都会收敛到某一份完整名单。
func BalanceAlertCreditExcludeReplace(ctx *ctx.Context, rows []BalanceAlertCreditExclude) error {
	return DB(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).
			Where("1 = 1").Delete(&BalanceAlertCreditExclude{}).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).
			CreateInBatches(rows, 500).Error
	})
}

func BalanceAlertCreditExcludeReasonCounts(ctx *ctx.Context) (map[string]int, int, error) {
	var out []struct {
		Reason string
		N      int
	}
	err := DB(ctx).Model(&BalanceAlertCreditExclude{}).
		Select("reason, count(*) as n").
		Group("reason").
		Scan(&out).Error
	if err != nil {
		return nil, 0, err
	}
	counts := make(map[string]int, len(out))
	total := 0
	for _, row := range out {
		counts[row.Reason] = row.N
		total += row.N
	}
	return counts, total, nil
}

// 排除名单刷新状态落在 n9e configs（不是计费库）：它描述 alerting 自己的
// 任务健康度，必须跨进程重启保留。否则「从未成功刷新」会被误读成「名单为空但
// 可用」，把全部授信企业当预付费，直接制造误报。
const BalanceAlertExcludeStateKey = "balance_alert_exclude_refresh"

type BalanceAlertExcludeState struct {
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastTrigger   string     `json:"last_trigger,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	// ConsecutiveFailures 是连续失败次数；LastNotifiedFailures 是「已通知到第几次失败」。
	//
	// 必须分开记：只靠时间戳推断「通知有没有发出去」不可靠，告警开关关闭期间失败
	// 照常累加但不投递，用单一时戳会让重新打开开关后再也报不出来。
	ConsecutiveFailures  int            `json:"consecutive_failures"`
	LastNotifiedFailures int            `json:"last_notified_failures,omitempty"`
	WindowDays           int            `json:"window_days,omitempty"`
	Total                int            `json:"total"`
	Reasons              map[string]int `json:"reasons,omitempty"`
	DurationMS           int64          `json:"duration_ms,omitempty"`
}

// Ready 表示可以可信地用排除名单圈人。从未成功刷新过时必须 fail closed。
func (s BalanceAlertExcludeState) Ready() bool {
	return s.LastSuccessAt != nil
}

// ExcludeOpsAlertAfterFailures 连续失败达到该次数时首次喊人（仅限「已有成功名单」）。
//
// 刷新每天只跑一次：若之后每次失败都发，一次持续一周的故障就是七条重复钉钉，
// 运营会开始忽略它。恢复时另发一条，所以中途不需要重复提醒。
const ExcludeOpsAlertAfterFailures = 2

// ShouldNotifyFailure 决定这次失败要不要喊运营。
//
// 两种情形力度不同：
//   - 从未成功刷新：整条链路不可用、当轮已停止圈选，属于最不该省消息的场景，
//     每次失败都要喊；告警开关关着时不推进 LastNotifiedFailures，重新打开后
//     第一次失败就会补报。
//   - 已有成功名单：只是「仍在用旧名单」的功能降级，第 2 次失败喊一次即止，
//     LastNotifiedFailures 一旦非零就不再重复，直到恢复时清零。
func (s BalanceAlertExcludeState) ShouldNotifyFailure() bool {
	if !s.Ready() {
		return s.ConsecutiveFailures > s.LastNotifiedFailures
	}
	return s.LastNotifiedFailures == 0 && s.ConsecutiveFailures >= ExcludeOpsAlertAfterFailures
}

// NeedsRecoveryNotice 上一次失败确实已经喊过人，本次成功才需要回一条恢复通知。
func (s BalanceAlertExcludeState) NeedsRecoveryNotice() bool {
	return s.LastNotifiedFailures > 0
}

func BalanceAlertExcludeStateGet(ctx *ctx.Context) (BalanceAlertExcludeState, error) {
	var s BalanceAlertExcludeState
	val, err := ConfigsGet(ctx, BalanceAlertExcludeStateKey)
	if err != nil {
		return s, err
	}
	if val == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(val), &s); err != nil {
		return s, errors.WithMessage(err, "decode balance_alert_exclude_refresh")
	}
	return s, nil
}

func BalanceAlertExcludeStatePut(ctx *ctx.Context, s BalanceAlertExcludeState, username string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return ConfigsSetWithUname(ctx, BalanceAlertExcludeStateKey, string(b), username)
}
