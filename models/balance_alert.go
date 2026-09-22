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
)

type BalanceAlertConfig struct {
	BillingAccountID string     `json:"billing_account_id" gorm:"primaryKey;type:varchar(128)"`
	Enabled          bool       `json:"enabled" gorm:"not null;default:true"`
	CurrentState     string     `json:"current_state" gorm:"type:varchar(16);not null;default:NORMAL"`
	StateSince       *time.Time `json:"state_since"`
	LastAlertAt      *time.Time `json:"last_alert_at"`
	CreatedAt        time.Time  `json:"created_at" gorm:"not null"`
	UpdatedAt        time.Time  `json:"updated_at" gorm:"not null"`
}

func (BalanceAlertConfig) TableName() string {
	return "balance_alert_configs"
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
	CreatedAt        time.Time  `json:"created_at" gorm:"not null;index:idx_balance_alert_account_created,priority:2"`
}

func (BalanceAlertRecord) TableName() string {
	return "balance_alert_records"
}

type BalanceAlertSettings struct {
	SendMode             string   `json:"send_mode"`
	PilotReceivers       []string `json:"pilot_receivers"`
	VoucherThresholdUSD  float64  `json:"voucher_threshold_usd"`
	DatasourceID         int64    `json:"datasource_id"`
	SmsWebhook           string   `json:"sms_webhook"`
	CustomerPhoneSQLHint string   `json:"customer_phone_sql_hint,omitempty"`
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
