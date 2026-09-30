package authing

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Authing/authing-golang-sdk/v3/authentication"
	"github.com/Authing/authing-golang-sdk/v3/dto"
	"github.com/toolkits/pkg/logger"
)

const (
	channelLogin       = "CHANNEL_LOGIN"
	phoneCountryCodeCN = "+86"
)

var (
	ErrNotConfigured = errors.New("Authing 未配置")
	errSendFailed    = errors.New("验证码发送失败，请稍后重试")
	errVerifyFailed  = errors.New("验证码错误")
)

// PhoneCodes is the Authing SMS OTP surface used by 我的余额告警.
type PhoneCodes interface {
	SendLoginSMS(phone string) error
	VerifyPhoneCode(phone, code string) error
}

type Config struct {
	AppHost     string
	AppID       string
	AppSecret   string
	RedirectURI string
}

func (c Config) Ready() bool {
	return strings.TrimSpace(c.AppHost) != "" &&
		strings.TrimSpace(c.AppID) != "" &&
		strings.TrimSpace(c.AppSecret) != "" &&
		strings.TrimSpace(c.RedirectURI) != ""
}

type Client struct {
	inner *authentication.AuthenticationClient
}

func New(cfg Config) (phone PhoneCodes, err error) {
	if !cfg.Ready() {
		return nil, ErrNotConfigured
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Authing 初始化失败")
			logger.Warningf("Authing NewAuthenticationClient panic: %v", r)
		}
	}()
	inner, err := authentication.NewAuthenticationClient(&authentication.AuthenticationClientOptions{
		AppId:       strings.TrimSpace(cfg.AppID),
		AppSecret:   strings.TrimSpace(cfg.AppSecret),
		AppHost:     strings.TrimRight(strings.TrimSpace(cfg.AppHost), "/"),
		RedirectUri: strings.TrimSpace(cfg.RedirectURI),
		Timeout:     10000,
		ReadTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &Client{inner: inner}, nil
}

func (c *Client) SendLoginSMS(phone string) error {
	resp := c.inner.SendSms(&dto.SendSMSDto{
		Channel:          channelLogin,
		PhoneNumber:      phone,
		PhoneCountryCode: phoneCountryCodeCN,
	})
	return mapSendResult(resp)
}

func (c *Client) VerifyPhoneCode(phone, code string) error {
	resp := c.inner.SignInByPhonePassCode(phone, strings.TrimSpace(code), phoneCountryCodeCN, dto.SignInOptionsDto{
		// Match codesphere-user verifyExistingPhoneCode: do not auto-create Authing users.
		AutoRegister: false,
	})
	return mapVerifyResult(resp)
}

func mapSendResult(resp *dto.SendSMSRespDto) error {
	if resp == nil || resp.StatusCode != 200 {
		if resp != nil {
			logger.Warningf("Authing sendSms failed statusCode=%d apiCode=%d message=%s",
				resp.StatusCode, resp.ApiCode, resp.Message)
		} else {
			logger.Warningf("Authing sendSms returned empty response")
		}
		return errSendFailed
	}
	return nil
}

func mapVerifyResult(resp *dto.LoginTokenRespDto) error {
	if resp == nil || resp.StatusCode != 200 || strings.TrimSpace(resp.Data.AccessToken) == "" {
		if resp != nil {
			logger.Warningf("Authing signInByPhonePassCode failed statusCode=%d apiCode=%d message=%s",
				resp.StatusCode, resp.ApiCode, resp.Message)
		} else {
			logger.Warningf("Authing signInByPhonePassCode returned empty response")
		}
		return errVerifyFailed
	}
	return nil
}

// Lazy constructs the official SDK client on first use so n9e-center can start
// even if Authing is briefly unreachable (the SDK checks appId/secret over HTTP).
func Lazy(cfg Config) PhoneCodes {
	return &lazyClient{cfg: cfg}
}

type lazyClient struct {
	cfg   Config
	once  sync.Once
	inner PhoneCodes
	err   error
}

func (l *lazyClient) get() (PhoneCodes, error) {
	l.once.Do(func() {
		l.inner, l.err = New(l.cfg)
	})
	if l.err != nil {
		if errors.Is(l.err, ErrNotConfigured) {
			return nil, ErrNotConfigured
		}
		logger.Warningf("Authing client init failed: %v", l.err)
		return nil, ErrNotConfigured
	}
	return l.inner, nil
}

func (l *lazyClient) SendLoginSMS(phone string) error {
	c, err := l.get()
	if err != nil {
		return err
	}
	return c.SendLoginSMS(phone)
}

func (l *lazyClient) VerifyPhoneCode(phone, code string) error {
	c, err := l.get()
	if err != nil {
		return err
	}
	return c.VerifyPhoneCode(phone, code)
}
