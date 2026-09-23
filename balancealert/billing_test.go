package balancealert

import (
	"strings"
	"testing"
)

func TestResolveAccountByEmailOrPhoneSQL(t *testing.T) {
	needles := []string{
		"owner_user_id = u.id",
		"type = 'enterprise'",
		"status = 'active'",
		"deleted_at IS NULL",
		"LOWER(TRIM(COALESCE(u.email, '')))",
		"TRIM(COALESCE(u.phone, ''))",
		"account_wallets",
	}
	for _, n := range needles {
		if !strings.Contains(ResolveAccountByEmailOrPhoneSQL, n) {
			t.Errorf("ResolveAccountByEmailOrPhoneSQL missing %q", n)
		}
	}
	if strings.Contains(ResolveAccountByEmailOrPhoneSQL, "$1") {
		t.Error("use GORM ? placeholders, not native $1")
	}
}

func TestPrepaidSelectSQL(t *testing.T) {
	needles := []string{
		"type = 'enterprise'",
		"status = 'active'",
		"deleted_at IS NULL",
		"provisioning_quarantined_at IS NULL",
		"billing_account_credit_configs",
		"status = 'ACTIVE'",
		"limit_usd > 0",
		"terms_days > 0",
		"type = 'RECHARGE'",
		"type = 'ADJUST' AND description LIKE 'voucher:%'",
		"account_wallets",
		"users u ON u.id = p.owner_user_id AND u.deleted_at IS NULL",
	}
	for _, n := range needles {
		if !strings.Contains(PrepaidSelectSQL, n) {
			t.Errorf("PrepaidSelectSQL missing %q", n)
		}
	}
}
