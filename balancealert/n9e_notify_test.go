package balancealert

import (
	"testing"

	"github.com/ccfos/nightingale/v6/alert/dispatch"
	"github.com/ccfos/nightingale/v6/alert/sender/provider"
	"github.com/ccfos/nightingale/v6/models"
)

func TestNotifyRuleIDFor(t *testing.T) {
	s := models.BalanceAlertSettings{
		SendMode:             SendModeOff,
		PilotNotifyRuleID:    11,
		CustomerNotifyRuleID: 22,
	}
	if got := notifyRuleIDFor(s); got != 0 {
		t.Fatalf("OFF: %d", got)
	}
	s.SendMode = SendModePilot
	if got := notifyRuleIDFor(s); got != 11 {
		t.Fatalf("PILOT: %d", got)
	}
	s.SendMode = SendModeCustomer
	if got := notifyRuleIDFor(s); got != 22 {
		t.Fatalf("CUSTOMER: %d", got)
	}
}

func TestAliyunSmsTemplateParam(t *testing.T) {
	got := aliyunSmsTemplateParam(SendRequest{Name: "中国冶金科工股份有限公司", Balance: -0.21, AccountID: "ba1"})
	want := `{"name":"中国冶金科工股份有限公司","value":"¥-0.21"}`
	if got != want {
		t.Fatalf("TemplateParam=%s want %s", got, want)
	}
	pilot := fallbackTplContent(SendRequest{AccountID: "ba1", Balance: 12.5, Level: StateWarn, Mode: SendModePilot})
	if _, ok := pilot["name"]; ok {
		t.Fatalf("PILOT fallback must not inject Aliyun vars: %#v", pilot)
	}
	sms := fallbackTplContent(SendRequest{AccountID: "ba1", Balance: 12.5, Level: StateWarn, Mode: SendModeCustomer})
	if sms["name"] != "ba1" || sms["value"] != "¥12.50" {
		t.Fatalf("CUSTOMER fallback vars: %#v", sms)
	}
}

func TestApplyCustomerAliyunTemplateParamSkipsDingtalk(t *testing.T) {
	ch := &models.NotifyChannelConfig{
		Ident: "dingtalk",
		RequestConfig: &models.RequestConfig{
			HTTPRequestConfig: &models.HTTPRequestConfig{
				Request: models.RequestDetail{Parameters: map[string]string{"access_token": "keep"}},
			},
		},
	}
	ncCtx := &dispatch.NotifyContext{
		Request: &provider.NotifyRequest{Config: ch},
	}
	applyCustomerAliyunTemplateParam(ncCtx, SendRequest{Name: "数商云", Balance: 15.5})
	if ncCtx.Request.Config != ch {
		t.Fatal("dingtalk channel must not be cloned")
	}
	if ch.RequestConfig.HTTPRequestConfig.Request.Parameters["access_token"] != "keep" {
		t.Fatal("dingtalk params mutated")
	}
}

func TestApplyCustomerAliyunTemplateParamClonesCache(t *testing.T) {
	origParams := map[string]string{
		"TemplateParam": `{"incident":"{{ $tpl.incident }}"}`,
		"SignName":      "测试短信",
	}
	ch := &models.NotifyChannelConfig{
		Ident: provider.AliyunSmsIdent,
		RequestConfig: &models.RequestConfig{
			HTTPRequestConfig: &models.HTTPRequestConfig{
				Request: models.RequestDetail{Parameters: origParams},
			},
		},
	}
	ncCtx := &dispatch.NotifyContext{
		Request: &provider.NotifyRequest{Config: ch},
	}
	applyCustomerAliyunTemplateParam(ncCtx, SendRequest{Name: "数商云", Balance: 15.5})

	if origParams["TemplateParam"] != `{"incident":"{{ $tpl.incident }}"}` {
		t.Fatalf("cache mutated: %s", origParams["TemplateParam"])
	}
	got := ncCtx.Request.Config.RequestConfig.HTTPRequestConfig.Request.Parameters["TemplateParam"]
	if got != `{"name":"数商云","value":"¥15.50"}` {
		t.Fatalf("cloned TemplateParam=%s", got)
	}
	if ncCtx.Request.Config == ch {
		t.Fatal("expected cloned channel, not the cache pointer")
	}
}

func TestBuildSyntheticEvent(t *testing.T) {
	ev := buildSyntheticEvent(SendRequest{
		Mode: SendModePilot, Level: StateCritical, ThresholdMode: ModeVoucherFixed,
		AccountID: "ba1", Name: "数商云", Balance: -0.04, Threshold: 20, Phone: "138",
	})
	if ev.Severity != 1 || ev.AnnotationsJSON["level"] != StateCritical {
		t.Fatalf("event: sev=%d ann=%v", ev.Severity, ev.AnnotationsJSON)
	}
	if ev.TagsMap["product"] != "balance_alert" {
		t.Fatalf("tags: %v", ev.TagsMap)
	}
	if ev.AnnotationsJSON["pilot_line"] == "" || ev.AnnotationsJSON["copy"] == "" {
		t.Fatalf("missing rendered copy fields: %v", ev.AnnotationsJSON)
	}
}
