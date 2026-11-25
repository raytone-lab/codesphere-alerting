package balancealert

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/alert/dispatch"
	"github.com/ccfos/nightingale/v6/alert/sender"
	"github.com/ccfos/nightingale/v6/alert/sender/provider"
	"github.com/ccfos/nightingale/v6/memsto"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/toolkits/pkg/logger"
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
	if req.OpsNotice && req.Mode != SendModePilot {
		return SendResult{Err: fmt.Errorf("operations notices require PILOT mode")}
	}
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
		if req.OpsNotice {
			tplContent = s.opsNoticeTplContent(event, ch.Ident, siteURL)
		} else if tpl != nil {
			tplContent = tpl.RenderEvent(events, siteURL)
		} else {
			tplContent = fallbackTplContent(req)
		}
		if ch.Ident == models.Dingtalk {
			tplContent = labelDingTalkContent(tplContent)
		}
		if req.Mode == SendModeCustomer {
			tplContent = withAliyunSmsVars(req, tplContent)
		}

		ncCtx, err := dispatch.BuildNotifyContext(s.Ctx, s.UserCache, s.UserGroupCache,
			events, ruleID, &nc, ch, tplContent,
			s.NotifyChannelCache.GetHttpClient(ch.ID), siteURL)
		if err != nil {
			lastErr = err
			continue
		}

		// CUSTOMER SMS only: do not touch DingTalk / PILOT channel config.
		if req.Mode == SendModeCustomer {
			phones := requestPhones(req)
			if len(phones) > 0 {
				ncCtx.Request.Sendtos = phones
			}
			applyCustomerAliyunTemplateParam(ncCtx, req)
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

// Operations summaries must not be rendered by an account alert template:
// existing templates can omit the summary and display meaningless zero values.
// Render through MessageTemplate to retain channel-specific escaping.
func (s *N9eNotifySender) opsNoticeTplContent(event *models.AlertCurEvent, channelIdent, siteURL string) map[string]interface{} {
	var tpl *models.MessageTemplate
	if s.Ctx != nil && s.Ctx.IsCenter {
		var err error
		tpl, err = models.MessageTemplateGet(s.Ctx, "ident = ?", reconNoticeTemplateIdent)
		if err != nil {
			logger.Warningf("balancealert: cannot read reconciliation template, using default: %v", err)
		}
	}
	if tpl != nil && tpl.NotifyChannelIdent == channelIdent {
		content, err := tpl.RenderEventStrict([]*models.AlertCurEvent{event}, siteURL)
		if err == nil && content["content"] != nil && strings.TrimSpace(fmt.Sprint(content["content"])) != "" && content["text"] != nil && strings.TrimSpace(fmt.Sprint(content["text"])) != "" {
			return content
		}
		logger.Warningf("balancealert: invalid reconciliation template, using default: %v", err)
	}
	fallback := &models.MessageTemplate{NotifyChannelIdent: channelIdent, Content: reconNoticeTemplateContent}
	return fallback.RenderEvent([]*models.AlertCurEvent{event}, siteURL)
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

// Environment is configured separately for each deployment. Never infer it from
// database contents, which may have been copied from another environment.
func alertEnvironment() string {
	env := strings.ToUpper(strings.TrimSpace(os.Getenv("BALANCE_ALERT_ENV")))
	if env == "" {
		return "未配置环境"
	}
	for _, c := range env {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return "未配置环境"
		}
	}
	return env
}

func labelAlertText(text string) string {
	prefix := "【" + alertEnvironment() + "】"
	if strings.HasPrefix(text, prefix) {
		return text
	}
	return prefix + text
}

// Mark both the DingTalk card title and the visible body, even when a shared
// or operator-edited database template does not contain an environment field.
func labelDingTalkContent(content map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(content))
	for key, value := range content {
		out[key] = value
	}
	for _, key := range []string{"title", "content", "text"} {
		switch text := out[key].(type) {
		case string:
			if strings.TrimSpace(text) != "" {
				out[key] = labelAlertText(text)
			}
		case template.HTML:
			if strings.TrimSpace(string(text)) != "" {
				out[key] = template.HTML(labelAlertText(string(text)))
			}
		}
	}
	return out
}

func severityEmoji(level string) string {
	if level == StateCritical {
		return "🔴"
	}
	return "🟠"
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
		"severity_emoji":  severityEmoji(req.Level),
	}
	if strings.TrimSpace(req.CopyOverride) != "" {
		ann["pilot_line"] = RenderCopy(req)
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
	body := fmt.Sprintf("%s 告警级别：%s级\n告警时间：%s\n企业名称：%s\n事由签名：%s\n当次触发时值：%.2f",
		severityEmoji(req.Level), req.Level,
		time.Now().In(locShanghai).Format("2006-01-02 15:04:05")+"（北京时间）",
		name,
		RenderCopy(req),
		req.Balance,
	)
	tpl := map[string]interface{}{
		"content": body,
		"text":    body,
		"title":   "预付费余额预警",
	}
	if req.Mode == SendModeCustomer {
		return withAliyunSmsVars(req, tpl)
	}
	return tpl
}

// aliyunSmsVars matches Aliyun template SMS_512500774:
// 尊敬的 ${name}，您的余额为${value}，请及时充值，以免耽误您的正常使用。
func aliyunSmsVars(req SendRequest) (name, value string) {
	name = strings.TrimSpace(req.Name)
	if name == "" {
		name = req.AccountID
	}
	return name, fmt.Sprintf("¥%.2f", req.Balance)
}

func aliyunSmsTemplateParam(req SendRequest) string {
	name, value := aliyunSmsVars(req)
	b, err := json.Marshal(map[string]string{"name": name, "value": value})
	if err != nil {
		return fmt.Sprintf(`{"name":%q,"value":%q}`, name, value)
	}
	return string(b)
}

func withAliyunSmsVars(req SendRequest, tpl map[string]interface{}) map[string]interface{} {
	if tpl == nil {
		tpl = map[string]interface{}{}
	}
	name, value := aliyunSmsVars(req)
	tpl["name"] = name
	tpl["value"] = value
	return tpl
}

// applyCustomerAliyunTemplateParam clones the cached channel and writes TemplateParam
// as already-rendered JSON so Aliyun receives ${name}/${value}, not Nightingale's
// default ${incident}. Cloning avoids mutating the notify-channel cache.
func applyCustomerAliyunTemplateParam(ncCtx *dispatch.NotifyContext, req SendRequest) {
	if ncCtx == nil || ncCtx.Request == nil || ncCtx.Request.Config == nil {
		return
	}
	ch := ncCtx.Request.Config
	if ch.Ident != provider.AliyunSmsIdent {
		return
	}
	if ch.RequestConfig == nil || ch.RequestConfig.HTTPRequestConfig == nil {
		return
	}

	origHTTP := ch.RequestConfig.HTTPRequestConfig
	params := make(map[string]string, len(origHTTP.Request.Parameters)+1)
	for k, v := range origHTTP.Request.Parameters {
		params[k] = v
	}
	params["TemplateParam"] = aliyunSmsTemplateParam(req)

	httpCopy := *origHTTP
	httpCopy.Request.Parameters = params
	reqCfgCopy := *ch.RequestConfig
	reqCfgCopy.HTTPRequestConfig = &httpCopy
	chCopy := *ch
	chCopy.RequestConfig = &reqCfgCopy
	ncCtx.Request.Config = &chCopy
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
