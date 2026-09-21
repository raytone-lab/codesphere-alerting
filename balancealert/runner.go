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

type Stats struct {
	Prepaid       int      `json:"prepaid"`
	SkippedIncome int      `json:"skipped_income"`
	Evaluated     int      `json:"evaluated"`
	Sent          int      `json:"sent"`
	Cooldown      int      `json:"cooldown"`
	Failed        int      `json:"failed"`
	Errors        []string `json:"errors,omitempty"`
}

type Runner struct {
	Ctx   *ctx.Context
	Store Store
	HTTP  HTTPClient
	Now   func() time.Time
}

var runMu sync.Mutex

func RunOnce(n9e *ctx.Context, httpClient HTTPClient) (Stats, error) {
	runMu.Lock()
	defer runMu.Unlock()

	r := &Runner{Ctx: n9e, HTTP: httpClient, Now: time.Now}
	return r.run(context.Background())
}

func (r *Runner) run(std context.Context) (Stats, error) {
	var stats Stats
	settings, err := models.BalanceAlertSettingsGet(r.Ctx)
	if err != nil {
		return stats, err
	}

	store := r.Store
	if store == nil {
		store, err = OpenStore(r.Ctx, settings)
		if err != nil {
			return stats, err
		}
	}

	accounts, err := store.ListPrepaid(std)
	if err != nil {
		return stats, fmt.Errorf("list prepaid enterprises: %w", err)
	}
	stats.Prepaid = len(accounts)

	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	httpClient := r.HTTP
	if httpClient == nil {
		httpClient = DefaultHTTP()
	}

	for _, acct := range accounts {
		if err := r.evalOne(acct, settings, now, httpClient, &stats); err != nil {
			msg := fmt.Sprintf("%s: %v", acct.ID, err)
			logger.Errorf("balancealert: %s", msg)
			stats.Errors = append(stats.Errors, msg)
		}
	}
	return stats, nil
}

func (r *Runner) evalOne(acct Account, settings models.BalanceAlertSettings, now time.Time, httpClient HTTPClient, stats *Stats) error {
	th, mode, skip := ComputeThreshold(acct.LastRecharge, acct.HasVoucher, settings.VoucherThresholdUSD)
	if skip {
		stats.SkippedIncome++
		return nil
	}

	cfg, err := models.BalanceAlertConfigGet(r.Ctx, acct.ID)
	if err != nil {
		return err
	}
	if cfg != nil && !cfg.Enabled {
		return nil
	}
	if cfg == nil {
		cfg = &models.BalanceAlertConfig{
			BillingAccountID: acct.ID,
			Enabled:          true,
			CurrentState:     StateNormal,
			StateSince:       &now,
		}
	}

	warnAt, err := models.BalanceAlertRecordLastSentAt(r.Ctx, acct.ID, StateWarn)
	if err != nil {
		return err
	}
	critAt, err := models.BalanceAlertRecordLastSentAt(r.Ctx, acct.ID, StateCritical)
	if err != nil {
		return err
	}

	d := Decide(cfg.CurrentState, acct.Balance, th, now, SendHistory{WarnAt: warnAt, CriticalAt: critAt})
	if d.NextState != cfg.CurrentState {
		cfg.CurrentState = d.NextState
		cfg.StateSince = &now
	}
	if err := models.BalanceAlertConfigUpsert(r.Ctx, cfg); err != nil {
		return err
	}

	if d.Status == "" {
		return nil
	}
	stats.Evaluated++

	rec := &models.BalanceAlertRecord{
		BillingAccountID: acct.ID,
		Level:            d.NextState,
		BalanceUSD:       acct.Balance,
		ThresholdUSD:     th,
		ThresholdMode:    mode,
		SendMode:         settings.SendMode,
		Status:           d.Status,
	}
	if d.SendLevel != "" {
		rec.Level = d.SendLevel
	}

	if d.Status == StatusSkippedCooldown {
		stats.Cooldown++
		return models.BalanceAlertRecordInsert(r.Ctx, rec)
	}

	if d.Status != StatusSent {
		return models.BalanceAlertRecordInsert(r.Ctx, rec)
	}

	sendRes := Dispatch(httpClient, SendRequest{
		Mode:          settings.SendMode,
		Level:         rec.Level,
		ThresholdMode: mode,
		Name:          acct.Name,
		Balance:       acct.Balance,
		Threshold:     th,
		Phone:         acct.Phone,
		PilotURLs:     settings.PilotReceivers,
		SmsURL:        settings.SmsWebhook,
	})
	rec.Channel = sendRes.Channel
	rec.Receiver = sendRes.Receiver
	if sendRes.Err != nil && settings.SendMode != SendModeOff && settings.SendMode != "" {
		rec.Status = StatusFailed
		stats.Failed++
		if err := models.BalanceAlertRecordInsert(r.Ctx, rec); err != nil {
			return err
		}
		return sendRes.Err
	}

	sentAt := now
	if settings.SendMode != SendModeOff && settings.SendMode != "" {
		rec.SentAt = &sentAt
	}
	cfg.LastAlertAt = &sentAt
	if err := models.BalanceAlertConfigUpsert(r.Ctx, cfg); err != nil {
		return err
	}
	stats.Sent++
	return models.BalanceAlertRecordInsert(r.Ctx, rec)
}
