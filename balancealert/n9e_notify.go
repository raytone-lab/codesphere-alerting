package balancealert

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/alert/dispatch"
	"github.com/ccfos/nightingale/v6/alert/sender"
	"github.com/ccfos/nightingale/v6/alert/sender/provider"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

// N9eNotifySender routes PILOT/CUSTOMER through Nightingale notify rules
// (channel + message template). Decision stays in balancealert; only egress
// reuses the Nightingale stack. Sends synchronously so the runner can mark
// SENT vs FAILED before writing balance_alert_records.
type N9eNotifySender struct {
	Ctx                  *ctx.Context
	NotifyRuleCache      *memsto.NotifyRuleCacheType
	NotifyChannelCache   *memsto.NotifyChannelCacheType
	MessageTemplateCache *memsto.MessageTemplateCacheType
	UserCache            *memsto.UserCacheType
	UserGroupCache       *memsto.UserGroupCacheType
	ConfigCvalCache      *memsto.CvalCache
	Fallback             Sender
}

func (s *N9eNotifySender) Send(req SendRequest) SendResult {
	ruleID := req.NotifyRuleID
	if ruleID <= 0 {
		if s.Fallback != nil {
			return s.Fallback.Send(req)
		}
		return SendResult{Err: fmt.Errorf("notify rule id not configured for send_mode %s", req.Mode)}
	}

	rule := s.NotifyRuleCache.Get(ruleID)
	if rule == nil || !rule.Enable {
		return SendResult{Err: fmt.Errorf("notify rule %d not found or disabled", ruleID)}
	}
	if len(rule.NotifyConfigs) == 0 {
		return SendResult{Err: fmt.Errorf("notify rule %d has no notify configs", ruleID)}
	}

	event := buildSyntheticEvent(req)
	events := []*models.AlertCurEvent{event}

	siteURL := ""
	if s.ConfigCvalCache != nil {
		if info := s.ConfigCvalCache.GetSiteInfo(); info != nil {
			siteURL = info.SiteUrl
		}
	}

	var (
		lastErr   error
		channels  []string
		receivers []string
		sent      bool
	)

	for i := range rule.NotifyConfigs {
		nc := rule.NotifyConfigs[i]
		if err := matchNotifyConfig(&nc, event); err != nil {
			continue
		}

		ch := s.NotifyChannelCache.Get(nc.ChannelID)
		if ch == nil || !ch.Enable {
			lastErr = fmt.Errorf("notify channel %d missing or disabled", nc.ChannelID)
			continue
		}

		var tpl *models.MessageTemplate
		if nc.TemplateID > 0 {
			tpl = s.MessageTemplateCache.Get(nc.TemplateID)
		}

		tplContent := map[string]interface{}{}
		if tpl != nil {
			tplContent = tpl.RenderEvent(events, siteURL)
		} else {
			tplContent = fallbackTplContent(req)
		}

		ncCtx, err := dispatch.BuildNotifyContext(s.Ctx, s.UserCache, s.UserGroupCache,
			events, ruleID, &nc, ch, tplContent,
			s.NotifyChannelCache.GetHttpClient(ch.ID), siteURL)
		if err != nil {
			lastErr = err
			continue
		}

		// CUSTOMER SMS: inject billing owner phone as sendto when the channel
		// expects user contact phones and the rule did not resolve any.
		if req.Mode == SendModeCustomer && strings.TrimSpace(req.Phone) != "" {
			if len(ncCtx.Request.Sendtos) == 0 {
				ncCtx.Request.Sendtos = []string{strings.TrimSpace(req.Phone)}
			}
		}

		result := notifySync(s.Ctx, ncCtx)
		channelName := ch.Name
		if channelName == "" {
			channelName = ch.Ident
		}
		channels = append(channels, channelName)
		target := ""
		if result != nil {
			target = result.Target
			if result.Err != nil {
				lastErr = result.Err
				sender.NotifyRecord(s.Ctx, events, ruleID, channelName, target, result.Response, result.Err)
				continue
			}
			sender.NotifyRecord(s.Ctx, events, ruleID, channelName, target, result.Response, nil)
			sent = true
		}
		if target != "" {
			receivers = append(receivers, target)
		} else if req.Mode == SendModeCustomer && req.Phone != "" {
			receivers = append(receivers, req.Phone)
		}
	}

	out := SendResult{
		Channel:  strings.Join(uniqueNonEmpty(channels), ","),
		Receiver: strings.Join(uniqueNonEmpty(receivers), ","),
	}
	if !sent {
		if lastErr != nil {
			out.Err = lastErr
		} else {
			out.Err = fmt.Errorf("notify rule %d matched no channel config", ruleID)
		}
		if out.Channel == "" {
			if req.Mode == SendModePilot {
				out.Channel = "DINGTALK"
			} else {
				out.Channel = "SMS"
			}
		}
	}
	return out
}

func matchNotifyConfig(nc *models.NotifyConfig, event *models.AlertCurEvent) error {
	cfg := *nc
	if len(cfg.Severities) == 0 {
		cfg.Severities = []int{event.Severity}
	}
	return dispatch.NotifyRuleMatchCheck(&cfg, event)
}

func notifySync(nctx *ctx.Context, nc *dispatch.NotifyContext) *provider.NotifyResult {
	if nc == nil || nc.Provider == nil || nc.Request == nil {
		return &provider.NotifyResult{Err: fmt.Errorf("empty notify context")}
	}
	req := nc.Request
	switch {
	case req.Config != nil && req.Config.RequestType == "smtp":
		// SMTP needs a channel; balance alert phase-1 uses HTTP DingTalk/SMS.
		if req.SmtpChan == nil {
			return &provider.NotifyResult{Err: fmt.Errorf("smtp channel not configured for balance alert")}
		}
		return nc.Provider.Notify(context.Background(), req)
	default:
		if req.HttpClient == nil {
			req.HttpClient = &http.Client{Timeout: 10 * time.Second}
		}
		return nc.Provider.Notify(context.Background(), req)
	}
}

func buildSyntheticEvent(req SendRequest) *models.AlertCurEvent {
	sev := 2
	if req.Level == StateCritical {
		sev = 1
	}
	now := time.Now().Unix()
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.AccountID
	}
	ann := map[string]string{
		"enterprise_name": name,
		"balance":         fmt.Sprintf("%.2f", req.Balance),
		"threshold":       fmt.Sprintf("%.2f", req.Threshold),
		"level":           req.Level,
		"threshold_mode":  req.ThresholdMode,
		"trigger_type":    req.TriggerType,
		"send_mode":       req.Mode,
		"phone":           req.Phone,
		"billing_account": req.AccountID,
		"copy":            RenderCopy(req),
		"pilot_line":      RenderPilotLine(name, req.Balance, req.Threshold, req.Level),
	}
	if req.TriggerType == models.TriggerTypeDynamic {
		ann["remaining_days"] = FormatRemainingDays(req.DynamicDays)
	}
	tags := []string{
		"product=balance_alert",
		"level=" + req.Level,
		"threshold_mode=" + req.ThresholdMode,
		"trigger_type=" + req.TriggerType,
		"billing_account_id=" + req.AccountID,
	}
	tagMap := map[string]string{
		"product":            "balance_alert",
		"level":              req.Level,
		"threshold_mode":     req.ThresholdMode,
		"trigger_type":       req.TriggerType,
		"billing_account_id": req.AccountID,
	}
	return &models.AlertCurEvent{
		Cate:            "balance_alert",
		RuleName:        "预付费余额预警",
		RuleNote:        "codesphere prepaid balance alert",
		Severity:        sev,
		TriggerTime:     now,
		TriggerValue:    fmt.Sprintf("%.2f", req.Balance),
		TargetIdent:     req.AccountID,
		TargetNote:      name,
		AnnotationsJSON: ann,
		TagsJSON:        tags,
		TagsMap:         tagMap,
		Hash:            "balance-alert:" + req.AccountID + ":" + req.Level,
	}
}

func fallbackTplContent(req SendRequest) map[string]interface{} {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.AccountID
	}
	var body string
	if req.Mode == SendModePilot {
		body = RenderPilotLine(name, req.Balance, req.Threshold, req.Level)
	} else {
		body = RenderCopy(req)
	}
	return map[string]interface{}{
		"content": body,
		"text":    body,
		"title":   "预付费余额预警",
	}
}

func uniqueNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
