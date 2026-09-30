package cconf

import (
	"testing"
)

func TestAuthingOverlayEnvDoesNotTouchSecret(t *testing.T) {
	t.Setenv("AUTHING_APP_HOST", "https://from-env.authing.cn")
	t.Setenv("AUTHING_APP_SECRET", "from-env")
	t.Setenv("AUTHING_REDIRECT_URI", "http://localhost:9/callback")

	a := Authing{
		AppHost:     "https://from-toml.authing.cn",
		AppSecret:   "from-toml",
		RedirectURI: "http://localhost:5173/auth-callback/authing",
	}
	a.OverlayEnv()
	if a.AppSecret != "from-toml" {
		t.Fatalf("AppSecret must stay from config.toml, got %q", a.AppSecret)
	}
	if a.AppHost != "https://from-env.authing.cn" {
		t.Fatalf("host overlay: %s", a.AppHost)
	}
	if a.RedirectURI != "http://localhost:9/callback" {
		t.Fatalf("redirect overlay: %s", a.RedirectURI)
	}
}
