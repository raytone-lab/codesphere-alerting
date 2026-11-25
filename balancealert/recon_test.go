package balancealert

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
)

type reconReviewStore struct {
	fakeStore
	reviewed []Account
}

func (s *reconReviewStore) ReviewLowBalanceExcludes(_ context.Context, accounts []Account, _ int) ([]ExcludeReview, error) {
	s.reviewed = accounts
	return []ExcludeReview{{BillingAccountID: "credit", Name: "授信企业", Reason: "核销资金分类不完整或关联异常，需人工核实"}}, nil
}

func TestReconMissReportsUsesStaticThresholdAndRetainsCreditReview(t *testing.T) {
	n9e := testRunnerCtx(t)
	now := time.Now()
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{{BillingAccountID: "credit", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: now}}); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []*models.BalanceAlertConfig{
		{BillingAccountID: "custom", ThresholdMode: models.ThresholdModeCustom, ThresholdFixedUSD: 10},
		{BillingAccountID: "invalid-custom", ThresholdMode: models.ThresholdModeCustom, ThresholdFixedUSD: 0},
	} {
		if err := models.BalanceAlertConfigUpsert(n9e, cfg); err != nil {
			t.Fatal(err)
		}
	}
	if err := models.BalanceAlertRecordInsert(n9e, &models.BalanceAlertRecord{ID: "sent", BillingAccountID: "already-sent", Level: StateCritical, Status: StatusSent, SentAt: &now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := models.BalanceAlertRecordInsert(n9e, &models.BalanceAlertRecord{ID: "failed", BillingAccountID: "failed", Level: StateCritical, Status: StatusFailed, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	store := &reconReviewStore{fakeStore: fakeStore{accts: []Account{
		{ID: "empty"}, {ID: "invalid-custom"}, {ID: "custom"}, {ID: "recharge", LastRecharge: recharge(100)},
		{ID: "voucher", HasVoucher: true}, {ID: "already-sent", LastRecharge: recharge(100)},
		{ID: "failed", LastRecharge: recharge(100)}, {ID: "positive", Balance: 1, LastRecharge: recharge(100)}, {ID: "credit"},
	}}}
	out, err := reconMissReports(n9e, store, models.DefaultBalanceAlertSettings(), MissReportResult{AsOf: now, LookbackDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, item := range out.Misses {
		got[item.BillingAccountID] = true
	}
	for _, id := range []string{"custom", "recharge", "voucher", "failed"} {
		if !got[id] {
			t.Errorf("missing candidate %s: %v", id, got)
		}
		delete(got, id)
	}
	if len(got) != 0 {
		t.Errorf("unexpected candidates: %v", got)
	}
	if len(store.reviewed) != 1 || store.reviewed[0].ID != "credit" || len(out.ExcludeReviews) != 1 || out.ExcludeReviews[0].BillingAccountID != "credit" {
		t.Fatalf("credit review lost: reviewed=%+v result=%+v", store.reviewed, out.ExcludeReviews)
	}
}

func TestReconMissReportsFailsOnConfigReadError(t *testing.T) {
	n9e := testRunnerCtx(t)
	if err := n9e.DB.Migrator().DropTable(&models.BalanceAlertConfig{}); err != nil {
		t.Fatal(err)
	}
	_, err := reconMissReports(n9e, fakeStore{accts: []Account{{ID: "empty"}}}, models.DefaultBalanceAlertSettings(), MissReportResult{AsOf: time.Now(), LookbackDays: 7})
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("expected config read failure, got %v", err)
	}
}
