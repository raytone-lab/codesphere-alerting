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
		"last_recharge_usd",
		"has_voucher",
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
		"type = 'RECHARGE'",
		"type = 'ADJUST' AND description LIKE 'voucher:%'",
		"account_wallets",
		"users u ON u.id = p.owner_user_id AND u.deleted_at IS NULL",
		"p.created_at",
	}
	for _, n := range needles {
		if !strings.Contains(PrepaidSelectSQL, n) {
			t.Errorf("PrepaidSelectSQL missing %q", n)
		}
	}
	// 旧口径必须彻底退出圈选，否则两套客群定义会同时生效。
	// 排除名单已物化到 alerting 库，计费库 SQL 里不应再出现它（跨库无法 join）。
	for _, banned := range []string{"billing_account_credit_configs", "balance_consumption_allocations", "balance_alert_credit_excludes"} {
		if strings.Contains(PrepaidSelectSQL, banned) {
			t.Errorf("PrepaidSelectSQL must not reference %q", banned)
		}
	}
}

func TestConsumptionSelectSQL(t *testing.T) {
	needles := []string{"amount_7d", "amount_3d", "amount_today", "type = 'CONSUME'"}
	for _, n := range needles {
		if !strings.Contains(ConsumptionSelectSQL, n) {
			t.Errorf("ConsumptionSelectSQL missing %q", n)
		}
	}
}

func TestBillingUserSelectSQL(t *testing.T) {
	needles := []string{
		"FROM users u",
		"deleted_at IS NULL",
		"is_active",
		"u.email",
		"u.phone",
		"u.nickname",
	}
	for _, n := range needles {
		if !strings.Contains(BillingUserSelectSQL, n) {
			t.Errorf("BillingUserSelectSQL missing %q", n)
		}
	}
}
