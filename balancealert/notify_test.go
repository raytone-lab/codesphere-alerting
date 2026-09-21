package balancealert

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRenderCopy(t *testing.T) {
	warn := RenderCopy(StateWarn, ModeLastRecharge5Pct, "数商云", 15.5)
	if warn != "【签名】数商云您好，您的账户余额为15.50元，为避免影响业务调用请及时充值。" {
		t.Fatalf("warn copy: %s", warn)
	}
	crit := RenderCopy(StateCritical, ModeVoucherFixed, "数商云", -0.04)
	if crit != "【签名】数商云您好，您的账户余额已耗尽/即将耗尽，服务即将暂停，请立即充值恢复。" {
		t.Fatalf("critical copy must win over voucher: %s", crit)
	}
	voucher := RenderCopy(StateWarn, ModeVoucherFixed, "数商云", 15)
	if voucher != "【签名】数商云您好，您的体验额度即将用完，充值后可继续使用服务。" {
		t.Fatalf("voucher copy: %s", voucher)
	}
}

func TestRenderPilotLine(t *testing.T) {
	got := RenderPilotLine("数商云", 15.2, 20, StateWarn)
	if got != "数商云 / 余额 15.20 / 提醒线 20.00 / WARN" {
		t.Fatalf("pilot: %s", got)
	}
}

type fakeHTTP struct {
	urls     []string
	payloads []any
	err      error
}

func (f *fakeHTTP) PostJSON(url string, payload any) error {
	f.urls = append(f.urls, url)
	f.payloads = append(f.payloads, payload)
	return f.err
}

func TestDispatchModes(t *testing.T) {
	httpClient := &fakeHTTP{}
	if res := Dispatch(httpClient, SendRequest{Mode: SendModeOff, Level: StateWarn, Name: "A", Balance: 1}); res.Channel != "" || len(httpClient.urls) != 0 {
		t.Fatalf("OFF must not send: %+v urls=%v", res, httpClient.urls)
	}

	res := Dispatch(httpClient, SendRequest{
		Mode: SendModePilot, Level: StateWarn, Name: "A", Balance: 15, Threshold: 20,
		PilotURLs: []string{"https://oapi.dingtalk.com/robot/send?access_token=x"},
	})
	if res.Channel != "DINGTALK" || len(httpClient.urls) != 1 {
		t.Fatalf("PILOT: %+v urls=%v", res, httpClient.urls)
	}

	httpClient = &fakeHTTP{}
	res = Dispatch(httpClient, SendRequest{
		Mode: SendModeCustomer, Level: StateWarn, ThresholdMode: ModeLastRecharge5Pct,
		Name: "A", Balance: 15, Phone: "13800000000", SmsURL: "https://sms.example/send",
	})
	if res.Channel != "SMS" || res.Receiver != "13800000000" || len(httpClient.urls) != 1 {
		t.Fatalf("CUSTOMER: %+v urls=%v", res, httpClient.urls)
	}
}

func TestWebhookErrCodeError(t *testing.T) {
	if err := webhookErrCodeError([]byte(`{"errcode":0,"errmsg":"ok"}`)); err != nil {
		t.Fatalf("success body: %v", err)
	}
	err := webhookErrCodeError([]byte(`{"errcode":300005,"errmsg":"token is not exist"}`))
	if err == nil || !strings.Contains(err.Error(), "webhook id is not exist") {
		t.Fatalf("error = %v", err)
	}
}

func TestSignDingTalkRobotURL(t *testing.T) {
	now := time.UnixMilli(1700000000000)
	if _, err := SignDingTalkRobotURL("SECdeadbeef", now); err == nil {
		t.Fatal("bare SEC secret must be rejected")
	}
	if _, err := SignDingTalkRobotURL("https://oapi.dingtalk.com/robot/send?access_token=SECdeadbeef", now); err == nil {
		t.Fatal("SEC used as access_token must be rejected")
	}

	plain := "https://oapi.dingtalk.com/robot/send?access_token=real-token"
	got, err := SignDingTalkRobotURL(plain, now)
	if err != nil || got != plain {
		t.Fatalf("unsigned url: got %q err=%v", got, err)
	}

	got, err = SignDingTalkRobotURL(plain+"&secret=SECtest", now)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("access_token") != "real-token" {
		t.Fatalf("access_token = %q", q.Get("access_token"))
	}
	if q.Get("secret") != "" {
		t.Fatalf("secret must be stripped, got %q", q.Get("secret"))
	}
	wantTS, wantSign := dingTalkSign("SECtest", now)
	if q.Get("timestamp") != wantTS || q.Get("sign") != wantSign {
		t.Fatalf("query = %v", q)
	}
}

func TestDispatchRejectsSECToken(t *testing.T) {
	httpClient := &fakeHTTP{}
	res := Dispatch(httpClient, SendRequest{
		Mode: SendModePilot, Level: StateWarn, Name: "A", Balance: 15, Threshold: 20,
		PilotURLs: []string{"https://oapi.dingtalk.com/robot/send?access_token=SECdeadbeef"},
	})
	if res.Err == nil || !strings.Contains(res.Err.Error(), "starts with SEC") {
		t.Fatalf("error = %v", res.Err)
	}
	if len(httpClient.urls) != 0 {
		t.Fatalf("must not POST with a SEC token: %v", httpClient.urls)
	}
}
