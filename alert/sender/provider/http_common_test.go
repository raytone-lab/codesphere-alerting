package provider

import (
	htmltemplate "html/template"
	"strings"
	"testing"

	"github.com/ccfos/nightingale/v6/models"
)

func TestHTTPTemplateRenderingKeepsLegacyHTMLEscaping(t *testing.T) {
	event := &models.AlertCurEvent{
		RuleName: `rule "quote" <tag> & raw`,
	}
	tplData := map[string]interface{}{
		"tpl": map[string]interface{}{
			"content": htmltemplate.HTML(`rendered \"quote\" &lt;tag&gt;`),
		},
		"params": map[string]string{
			"name": `param "quote" <tag> & raw`,
		},
		"events":  []*models.AlertCurEvent{event},
		"event":   event,
		"sendtos": []string{"13800138000"},
		"sendto":  "13800138000",
	}
	cfg := &models.HTTPRequestConfig{
		Request: models.RequestDetail{
			Body: `{"summary":"{{$event.RuleName}}","param":"{{$params.name}}","content":"{{$tpl.content}}"}`,
		},
	}

	got, err := parseRequestBody(cfg, tplData)
	if err != nil {
		t.Fatalf("parseRequestBody failed: %v", err)
	}
	body := string(got)

	for _, want := range []string{
		`"summary":"rule &#34;quote&#34; &lt;tag&gt; &amp; raw"`,
		`"param":"param &#34;quote&#34; &lt;tag&gt; &amp; raw"`,
		`"content":"rendered \"quote\" &lt;tag&gt;"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered body does not contain %q, got %s", want, body)
		}
	}
}

func TestHTTPVariableRenderingKeepsLegacyHTMLEscaping(t *testing.T) {
	event := &models.AlertCurEvent{
		RuleName: `rule "quote" <tag> & raw`,
	}
	tplData := map[string]interface{}{
		"tpl": map[string]interface{}{
			"content": htmltemplate.HTML(`rendered \"quote\" &lt;tag&gt;`),
		},
		"params": map[string]string{
			"path":  `path "quote" <tag> & raw`,
			"query": `query "quote" <tag> & raw`,
		},
		"events":  []*models.AlertCurEvent{event},
		"event":   event,
		"sendtos": []string{"13800138000"},
		"sendto":  "13800138000",
	}
	cfg := &models.HTTPRequestConfig{
		URL: `https://example.com/{{$params.path}}`,
		Headers: map[string]string{
			"X-Rule":    `{{$event.RuleName}}`,
			"X-Content": `{{$tpl.content}}`,
		},
		Request: models.RequestDetail{
			Parameters: map[string]string{
				"q": `{{$params.query}}`,
			},
		},
	}

	gotURL, gotHeaders, gotParams := replaceVariables(cfg, tplData)

	if want := `https://example.com/path &#34;quote&#34; &lt;tag&gt; &amp; raw`; gotURL != want {
		t.Fatalf("unexpected URL render:\nwant %q\ngot  %q", want, gotURL)
	}
	if want := `rule &#34;quote&#34; &lt;tag&gt; &amp; raw`; gotHeaders["X-Rule"] != want {
		t.Fatalf("unexpected header render:\nwant %q\ngot  %q", want, gotHeaders["X-Rule"])
	}
	if want := `rendered \"quote\" &lt;tag&gt;`; gotHeaders["X-Content"] != want {
		t.Fatalf("unexpected safe header render:\nwant %q\ngot  %q", want, gotHeaders["X-Content"])
	}
	if want := `query &#34;quote&#34; &lt;tag&gt; &amp; raw`; gotParams["q"] != want {
		t.Fatalf("unexpected parameter render:\nwant %q\ngot  %q", want, gotParams["q"])
	}
}

func TestTextTemplateRenderingRemainsRawForAppProviders(t *testing.T) {
	event := &models.AlertCurEvent{
		RuleName: `rule "quote" <tag> & raw`,
	}
	tplData := map[string]interface{}{
		"events": []*models.AlertCurEvent{event},
		"event":  event,
	}

	got := getParsedString("app_provider_text", `{{$event.RuleName}}`, tplData)
	want := `rule "quote" <tag> & raw`
	if got != want {
		t.Fatalf("text template render changed:\nwant %q\ngot  %q", want, got)
	}
}

func TestWebhookErrCodeError(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "dingtalk success", body: `{"errcode":0,"errmsg":"ok"}`},
		{name: "string zero", body: `{"errcode":"0","errmsg":"ok"}`},
		{name: "empty", body: ``},
		{name: "plain ok", body: `ok`},
		{name: "callback json without errcode", body: `{"status":"ok","code":200}`},
		{name: "token missing", body: `{"errcode":300005,"errmsg":"token is not exist"}`, wantErr: "webhook id is not exist"},
		{name: "keyword missing", body: `{"errcode":310000,"errmsg":"keywords not in content"}`, wantErr: "keywords not in content"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := webhookErrCodeError([]byte(c.body))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}
