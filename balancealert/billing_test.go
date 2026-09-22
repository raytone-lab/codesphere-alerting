package balancealert

import (
	"strings"
	"testing"
)

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
