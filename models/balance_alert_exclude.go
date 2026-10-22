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

// BalanceAlertCreditExclude 是预付费圈选的「授信活跃」排除名单。
//
// 归属 alerting 自己的库（n9e 库），由 AutoMigrate 建表，随 alerting 一起升级：
// 计费库只被**只读**查询，不需要为一张派生缓存协调跨仓发布顺序或 GRANT。
// 无外键、无金额列：整份可重算，被误删只会在下一次刷新前放大告警面，不会改账。
type BalanceAlertCreditExclude struct {
	BillingAccountID string    `json:"billing_account_id" gorm:"column:billing_account_id;type:varchar(128);primaryKey"`
	Reason           string    `json:"reason" gorm:"column:reason;type:varchar(32);not null"`
	RefreshedAt      time.Time `json:"refreshed_at" gorm:"column:refreshed_at;not null"`
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
	LastSuccessAt       *time.Time     `json:"last_success_at,omitempty"`
	LastAttemptAt       *time.Time     `json:"last_attempt_at,omitempty"`
	LastTrigger         string         `json:"last_trigger,omitempty"`
	LastError           string         `json:"last_error,omitempty"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	WindowDays          int            `json:"window_days,omitempty"`
	Total               int            `json:"total"`
	Reasons             map[string]int `json:"reasons,omitempty"`
	DurationMS          int64          `json:"duration_ms,omitempty"`
}

// Ready 表示可以可信地用排除名单圈人。从未成功刷新过时必须 fail closed。
func (s BalanceAlertExcludeState) Ready() bool {
	return s.LastSuccessAt != nil
}

// NeedsOpsAlert 对齐 Q9：从未成功（当轮已 fail closed）或连续 ≥2 次失败都要喊人，
// 否则线上会安静地一直用旧名单。
func (s BalanceAlertExcludeState) NeedsOpsAlert() bool {
	return !s.Ready() || s.ConsecutiveFailures >= 2
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
