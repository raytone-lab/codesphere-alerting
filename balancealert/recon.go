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
	AsOf           time.Time    `json:"as_of"`
	LookbackDays   int          `json:"lookback_days"`
	PrepaidScanned int          `json:"prepaid_scanned"`
	Misses         []MissReport `json:"misses"`
}

// ReconMissReports implements PRD §8.2 漏报对账: scan prepaid enterprises
// with balance <= 0 and check for any WARN/CRITICAL record in the last N days.
func ReconMissReports(n9e *ctx.Context, lookbackDays int) (MissReportResult, error) {
	out := MissReportResult{
		AsOf:         time.Now().In(locShanghai),
		LookbackDays: lookbackDays,
		Misses:       []MissReport{},
	}
	if lookbackDays <= 0 {
		lookbackDays = 7
		out.LookbackDays = 7
	}

	settings, err := models.BalanceAlertSettingsGet(n9e)
	if err != nil {
		return out, err
	}
	store, err := OpenStore(n9e, settings)
	if err != nil {
		return out, err
	}
	accts, err := store.ListPrepaid(context.Background())
	if err != nil {
		return out, fmt.Errorf("list prepaid: %w", err)
	}
	out.PrepaidScanned = len(accts)

	since := out.AsOf.AddDate(0, 0, -lookbackDays)
	for _, a := range accts {
		if a.Balance > 0 {
			continue
		}
		ok, lastAt, err := models.BalanceAlertRecordHasLevelSince(n9e, a.ID, since)
		if err != nil {
			return out, err
		}
		if ok {
			continue
		}
		cfg, _ := models.BalanceAlertConfigGet(n9e, a.ID)
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
			Reason:           "balance<=0 without WARN/CRITICAL record in lookback",
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
