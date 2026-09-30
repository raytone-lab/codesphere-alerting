package authing

import (
	"errors"
	"testing"

	"github.com/Authing/authing-golang-sdk/v3/dto"
)

func TestConfigReady(t *testing.T) {
	cfg := Config{
		AppHost:     "https://raytone.authing.cn",
		AppID:       "app",
		AppSecret:   "secret",
		RedirectURI: "http://localhost:5173/auth-callback/authing",
	}
	if !cfg.Ready() {
		t.Fatal("expected ready")
	}
	cfg.AppSecret = ""
	if cfg.Ready() {
		t.Fatal("secret required")
	}
}

func TestLazyNotConfigured(t *testing.T) {
	c := Lazy(Config{})
	if err := c.SendLoginSMS("13900000000"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("send: %v", err)
	}
	if err := c.VerifyPhoneCode("13900000000", "123456"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("verify: %v", err)
	}
}

func TestMapSendResult(t *testing.T) {
	if err := mapSendResult(&dto.SendSMSRespDto{StatusCode: 200}); err != nil {
		t.Fatalf("ok: %v", err)
	}
	if err := mapSendResult(nil); !errors.Is(err, errSendFailed) {
		t.Fatalf("nil: %v", err)
	}
	if err := mapSendResult(&dto.SendSMSRespDto{StatusCode: 400, Message: "用户不存在"}); !errors.Is(err, errSendFailed) {
		t.Fatalf("fail: %v", err)
	}
}

func TestMapVerifyResult(t *testing.T) {
	if err := mapVerifyResult(&dto.LoginTokenRespDto{
		StatusCode: 200,
		Data:       dto.LoginTokenResponseDataDto{AccessToken: "tok"},
	}); err != nil {
		t.Fatalf("ok: %v", err)
	}
	if err := mapVerifyResult(&dto.LoginTokenRespDto{StatusCode: 200}); !errors.Is(err, errVerifyFailed) {
		t.Fatalf("no token: %v", err)
	}
	if err := mapVerifyResult(&dto.LoginTokenRespDto{StatusCode: 400, Message: "验证码错误"}); !errors.Is(err, errVerifyFailed) {
		t.Fatalf("fail: %v", err)
	}
}
