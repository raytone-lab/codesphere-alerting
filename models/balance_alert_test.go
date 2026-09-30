package models

import (
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testBalanceAlertCtx(t *testing.T) *ctx.Context {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Configs{}, &BalanceAlertConfig{}, &BalanceAlertRecord{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &ctx.Context{DB: db, IsCenter: true}
}

func TestBalanceAlertSettingsRoundTrip(t *testing.T) {
	c := testBalanceAlertCtx(t)
	got, err := BalanceAlertSettingsGet(c)
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if got.SendMode != "OFF" || got.VoucherThresholdUSD != 20 || got.CriticalRepeatDays != 3 || !got.AlertEnabled() {
		t.Fatalf("defaults: %+v", got)
	}
	if got.CriticalRepeatAfter() != 72*time.Hour {
		t.Fatalf("default repeat: %s", got.CriticalRepeatAfter())
	}

	got.SendMode = "PILOT"
	got.PilotReceivers = []string{"dingtalk:ops"}
	got.DatasourceID = 2
	got.CriticalRepeatDays = 5
	if err := BalanceAlertSettingsPut(c, got, "root"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err = BalanceAlertSettingsGet(c)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SendMode != "PILOT" || got.DatasourceID != 2 || len(got.PilotReceivers) != 1 || got.CriticalRepeatDays != 5 {
		t.Fatalf("saved: %+v", got)
	}
	if got.CriticalRepeatAfter() != 5*24*time.Hour {
		t.Fatalf("saved repeat: %s", got.CriticalRepeatAfter())
	}

	got.CriticalRepeatDays = 0
	if err := BalanceAlertSettingsPut(c, got, "root"); err != nil {
		t.Fatalf("put zero days: %v", err)
	}
	got, err = BalanceAlertSettingsGet(c)
	if err != nil {
		t.Fatalf("get after zero: %v", err)
	}
	if got.CriticalRepeatDays != 3 {
		t.Fatalf("zero days should default to 3: %+v", got)
	}

	off := false
	got.Enabled = &off
	if err := BalanceAlertSettingsPut(c, got, "root"); err != nil {
		t.Fatalf("put disabled: %v", err)
	}
	got, err = BalanceAlertSettingsGet(c)
	if err != nil {
		t.Fatalf("get disabled: %v", err)
	}
	if got.AlertEnabled() {
		t.Fatal("expected disabled")
	}

	pub := got.Public()
	if pub.DatasourceID != 2 {
		t.Fatalf("public: %+v", pub)
	}
}

func TestBalanceAlertConfigAndRecord(t *testing.T) {
	c := testBalanceAlertCtx(t)
	missing, err := BalanceAlertConfigGet(c, "acct-1")
	if err != nil || missing != nil {
		t.Fatalf("missing config: cfg=%v err=%v", missing, err)
	}

	cfg := &BalanceAlertConfig{BillingAccountID: "acct-1", Enabled: true, CurrentState: BalanceAlertStateWarn}
	if err := BalanceAlertConfigUpsert(c, cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := BalanceAlertConfigGet(c, "acct-1")
	if err != nil || got == nil || got.CurrentState != BalanceAlertStateWarn {
		t.Fatalf("get cfg: %+v err=%v", got, err)
	}

	now := time.Now()
	rec := &BalanceAlertRecord{
		BillingAccountID: "acct-1",
		Level:            BalanceAlertStateWarn,
		BalanceUSD:       15,
		ThresholdUSD:     20,
		ThresholdMode:    "VOUCHER_FIXED",
		SendMode:         "OFF",
		Status:           "SENT",
		SentAt:           &now,
	}
	if err := BalanceAlertRecordInsert(c, rec); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("expected generated id")
	}
	if err := BalanceAlertRecordSetReview(c, rec.ID, "OK"); err != nil {
		t.Fatalf("review: %v", err)
	}
	lst, err := BalanceAlertRecordGets(c, "acct-1", "", "", 10)
	if err != nil || len(lst) != 1 || lst[0].OperatorReview != "OK" {
		t.Fatalf("reviewed: %+v err=%v", lst, err)
	}
	sent, err := BalanceAlertRecordLastSentAt(c, "acct-1", BalanceAlertStateWarn)
	if err != nil || sent.IsZero() {
		t.Fatalf("last sent: %v err=%v", sent, err)
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	ok, err := BalanceAlertRecordSentToday(c, "acct-1", BalanceAlertStateWarn, dayStart)
	if err != nil || !ok {
		t.Fatalf("sent today: %v err=%v", ok, err)
	}
}

func TestBalanceAlertOTPRoundTrip(t *testing.T) {
	c := testBalanceAlertCtx(t)
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	if err := BalanceAlertOTPBegin(c, "acct-1", "13900000000", now); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := BalanceAlertOTPBegin(c, "acct-1", "13900000000", now.Add(10*time.Second)); err == nil {
		t.Fatal("expected rate limit")
	}
	if err := BalanceAlertOTPMarkVerified(c, "acct-1", "13900000000", now.Add(time.Minute)); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if !BalanceAlertOTPVerified(c, "acct-1", "13900000000", now.Add(time.Minute)) {
		t.Fatal("expected verified")
	}
	if BalanceAlertOTPVerified(c, "acct-1", "13900000000", now.Add(10*time.Minute)) {
		t.Fatal("expired should not stay verified")
	}
}
