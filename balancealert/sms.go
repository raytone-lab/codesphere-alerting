package balancealert

import (
	"fmt"
	"strings"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

// SendPlainSMS delivers a one-off SMS (OTP) via the customer notify rule or
// legacy SMS webhook. It does not write balance_alert_records.
func SendPlainSMS(n9e *ctx.Context, phone, content string) error {
	phone = strings.TrimSpace(phone)
	content = strings.TrimSpace(content)
	if phone == "" {
		return fmt.Errorf("empty phone")
	}
	if content == "" {
		return fmt.Errorf("empty sms content")
	}
	settings, err := models.BalanceAlertSettingsGet(n9e)
	if err != nil {
		return err
	}
	req := SendRequest{
		Mode:         SendModeCustomer,
		Level:        StateWarn,
		Name:         "余额告警",
		Phone:        phone,
		Phones:       []string{phone},
		CopyOverride: content,
		NotifyRuleID: settings.CustomerNotifyRuleID,
		SmsURL:       settings.SmsWebhook,
	}
	var snd Sender
	switch {
	case strings.TrimSpace(settings.SmsWebhook) != "":
		snd = HTTPSender{HTTP: DefaultHTTP()}
	case settings.SendMode == SendModeCustomer && settings.CustomerNotifyRuleID > 0 && defaultSnd != nil:
		snd = defaultSnd
	default:
		return fmt.Errorf("短信通道未配置，无法发送验证码")
	}
	res := snd.Send(req)
	return res.Err
}
