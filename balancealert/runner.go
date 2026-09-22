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
	Ctx    *ctx.Context
	Store  Store
	HTTP   HTTPClient
	Sender Sender
	Now    func() time.Time
}

var (
	runMu      sync.Mutex
	defaultSnd Sender
)

// SetDefaultSender installs the process-wide sender used by cron / RunOnce.
func SetDefaultSender(s Sender) {
	defaultSnd = s
}

func RunOnce(n9e *ctx.Context, httpClient HTTPClient) (Stats, error) {
	runMu.Lock()
	defer runMu.Unlock()

	r := &Runner{Ctx: n9e, HTTP: httpClient, Sender: defaultSnd, Now: time.Now}
	return r.run(context.Background())
}

func RunOnceDynamic(n9e *ctx.Context, httpClient HTTPClient) (Stats, error) {
	runMu.Lock()
	defer runMu.Unlock()

	r := &Runner{Ctx: n9e, HTTP: httpClient, Sender: defaultSnd, Now: time.Now}
	return r.runDynamic(context.Background())
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

	consumptionList, err := store.ListConsumption(std)
	if err != nil {
		logger.Warningf("balancealert: list consumption: %v (dynamic alerts may not work)", err)
	}
	consumptionMap := make(map[string]*Consumption, len(consumptionList))
	for i := range consumptionList {
		c := consumptionList[i]
		consumptionMap[c.AccountID] = &c
	}

	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	httpClient := r.HTTP
	if httpClient == nil {
		httpClient = DefaultHTTP()
	}
	snd := r.Sender
	if snd == nil {
		snd = HTTPSender{HTTP: httpClient}
	}

	for _, acct := range accounts {
		consumption := consumptionMap[acct.ID]
		if err := r.evalOne(acct, settings, consumption, now, snd, &stats); err != nil {
			msg := fmt.Sprintf("%s: %v", acct.ID, err)
			logger.Errorf("balancealert: %s", msg)
			stats.Errors = append(stats.Errors, msg)
		}
	}
	return stats, nil
}

func (r *Runner) runDynamic(std context.Context) (Stats, error) {
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

	consumptionList, err := store.ListConsumption(std)
	if err != nil {
		return stats, fmt.Errorf("list consumption: %w", err)
	}
	consumptionMap := make(map[string]*Consumption, len(consumptionList))
	for i := range consumptionList {
		c := consumptionList[i]
		consumptionMap[c.AccountID] = &c
	}

	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	snd := r.Sender
	if snd == nil {
		snd = HTTPSender{HTTP: DefaultHTTP()}
	}

	for _, acct := range accounts {
		cfg, err := models.BalanceAlertConfigGet(r.Ctx, acct.ID)
		if err != nil {
			logger.Errorf("balancealert dynamic: %s: %v", acct.ID, err)
			continue
		}
		if cfg == nil || !cfg.Enabled || !cfg.DynamicEnabled {
			continue
		}
		consumption := consumptionMap[acct.ID]
		if consumption == nil {
			continue
		}
		r.evalDynamic(acct, cfg, consumption, settings, now, snd, &stats)
	}
	return stats, nil
}

func (r *Runner) evalOne(acct Account, settings models.BalanceAlertSettings, consumption *Consumption, now time.Time, snd Sender, stats *Stats) error {
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

	// Static threshold: custom overrides auto (PRD 10.1).
	th, mode, skip := ComputeThreshold(acct.LastRecharge, acct.HasVoucher, settings.VoucherThresholdUSD)
	if cfg.IsCustomThreshold() {
		th = cfg.ThresholdFixedUSD
		mode = models.ThresholdModeCustom
		skip = false
	}

	// Static evaluation (FR-02/FR-03).
	if !skip {
		if err := r.evalStatic(acct, cfg, th, mode, settings, now, snd, stats); err != nil {
			return err
		}
	} else {
		stats.SkippedIncome++
	}

	// Dynamic evaluation (PRD 10.2): independent of static, own state tracking.
	if cfg.DynamicEnabled && consumption != nil {
		r.evalDynamic(acct, cfg, consumption, settings, now, snd, stats)
	}

	return nil
}

func (r *Runner) evalStatic(acct Account, cfg *models.BalanceAlertConfig, th float64, mode string, settings models.BalanceAlertSettings, now time.Time, snd Sender, stats *Stats) error {
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
		TriggerType:      models.TriggerTypeStatic,
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

	sendRes := snd.Send(SendRequest{
		Mode:          settings.SendMode,
		Level:         rec.Level,
		ThresholdMode: mode,
		TriggerType:   models.TriggerTypeStatic,
		AccountID:     acct.ID,
		Name:          acct.Name,
		Balance:       acct.Balance,
		Threshold:     th,
		Phone:         acct.Phone,
		NotifyRuleID:  notifyRuleIDFor(settings),
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

func (r *Runner) evalDynamic(acct Account, cfg *models.BalanceAlertConfig, consumption *Consumption, settings models.BalanceAlertSettings, now time.Time, snd Sender, stats *Stats) {
	dr := EvaluateDynamic(acct.Balance, consumption)
	if !dr.ShouldAlert {
		return
	}

	dynAt, err := models.BalanceAlertRecordLastSentAt(r.Ctx, acct.ID, StateWarn)
	if err != nil {
		logger.Errorf("balancealert dynamic: %s: %v", acct.ID, err)
		return
	}
	if !canSendToday(dynAt, now) {
		stats.Cooldown++
		return
	}

	stats.Evaluated++

	rec := &models.BalanceAlertRecord{
		BillingAccountID: acct.ID,
		Level:            StateWarn,
		BalanceUSD:       acct.Balance,
		ThresholdUSD:     0,
		ThresholdMode:    "DYNAMIC",
		SendMode:         settings.SendMode,
		Status:           StatusSent,
		TriggerType:      models.TriggerTypeDynamic,
	}

	ruleID := settings.DynamicNotifyRuleID
	if ruleID <= 0 {
		ruleID = notifyRuleIDFor(settings)
	}

	sendRes := snd.Send(SendRequest{
		Mode:          settings.SendMode,
		Level:         StateWarn,
		ThresholdMode: "DYNAMIC",
		TriggerType:   models.TriggerTypeDynamic,
		AccountID:     acct.ID,
		Name:          acct.Name,
		Balance:       acct.Balance,
		Threshold:     0,
		DynamicDays:   dr.Days,
		Phone:         acct.Phone,
		NotifyRuleID:  ruleID,
		PilotURLs:     settings.PilotReceivers,
		SmsURL:        settings.SmsWebhook,
	})
	rec.Channel = sendRes.Channel
	rec.Receiver = sendRes.Receiver
	if sendRes.Err != nil && settings.SendMode != SendModeOff && settings.SendMode != "" {
		rec.Status = StatusFailed
		stats.Failed++
		if err := models.BalanceAlertRecordInsert(r.Ctx, rec); err != nil {
			logger.Errorf("balancealert dynamic: %s: %v", acct.ID, err)
		}
		return
	}

	sentAt := now
	if settings.SendMode != SendModeOff && settings.SendMode != "" {
		rec.SentAt = &sentAt
	}
	stats.Sent++
	if err := models.BalanceAlertRecordInsert(r.Ctx, rec); err != nil {
		logger.Errorf("balancealert dynamic: %s: %v", acct.ID, err)
	}
}

func notifyRuleIDFor(s models.BalanceAlertSettings) int64 {
	switch s.SendMode {
	case SendModePilot:
		return s.PilotNotifyRuleID
	case SendModeCustomer:
		return s.CustomerNotifyRuleID
	default:
		return 0
	}
}
