package balancealert

import (
	"context"
	"fmt"
	"strings"
	"time"

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
		// 有独立、明确的授信活动足以证明排除有依据；历史分类异常
		// 不应每天作为余额预警异常重复通知。原始名单和命中明细仍保留。
		if !advance && !settlement {
			if uncertain {
				reason = "未找到明确的授信活动，历史核销记录分类不完整，请核实是否应排除余额预警"
			} else {
				reason = "未找到明确的授信活动，排除名单可能已过期，请核实是否应排除余额预警"
			}
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
	return sendReconOpsNotice(settings, formatReconFindings(misses, reviews))
}

// Keep the notification readable even for large installations; the page has
// the complete list. Identifiers disambiguate enterprises with the same name.
func formatReconFindings(misses []MissReport, reviews []ExcludeReview) string {
	const maxShown = 5
	var text strings.Builder
	fmt.Fprintf(&text, "疑似漏发余额预警：%d 家\n", len(misses))
	for i, m := range misses {
		if i == maxShown {
			fmt.Fprintf(&text, "其余 %d 家请在对账页面查看。\n", len(misses)-i)
			break
		}
		fmt.Fprintf(&text, "- %s（账户 %s）：余额 %.2f 元，近 7 天没有成功发送的余额预警。\n", noticeName(m.Name, m.BillingAccountID), noticeField(m.BillingAccountID), m.BalanceUSD)
	}
	if len(misses) > 0 {
		text.WriteString("请检查发送记录和通知渠道，确认是否需要补发。\n")
	}
	fmt.Fprintf(&text, "\n需要核实是否误排除：%d 家\n", len(reviews))
	for i, r := range reviews {
		if i == maxShown {
			fmt.Fprintf(&text, "其余 %d 家请在对账页面查看。\n", len(reviews)-i)
			break
		}
		fmt.Fprintf(&text, "- %s（账户 %s）：%s。\n", noticeName(r.Name, r.BillingAccountID), noticeField(r.BillingAccountID), noticeField(r.Reason))
	}
	if len(reviews) > 0 {
		text.WriteString("请核对企业授信情况和排除名单；确认前不要修改账目。\n")
	}
	text.WriteString("完整明细请查看余额预警页面的“漏报对账”。")
	return text.String()
}

func noticeField(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func noticeName(name, id string) string {
	if name = noticeField(name); name != "" {
		return name
	}
	if id = noticeField(id); id != "" {
		return id
	}
	return "未知企业"
}

func sendReconOpsNotice(settings models.BalanceAlertSettings, text string) error {
	text = fmt.Sprintf("【余额预警每日检查】\n%s\n检查时间：%s（北京时间）", text, time.Now().In(locShanghai).Format("2006-01-02 15:04:05"))
	req := SendRequest{Mode: SendModePilot, Level: StateWarn, Name: "排除名单复核", AccountID: "exclude-review", CopyOverride: text, OpsNotice: true, NotifyRuleID: settings.PilotNotifyRuleID, PilotURLs: settings.PilotReceivers}
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
