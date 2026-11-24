package balancealert

import (
	"context"
	"testing"
)

func TestExcludeReviewIndependentlyAgainstPostgres(t *testing.T) {
	db := pgTestDB(t)
	for _, id := range []string{"stale", "unknown", "credit", "both"} {
		account(t, db, id, "enterprise", "active", false)
	}
	advance(t, db, "credit", 10, 10, 200)
	settlement(t, db, "credit", "ENTERPRISE_CREDIT", 1)
	advance(t, db, "both", 10, 0, 1)
	for _, id := range []string{"unknown", "both"} {
		var allocation int64
		if err := db.Raw(`INSERT INTO balance_consumption_allocations (billing_account_id, funding_classification_complete, created_at) VALUES (?, false, ?) RETURNING id`, id, daysAgo(200)).Scan(&allocation).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO enterprise_credit_settlements (billing_account_id, allocation_id, debt_source, occurred_at) VALUES (?, ?, 'ENTERPRISE_CREDIT', ?)`, id, allocation, daysAgo(1)).Error; err != nil {
			t.Fatal(err)
		}
	}
	accounts := []Account{{ID: "stale"}, {ID: "unknown"}, {ID: "credit"}, {ID: "both"}}
	reviews, err := (&PGStore{db: db}).ReviewLowBalanceExcludes(context.Background(), accounts, 90)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range reviews {
		found[r.BillingAccountID] = true
	}
	for _, id := range []string{"stale", "unknown", "both"} {
		if !found[id] {
			t.Errorf("%s missing from reviews: %+v", id, reviews)
		}
	}
	if found["credit"] {
		t.Errorf("confirmed credit should not require review: %+v", reviews)
	}
}
