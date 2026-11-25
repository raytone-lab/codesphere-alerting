package balancealert

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestOpsNoticeDingtalkHTTPContainsSummary(t *testing.T) {
	var payload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer server.Close()
	summary := "【余额预警每日检查】\n" + formatReconFindings(
		[]MissReport{{BillingAccountID: "prepaid-1", Name: "预付费企业", BalanceUSD: -1}},
		[]ExcludeReview{{BillingAccountID: "review-1", Name: "待核实企业", Reason: "未找到明确的授信活动"}},
	) + "\n检查时间：2026-11-25 08:05:00（北京时间）"
	event := buildSyntheticEvent(SendRequest{Mode: SendModePilot, OpsNotice: true, AccountID: "exclude-review", CopyOverride: summary})
	ch := &models.NotifyChannelConfig{Ident: "dingtalk", RequestConfig: &models.RequestConfig{HTTPRequestConfig: &models.HTTPRequestConfig{
		URL: server.URL, Method: "POST", Headers: map[string]string{"Content-Type": "application/json"}, RetryTimes: 1,
		Request: models.RequestDetail{Body: `{"msgtype":"markdown","markdown":{"title":"{{ $tpl.title }}","text":"{{ $tpl.content }}"}}`},
	}}}
	p := &provider.DingtalkProvider{}
	ctx := testSeedNotifyCtx(t)
	SeedMessageTemplates(ctx)
	sender := &N9eNotifySender{Ctx: ctx}
	result := p.Notify(context.Background(), &provider.NotifyRequest{Config: ch, Events: []*models.AlertCurEvent{event}, TplContent: sender.opsNoticeTplContent(event, ch.Ident, ""), HttpClient: server.Client()})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	markdown, ok := payload["markdown"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing markdown: %#v", payload)
	}
	text, _ := markdown["text"].(string)
	if text != summary {
		t.Fatalf("message=%q want=%q", text, summary)
	}
	if strings.Contains(text, "exclude-review") || strings.Contains(text, "余额 ¥0.00") {
		t.Fatalf("synthetic account leaked: %q", text)
	}
	for _, want := range []string{"疑似漏发余额预警：1 家", "预付费企业（账户 prepaid-1）", "待核实企业（账户 review-1）", "未找到明确的授信活动", "检查时间：2026-11-25 08:05:00（北京时间）"} {
		if !strings.Contains(text, want) {
			t.Errorf("message missing %q: %s", want, text)
		}
	}
}

func TestReconTemplateUsesDatabaseAndPreservesEdits(t *testing.T) {
	ctx := testSeedNotifyCtx(t)
	SeedMessageTemplates(ctx)
	tpl, err := models.MessageTemplateGet(ctx, "ident = ?", reconNoticeTemplateIdent)
	if err != nil || tpl == nil {
		t.Fatalf("seeded template: %v %v", tpl, err)
	}
	updated := map[string]string{"title": "运营检查", "content": "运营：{{ index $event.AnnotationsJSON \"copy\" }}", "text": "运营：{{ index $event.AnnotationsJSON \"copy\" }}"}
	if err := models.DB(ctx).Model(tpl).Updates(map[string]interface{}{"content": `{"title":"运营检查","content":"运营：{{ index $event.AnnotationsJSON \"copy\" }}","text":"运营：{{ index $event.AnnotationsJSON \"copy\" }}"}`, "update_by": "operator"}).Error; err != nil {
		t.Fatal(err)
	}
	SeedMessageTemplates(ctx)
	tpl, err = models.MessageTemplateGet(ctx, "ident = ?", reconNoticeTemplateIdent)
	if err != nil || !seedContentEqual(tpl.Content, updated) {
		t.Fatalf("operator changes lost: %v %v", tpl, err)
	}
	event := buildSyntheticEvent(SendRequest{CopyOverride: "详情", OpsNotice: true})
	sender := &N9eNotifySender{Ctx: ctx}
	got := sender.opsNoticeTplContent(event, "dingtalk", "")
	if fmt.Sprint(got["title"]) != "运营检查" || fmt.Sprint(got["content"]) != "运营：详情" {
		t.Fatalf("database template not used: %#v", got)
	}
}

func TestReconTemplateMissingOrInvalidFallsBack(t *testing.T) {
	ctx := testSeedNotifyCtx(t)
	event := buildSyntheticEvent(SendRequest{CopyOverride: "检查详情", OpsNotice: true})
	sender := &N9eNotifySender{Ctx: ctx}
	check := func() {
		t.Helper()
		got := sender.opsNoticeTplContent(event, "dingtalk", "")
		if fmt.Sprint(got["content"]) != "检查详情" {
			t.Fatalf("fallback lost summary: %#v", got)
		}
	}
	check()
	SeedMessageTemplates(ctx)
	tpl, err := models.MessageTemplateGet(ctx, "ident = ?", reconNoticeTemplateIdent)
	if err != nil || tpl == nil {
		t.Fatalf("seeded template: %v %v", tpl, err)
	}
	if err := models.DB(ctx).Model(tpl).Update("content", `{"title":"bad","content":"{{ invalid","text":"{{ invalid"}`).Error; err != nil {
		t.Fatal(err)
	}
	check()
}

func TestFormatReconFindingsLimitsDetails(t *testing.T) {
	misses := make([]MissReport, 7)
	for i := range misses {
		misses[i] = MissReport{BillingAccountID: "same", Name: "企业\n名称"}
	}
	text := formatReconFindings(misses, nil)
	if strings.Count(text, "企业 名称（账户 same）") != 5 || !strings.Contains(text, "其余 2 家请在对账页面查看") || !strings.Contains(text, "需要核实是否误排除：0 家") {
		t.Fatalf("unexpected formatted message: %s", text)
	}
}

func TestOpsNoticeCannotUseCustomerMode(t *testing.T) {
	result := (&N9eNotifySender{}).Send(SendRequest{Mode: SendModeCustomer, OpsNotice: true})
	if result.Err == nil {
		t.Fatal("operations notice must not be sent to customers")
	}
}
