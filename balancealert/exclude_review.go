package balancealert

import (
	"context"
	"fmt"

	"github.com/ccfos/nightingale/v6/models"
)

// reviewExcludeSQL independently checks only low-balance accounts already in
// the materialized list. It does not use the refresh query or its result.
const reviewExcludeSQL = `
SELECT a.id,
 EXISTS (SELECT 1 FROM balance_consumption_allocations x
   WHERE x.billing_account_id = a.id AND x.approved_credit_usd > 0
     AND (x.approved_credit_usd - COALESCE(x.settled_usd, 0) > 0.0001
       OR x.created_at > now() - make_interval(days => ?::int))) AS has_advance,
 EXISTS (SELECT 1 FROM enterprise_credit_settlements s
   JOIN balance_consumption_allocations x ON x.id = s.allocation_id
   WHERE s.billing_account_id = a.id AND s.debt_source = 'ENTERPRISE_CREDIT'
     AND s.occurred_at > now() - make_interval(days => ?::int)
     AND x.billing_account_id = s.billing_account_id AND x.source = s.debt_source
     AND x.funding_classification_complete AND x.approved_credit_usd > 0 AND x.unapproved_debt_usd = 0) AS has_credit_settlement,
 EXISTS (SELECT 1 FROM enterprise_credit_settlements s
   LEFT JOIN balance_consumption_allocations x ON x.id = s.allocation_id
   WHERE s.billing_account_id = a.id AND s.debt_source = 'ENTERPRISE_CREDIT'
     AND s.occurred_at > now() - make_interval(days => ?::int)
     AND NOT COALESCE(x.id IS NOT NULL AND x.billing_account_id = s.billing_account_id
       AND x.source = s.debt_source AND x.funding_classification_complete
       AND ((x.approved_credit_usd > 0 AND x.unapproved_debt_usd = 0)
         OR (x.approved_credit_usd = 0 AND x.unapproved_debt_usd > 0)), FALSE)) AS uncertain
FROM billing_accounts a WHERE a.id IN ?`

type ExcludeReview struct {
	BillingAccountID string  `json:"billing_account_id"`
	Name             string  `json:"name"`
	BalanceUSD       float64 `json:"balance_usd"`
	Reason           string  `json:"reason"`
}

func (s *PGStore) ReviewLowBalanceExcludes(parent context.Context, accounts []Account, window int) ([]ExcludeReview, error) {
	if len(accounts) == 0 {
		return []ExcludeReview{}, nil
	}
	ids := make([]string, 0, len(accounts))
	byID := make(map[string]Account, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
		byID[a.ID] = a
	}
	ctx, cancel := billingCtx(parent)
	defer cancel()
	rows, err := s.db.WithContext(ctx).Raw(reviewExcludeSQL, window, window, window, ids).Rows()
	if err != nil {
		return nil, wrapBillingSelectErr(err)
	}
	defer rows.Close()
	reviews := []ExcludeReview{}
	seen := make(map[string]bool, len(accounts))
	for rows.Next() {
		var id string
		var advance, settlement, uncertain bool
		if err := rows.Scan(&id, &advance, &settlement, &uncertain); err != nil {
			return nil, err
		}
		seen[id] = true
		reason := ""
		if uncertain {
			reason = "核销资金分类不完整或关联异常，需人工核实"
		} else if !advance && !settlement {
			reason = "当前没有可证明的授信活动，排除名单可能过期或误判"
		}
		if reason != "" {
			a := byID[id]
			reviews = append(reviews, ExcludeReview{a.ID, a.Name, a.Balance, reason})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if !seen[a.ID] {
			return nil, fmt.Errorf("excluded account %s missing during review", a.ID)
		}
	}
	return reviews, nil
}

func notifyReconFindings(settings models.BalanceAlertSettings, misses []MissReport, reviews []ExcludeReview) error {
	if len(reviews) == 0 && len(misses) == 0 {
		return nil
	}
	text := fmt.Sprintf("余额预警漏报对账：%d 家余额不大于零且近期无成功通知，%d 家排除名单需复核。请在余额预警漏报对账页面查看。", len(misses), len(reviews))
	return sendReconOpsNotice(settings, text)
}

func sendReconOpsNotice(settings models.BalanceAlertSettings, text string) error {
	req := SendRequest{Mode: SendModePilot, Level: StateWarn, Name: "排除名单复核", AccountID: "exclude-review", CopyOverride: text, NotifyRuleID: settings.PilotNotifyRuleID, PilotURLs: settings.PilotReceivers}
	if settings.PilotNotifyRuleID > 0 && defaultSnd != nil {
		result := defaultSnd.Send(req)
		if result.Err != nil {
			return result.Err
		}
		return nil
	}
	if sendExcludeOpsNotice(settings, text) {
		return nil
	}
	return fmt.Errorf("no usable operations notification channel or delivery failed")
}
