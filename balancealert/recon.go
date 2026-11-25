package balancealert

import (
	"context"
	"fmt"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

// MissReport holds a prepaid enterprise that is at/below zero (or already
// critical) without a WARN/CRITICAL record in the lookback window — PRD §8.2.
type MissReport struct {
	BillingAccountID string     `json:"billing_account_id"`
	Name             string     `json:"name"`
	BalanceUSD       float64    `json:"balance_usd"`
	CurrentState     string     `json:"current_state"`
	LastAlertAt      *time.Time `json:"last_alert_at,omitempty"`
	Reason           string     `json:"reason"`
}

type MissReportResult struct {
	Disabled       bool            `json:"disabled,omitempty"`
	AsOf           time.Time       `json:"as_of"`
	LookbackDays   int             `json:"lookback_days"`
	PrepaidScanned int             `json:"prepaid_scanned"`
	Misses         []MissReport    `json:"misses"`
	ExcludeReviews []ExcludeReview `json:"exclude_reviews"`
}

// ReconMissReports implements PRD §8.2 漏报对账: scan prepaid enterprises
// with balance <= 0 and check for any WARN/CRITICAL record in the last N days.
func ReconMissReports(n9e *ctx.Context, lookbackDays int) (MissReportResult, error) {
	out := MissReportResult{
		AsOf:           time.Now().In(locShanghai),
		LookbackDays:   lookbackDays,
		Misses:         []MissReport{},
		ExcludeReviews: []ExcludeReview{},
	}
	if lookbackDays <= 0 {
		lookbackDays = 7
		out.LookbackDays = 7
	}

	settings, err := models.BalanceAlertSettingsGet(n9e)
	if err != nil {
		return out, err
	}
	if !settings.AlertEnabled() {
		out.Disabled = true
		return out, nil
	}
	store, err := OpenStore(n9e, settings)
	if err != nil {
		return out, err
	}
	return reconMissReports(n9e, store, settings, out)
}

func reconMissReports(n9e *ctx.Context, store Store, settings models.BalanceAlertSettings, out MissReportResult) (MissReportResult, error) {
	accts, err := store.ListPrepaid(context.Background())
	if err != nil {
		return out, fmt.Errorf("list prepaid: %w", err)
	}
	// 与判定轮次同一口径：漏报对账也必须剔除授信活跃企业，否则会把
	// 正常排除的授信企业报成「漏报」。
	state, err := models.BalanceAlertExcludeStateGet(n9e)
	if err != nil {
		return out, fmt.Errorf("read credit exclude refresh state: %w", err)
	}
	if !state.Ready() {
		return out, fmt.Errorf("credit exclude list never refreshed; fail closed")
	}
	excludedIDs, err := models.BalanceAlertCreditExcludeIDs(n9e)
	if err != nil {
		return out, err
	}
	lowExcluded, filtered := make([]Account, 0), make([]Account, 0, len(accts))
	for _, a := range accts {
		if _, excluded := excludedIDs[a.ID]; excluded {
			if a.Balance <= 0 {
				lowExcluded = append(lowExcluded, a)
			}
		} else {
			filtered = append(filtered, a)
		}
	}
	out.ExcludeReviews, err = store.ReviewLowBalanceExcludes(context.Background(), lowExcluded, settings.WindowDays())
	if err != nil {
		return out, fmt.Errorf("review excluded accounts: %w", err)
	}
	accts = filtered
	out.PrepaidScanned = len(accts)

	since := out.AsOf.AddDate(0, 0, -out.LookbackDays)
	for _, a := range accts {
		if a.Balance > 0 {
			continue
		}
		cfg, err := models.BalanceAlertConfigGet(n9e, a.ID)
		if err != nil {
			return out, fmt.Errorf("read alert config for %s: %w", a.ID, err)
		}
		_, _, skip := ComputeThreshold(a.LastRecharge, a.HasVoucher, settings.VoucherThresholdUSD, settings.LastRechargePct)
		if skip && (cfg == nil || !cfg.IsCustomThreshold()) {
			continue
		}
		ok, lastAt, err := models.BalanceAlertRecordHasLevelSince(n9e, a.ID, since)
		if err != nil {
			return out, err
		}
		if ok {
			continue
		}
		state := StateNormal
		var cfgLast *time.Time
		if cfg != nil {
			state = cfg.CurrentState
			cfgLast = cfg.LastAlertAt
		}
		out.Misses = append(out.Misses, MissReport{
			BillingAccountID: a.ID,
			Name:             a.Name,
			BalanceUSD:       a.Balance,
			CurrentState:     state,
			LastAlertAt:      firstTime(lastAt, cfgLast),
			Reason:           "余额不大于零，回看期间没有成功发送的余额预警",
		})
	}
	return out, nil
}

func firstTime(a, b *time.Time) *time.Time {
	if a != nil {
		return a
	}
	return b
}
