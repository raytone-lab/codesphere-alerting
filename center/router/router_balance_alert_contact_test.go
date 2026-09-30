package router

import (
	"testing"

	"github.com/ccfos/nightingale/v6/models"
)

func TestLoginContactPrefersPhoneThenUsername(t *testing.T) {
	email, phone := loginContact(&models.User{
		Username: "13800000001",
		Email:    "test_alert_001@codesphere.test",
		Phone:    "13800000001",
	})
	if email != "test_alert_001@codesphere.test" || phone != "13800000001" {
		t.Fatalf("both set: email=%q phone=%q", email, phone)
	}

	email, phone = loginContact(&models.User{
		Username: "13800000001",
		Email:    "a@example.com",
	})
	if phone != "13800000001" || email != "a@example.com" {
		t.Fatalf("username as phone: email=%q phone=%q", email, phone)
	}

	email, phone = loginContact(&models.User{
		Username: "enterprise-v2-demo-003@example.com",
		Email:    "enterprise-v2-demo-003@example.com",
	})
	if phone != "" || email != "enterprise-v2-demo-003@example.com" {
		t.Fatalf("email-only: email=%q phone=%q", email, phone)
	}
}
