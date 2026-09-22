package models

import (
	"encoding/json"
	"time"

	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

const (
	BalanceAlertSettingsKey = "balance_alert_settings"

	BalanceAlertStateNormal   = "NORMAL"
	BalanceAlertStateWarn     = "WARN"
	BalanceAlertStateCritical = "CRITICAL"

	ThresholdModeAuto  = "AUTO"
	ThresholdModeCustom = "CUSTOM"

	TriggerTypeStatic  = "STATIC"
	TriggerTypeDynamic = "DYNAMIC"
)

type BalanceAlertConfig struct {
	BillingAccountID string     `json:"billing_account_id" gorm:"primaryKey;type:varchar(128)"`
	Enabled          bool       `json:"enabled" gorm:"not null;default:true"`
	CurrentState     string     `json:"current_state" gorm:"type:varchar(16);not null;default:NORMAL"`
	StateSince       *time.Time `json:"state_since"`
	LastAlertAt      *time.Time `json:"last_alert_at"`
	ThresholdMode    string     `json:"threshold_mode" gorm:"type:varchar(16);not null;default:AUTO"`
	ThresholdFixedUSD float64   `json:"threshold_fixed_usd" gorm:"type:numeric;not null;default:0"`
	Receivers        string     `json:"receivers" gorm:"type:text"`
	DynamicEnabled   bool       `json:"dynamic_enabled" gorm:"not null;default:false"`
	CreatedAt        time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt        time.Time  `json:"updated_at" gorm:"not null"`
}

func (BalanceAlertConfig) TableName() string {
	return "balance_alert_configs"
}

func (c *BalanceAlertConfig) ReceiverList() []string {
	if c.Receivers == "" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(c.Receivers), &list); err != nil {
		return nil
	}
	return list
}

func (c *BalanceAlertConfig) SetReceiverList(list []string) {
	if len(list) == 0 {
		c.Receivers = ""
		return
	}
	b, _ := json.Marshal(list)
	c.Receivers = string(b)
}

func (c *BalanceAlertConfig) IsCustomThreshold() bool {
	return c.ThresholdMode == ThresholdModeCustom && c.ThresholdFixedUSD > 0
}

type BalanceAlertRecord struct {
	ID               string     `json:"id" gorm:"primaryKey;type:varchar(64)"`
	BillingAccountID string     `json:"billing_account_id" gorm:"type:varchar(128);not null;index:idx_balance_alert_account_created,priority:1"`
	Level            string     `json:"level" gorm:"type:varchar(16);not null"`
	BalanceUSD       float64    `json:"balance_usd" gorm:"type:numeric;not null"`
	ThresholdUSD     float64    `json:"threshold_usd" gorm:"type:numeric;not null"`
	ThresholdMode    string     `json:"threshold_mode" gorm:"type:varchar(32);not null"`
	SendMode         string     `json:"send_mode" gorm:"type:varchar(16);not null"`
	Status           string     `json:"status" gorm:"type:varchar(32);not null"`
	Channel          string     `json:"channel" gorm:"type:varchar(16)"`
	Receiver         string     `json:"receiver" gorm:"type:varchar(255)"`
	SentAt           *time.Time `json:"sent_at"`
	OperatorReview   string     `json:"operator_review" gorm:"type:varchar(32)"`
	TriggerType      string     `json:"trigger_type" gorm:"type:varchar(16);not null;default:STATIC"`
	CreatedAt        time.Time  `json:"created_at" gorm:"not null;index:idx_balance_alert_account_created,priority:2"`
}

func (BalanceAlertRecord) TableName() string {
	return "balance_alert_records"
}

type BalanceAlertSettings struct {
	SendMode             string   `json:"send_mode"`
	PilotReceivers       []string `json:"pilot_receivers,omitempty"` // legacy; prefer PilotNotifyRuleID
	VoucherThresholdUSD  float64  `json:"voucher_threshold_usd"`
	DatasourceID         int64    `json:"datasource_id"`
	SmsWebhook           string   `json:"sms_webhook,omitempty"` // legacy; prefer CustomerNotifyRuleID
	CustomerPhoneSQLHint string   `json:"customer_phone_sql_hint,omitempty"`
	PilotNotifyRuleID    int64    `json:"pilot_notify_rule_id"`
	CustomerNotifyRuleID int64    `json:"customer_notify_rule_id"`
	DynamicNotifyRuleID  int64    `json:"dynamic_notify_rule_id"`
}

func DefaultBalanceAlertSettings() BalanceAlertSettings {
	return BalanceAlertSettings{
		SendMode:            "OFF",
		PilotReceivers:      []string{},
		VoucherThresholdUSD: 20,
	}
}

func BalanceAlertSettingsGet(ctx *ctx.Context) (BalanceAlertSettings, error) {
	s := DefaultBalanceAlertSettings()
	val, err := ConfigsGet(ctx, BalanceAlertSettingsKey)
	if err != nil {
		return s, err
	}
	if val == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(val), &s); err != nil {
		return s, errors.WithMessage(err, "decode balance_alert_settings")
	}
	if s.SendMode == "" {
		s.SendMode = "OFF"
	}
	if s.VoucherThresholdUSD <= 0 {
		s.VoucherThresholdUSD = 20
	}
	if s.PilotReceivers == nil {
		s.PilotReceivers = []string{}
	}
	return s, nil
}

func (s BalanceAlertSettings) Public() BalanceAlertSettings {
	return s
}

func BalanceAlertSettingsPut(ctx *ctx.Context, s BalanceAlertSettings, username string) error {
	switch s.SendMode {
	case "OFF", "PILOT", "CUSTOMER":
	case "":
		s.SendMode = "OFF"
	default:
		return errors.Errorf("invalid send_mode %s", s.SendMode)
	}
	if s.VoucherThresholdUSD <= 0 {
		s.VoucherThresholdUSD = 20
	}
	if s.PilotReceivers == nil {
		s.PilotReceivers = []string{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return ConfigsSetWithUname(ctx, BalanceAlertSettingsKey, string(b), username)
}

func BalanceAlertConfigGets(ctx *ctx.Context, limit int) ([]*BalanceAlertConfig, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var lst []*BalanceAlertConfig
	err := DB(ctx).Order("updated_at desc").Limit(limit).Find(&lst).Error
	return lst, err
}

func NewBalanceAlertRecordID() string {
	return uuid.NewString()
}

func BalanceAlertConfigGet(ctx *ctx.Context, accountID string) (*BalanceAlertConfig, error) {
	var cfg BalanceAlertConfig
	err := DB(ctx).Where("billing_account_id = ?", accountID).First(&cfg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func BalanceAlertConfigUpsert(ctx *ctx.Context, cfg *BalanceAlertConfig) error {
	now := time.Now()
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = now
	}
	cfg.UpdatedAt = now
	if cfg.CurrentState == "" {
		cfg.CurrentState = BalanceAlertStateNormal
	}
	return DB(ctx).Save(cfg).Error
}

func BalanceAlertRecordInsert(ctx *ctx.Context, rec *BalanceAlertRecord) error {
	if rec.ID == "" {
		rec.ID = NewBalanceAlertRecordID()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	return DB(ctx).Create(rec).Error
}

func BalanceAlertRecordLastSentAt(ctx *ctx.Context, accountID, level string) (time.Time, error) {
	var rec BalanceAlertRecord
	err := DB(ctx).Where("billing_account_id = ? AND level = ? AND status = ?", accountID, level, "SENT").
		Order("created_at desc").Limit(1).Take(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if rec.SentAt != nil {
		return *rec.SentAt, nil
	}
	return rec.CreatedAt, nil
}

func BalanceAlertRecordSentToday(ctx *ctx.Context, accountID, level string, dayStart time.Time) (bool, error) {
	var n int64
	err := DB(ctx).Model(&BalanceAlertRecord{}).
		Where("billing_account_id = ? AND level = ? AND status = ? AND created_at >= ?", accountID, level, "SENT", dayStart).
		Count(&n).Error
	return n > 0, err
}

func BalanceAlertRecordGets(ctx *ctx.Context, accountID, level, status string, limit int) ([]*BalanceAlertRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := DB(ctx).Model(&BalanceAlertRecord{}).Order("created_at desc").Limit(limit)
	if accountID != "" {
		q = q.Where("billing_account_id = ?", accountID)
	}
	if level != "" {
		q = q.Where("level = ?", level)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var lst []*BalanceAlertRecord
	err := q.Find(&lst).Error
	return lst, err
}

func BalanceAlertRecordSetReview(ctx *ctx.Context, id, review string) error {
	return DB(ctx).Model(&BalanceAlertRecord{}).Where("id = ?", id).Update("operator_review", review).Error
}

// BalanceAlertRecordHasLevelSince reports whether any WARN/CRITICAL record
// (any status) exists for the account since the given time. Used by miss-report recon.
func BalanceAlertRecordHasLevelSince(ctx *ctx.Context, accountID string, since time.Time) (bool, *time.Time, error) {
	var rec BalanceAlertRecord
	err := DB(ctx).Where(
		"billing_account_id = ? AND level IN (?, ?) AND created_at >= ?",
		accountID, BalanceAlertStateWarn, BalanceAlertStateCritical, since,
	).Order("created_at desc").First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	t := rec.CreatedAt
	return true, &t, nil
}

// BalanceAlertConfigPut upserts enterprise self-service fields (threshold_mode,
// threshold_fixed_usd, receivers, dynamic_enabled) without touching state machine fields.
func BalanceAlertConfigPut(ctx *ctx.Context, accountID string, cfg BalanceAlertConfig) error {
	existing, err := BalanceAlertConfigGet(ctx, accountID)
	if err != nil {
		return err
	}
	now := time.Now()
	if existing == nil {
		existing = &BalanceAlertConfig{
			BillingAccountID: accountID,
			Enabled:          true,
			CurrentState:     BalanceAlertStateNormal,
			CreatedAt:        now,
		}
	}
	existing.ThresholdMode = cfg.ThresholdMode
	existing.ThresholdFixedUSD = cfg.ThresholdFixedUSD
	existing.Receivers = cfg.Receivers
	existing.DynamicEnabled = cfg.DynamicEnabled
	existing.UpdatedAt = now
	return DB(ctx).Save(existing).Error
}
