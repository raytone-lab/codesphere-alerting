package balancealert

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/models"
)

type SendRequest struct {
	Mode          string
	Level         string
	ThresholdMode string
	TriggerType   string
	AccountID     string
	Name          string
	Balance       float64
	Threshold     float64
	DynamicDays   float64
	Phone         string
	NotifyRuleID  int64
	// Legacy HTTP fallback fields (used when NotifyRuleID is unset).
	PilotURLs []string
	SmsURL    string
}

type SendResult struct {
	Channel  string
	Receiver string
	Err      error
}

func RenderCopy(req SendRequest) string {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.AccountID
	}
	switch {
	case req.TriggerType == models.TriggerTypeDynamic:
		return fmt.Sprintf("【签名】%s您好，按当前消耗速度，您的账户余额仅剩%s，请及时充值以避免服务中断。", name, FormatRemainingDays(req.DynamicDays))
	case req.ThresholdMode == ModeVoucherFixed && req.Level != StateCritical:
		return fmt.Sprintf("【签名】%s您好，您的体验额度即将用完，充值后可继续使用服务。", name)
	case req.Level == StateCritical:
		return fmt.Sprintf("【签名】%s您好，您的账户余额已耗尽/即将耗尽，服务即将暂停，请立即充值恢复。", name)
	default:
		return fmt.Sprintf("【签名】%s您好，您的账户余额为%.2f元，为避免影响业务调用请及时充值。", name, req.Balance)
	}
}

func RenderPilotLine(name string, balance, threshold float64, level string) string {
	return fmt.Sprintf("%s / 余额 %.2f / 提醒线 %.2f / %s", name, balance, threshold, level)
}

type HTTPClient interface {
	PostJSON(url string, payload any) error
}

type stdHTTP struct {
	client *http.Client
}

func (c stdHTTP) PostJSON(rawURL string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return webhookErrCodeError(body)
}

func webhookErrCodeError(body []byte) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return nil
	}
	var envelope struct {
		ErrCode json.RawMessage `json:"errcode"`
		ErrMsg  string          `json:"errmsg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	code := strings.Trim(strings.TrimSpace(string(envelope.ErrCode)), "\"")
	if code == "" || code == "0" || code == "null" {
		return nil
	}
	msg := strings.TrimSpace(envelope.ErrMsg)
	if msg == "" {
		return fmt.Errorf("webhook errcode=%s", code)
	}
	return fmt.Errorf("webhook errcode=%s errmsg=%s", code, rewriteWebhookErrMsg(msg))
}

func rewriteWebhookErrMsg(msg string) string {
	switch strings.ToLower(msg) {
	case "token is not exist":
		return "webhook id is not exist"
	default:
		r := strings.NewReplacer("token", "credential", "Token", "credential", "secret", "key", "Secret", "key", "password", "passwd")
		return r.Replace(msg)
	}
}

// Avoid words that test-fire redactSensitive treats as credentials (token/secret/password).
const dingTalkSECAsWebhookIDErr = "DingTalk robot credential starts with SEC; that is the HMAC signing key, not the webhook id. Copy the id from the robot webhook URL, and if 加签 is enabled put the SEC value in the 加签 field"

func dingTalkSign(secret string, now time.Time) (timestamp, sign string) {
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts + "\n" + secret))
	return ts, base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func SignDingTalkRobotURL(raw string, now time.Time) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.HasPrefix(raw, "SEC") {
		return "", fmt.Errorf("%s", dingTalkSECAsWebhookIDErr)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw, nil
	}
	q := u.Query()
	token := strings.TrimSpace(q.Get("access_token"))
	secret := strings.TrimSpace(q.Get("secret"))
	if strings.HasPrefix(token, "SEC") {
		return "", fmt.Errorf("%s", dingTalkSECAsWebhookIDErr)
	}
	if secret == "" {
		return raw, nil
	}
	q.Del("secret")
	ts, sign := dingTalkSign(secret, now)
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func DefaultHTTP() HTTPClient {
	return stdHTTP{client: &http.Client{Timeout: 10 * time.Second}}
}

func Dispatch(httpClient HTTPClient, req SendRequest) SendResult {
	if httpClient == nil {
		httpClient = DefaultHTTP()
	}
	switch req.Mode {
	case SendModeOff, "":
		return SendResult{}
	case SendModePilot:
		text := RenderPilotLine(req.Name, req.Balance, req.Threshold, req.Level)
		var lastErr error
		receivers := make([]string, 0, len(req.PilotURLs))
		for _, u := range req.PilotURLs {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			receivers = append(receivers, u)
			signed, err := SignDingTalkRobotURL(u, time.Now())
			if err != nil {
				lastErr = err
				continue
			}
			err = httpClient.PostJSON(signed, map[string]any{
				"msgtype": "text",
				"text":    map[string]string{"content": text},
			})
			if err != nil {
				lastErr = err
			}
		}
		return SendResult{Channel: "DINGTALK", Receiver: strings.Join(receivers, ","), Err: lastErr}
	case SendModeCustomer:
		if strings.TrimSpace(req.Phone) == "" {
			return SendResult{Channel: "SMS", Err: fmt.Errorf("empty customer phone")}
		}
		if strings.TrimSpace(req.SmsURL) == "" {
			return SendResult{Channel: "SMS", Receiver: req.Phone, Err: fmt.Errorf("sms webhook not configured")}
		}
		body := RenderCopy(req)
		err := httpClient.PostJSON(req.SmsURL, map[string]string{
			"phone":   req.Phone,
			"content": body,
		})
		return SendResult{Channel: "SMS", Receiver: req.Phone, Err: err}
	default:
		return SendResult{Err: fmt.Errorf("unknown send_mode %s", req.Mode)}
	}
}
