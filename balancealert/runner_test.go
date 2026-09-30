package balancealert

import (
	"context"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fakeStore struct {
	accts        []Account
	consumption  []Consumption
	billingUsers []BillingUser
	err          error
}

func (f fakeStore) ListPrepaid(ctx context.Context) ([]Account, error) {
	return f.accts, f.err
}

func (f fakeStore) ListConsumption(ctx context.Context) ([]Consumption, error) {
	return f.consumption, f.err
}

func (f fakeStore) ResolveAccountByEmailOrPhone(ctx context.Context, email, phone string) (*MyAccount, error) {
	return nil, nil
}

func (f fakeStore) ListBillingUsers(ctx context.Context) ([]BillingUser, error) {
	return f.billingUsers, f.err
}

func testRunnerCtx(t *testing.T) *ctx.Context {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Configs{}, &models.BalanceAlertConfig{}, &models.BalanceAlertRecord{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &ctx.Context{DB: db, IsCenter: true}
}

func recharge(v float64) *float64 { return &v }

func TestRunnerOFFProducesRecordsWithoutHTTP(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	s.SendMode = SendModeOff
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}

	httpClient := &fakeHTTP{}
	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "数商云", Balance: 15, HasVoucher: true, Phone: "13800000000"},
			{ID: "ba2", Name: "充值户", Balance: 40, LastRecharge: recharge(1000)},
			{ID: "ba3", Name: "无入账", Balance: 10},
		}},
		HTTP:   httpClient,
		Sender: HTTPSender{HTTP: httpClient},
		Now:    func() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai) },
	}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedIncome != 1 {
		t.Fatalf("ba3 should skip: %+v", stats)
	}
	if stats.Sent != 2 || len(httpClient.urls) != 0 {
		t.Fatalf("OFF should persist without HTTP: stats=%+v urls=%v", stats, httpClient.urls)
	}
	recs, err := models.BalanceAlertRecordGets(n9e, "", "", "", 20)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records: n=%d err=%v", len(recs), err)
	}
}

func TestRunnerPilotAndCustomerAndCooldown(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	s.SendMode = SendModePilot
	s.PilotReceivers = []string{"https://oapi.dingtalk.com/robot/send?access_token=x"}
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai)
	httpClient := &fakeHTTP{}
	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "数商云", Balance: 15, HasVoucher: true, Phone: "13800000000"},
		}},
		HTTP:   httpClient,
		Sender: HTTPSender{HTTP: httpClient},
		Now:    func() time.Time { return now },
	}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 || len(httpClient.urls) != 1 {
		t.Fatalf("PILOT send: %+v urls=%v", stats, httpClient.urls)
	}

	cfg, err := models.BalanceAlertConfigGet(n9e, "ba1")
	if err != nil || cfg == nil {
		t.Fatalf("cfg: %v", err)
	}
	cfg.CurrentState = StateNormal
	if err := models.BalanceAlertConfigUpsert(n9e, cfg); err != nil {
		t.Fatal(err)
	}
	stats, err = r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Cooldown != 1 || stats.Sent != 0 {
		t.Fatalf("same-day reentry cooldown: %+v", stats)
	}

	stats, err = r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 0 {
		t.Fatalf("same-day stay WARN must not resend: %+v", stats)
	}
	recs, err := models.BalanceAlertRecordGets(n9e, "ba1", "", "SENT", 20)
	if err != nil || len(recs) != 1 {
		t.Fatalf("same-day still one SENT record: n=%d err=%v", len(recs), err)
	}

	s.SendMode = SendModeCustomer
	s.SmsWebhook = "https://sms.example/send"
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return now.Add(24 * time.Hour) }
	httpClient = &fakeHTTP{}
	r.HTTP = httpClient
	r.Sender = HTTPSender{HTTP: httpClient}
	// still WARN (15 < 20, 15 < 30 hysteresis), same state so Decide stays WARN without send
	stats, err = r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 0 {
		t.Fatalf("stay WARN must not resend next day: %+v", stats)
	}

	r.Store = fakeStore{accts: []Account{
		{ID: "ba1", Name: "数商云", Balance: 9, HasVoucher: true, Phone: "13800000000"},
	}}
	stats, err = r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 || len(httpClient.urls) != 1 {
		t.Fatalf("CUSTOMER CRITICAL upgrade: %+v urls=%v", stats, httpClient.urls)
	}
	if httpClient.urls[0] != "https://sms.example/send" {
		t.Fatalf("sms url: %v", httpClient.urls)
	}
}

func TestRunnerCriticalRepeatUsesSettings(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	s.SendMode = SendModeOff
	s.CriticalRepeatDays = 1
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai)
	sentAt := now.Add(-48 * time.Hour)
	cfg := &models.BalanceAlertConfig{
		BillingAccountID: "ba1",
		Enabled:          true,
		CurrentState:     StateCritical,
		StateSince:       &sentAt,
	}
	if err := models.BalanceAlertConfigUpsert(n9e, cfg); err != nil {
		t.Fatal(err)
	}
	if err := models.BalanceAlertRecordInsert(n9e, &models.BalanceAlertRecord{
		BillingAccountID: "ba1",
		Level:            StateCritical,
		BalanceUSD:       9,
		ThresholdUSD:     20,
		ThresholdMode:    ModeVoucherFixed,
		SendMode:         SendModeOff,
		Status:           StatusSent,
		SentAt:           &sentAt,
		CreatedAt:        sentAt,
		TriggerType:      models.TriggerTypeStatic,
	}); err != nil {
		t.Fatal(err)
	}

	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "数商云", Balance: 9, HasVoucher: true},
		}},
		Now: func() time.Time { return now },
	}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 {
		t.Fatalf("1-day CRITICAL interval after 2 days should send: %+v", stats)
	}

	s.CriticalRepeatDays = 5
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
	n9e2 := testRunnerCtx(t)
	if err := models.BalanceAlertSettingsPut(n9e2, s, "root"); err != nil {
		t.Fatal(err)
	}
	cfg2 := &models.BalanceAlertConfig{
		BillingAccountID: "ba1",
		Enabled:          true,
		CurrentState:     StateCritical,
		StateSince:       &sentAt,
	}
	if err := models.BalanceAlertConfigUpsert(n9e2, cfg2); err != nil {
		t.Fatal(err)
	}
	if err := models.BalanceAlertRecordInsert(n9e2, &models.BalanceAlertRecord{
		BillingAccountID: "ba1",
		Level:            StateCritical,
		BalanceUSD:       9,
		ThresholdUSD:     20,
		ThresholdMode:    ModeVoucherFixed,
		SendMode:         SendModeOff,
		Status:           StatusSent,
		SentAt:           &sentAt,
		CreatedAt:        sentAt,
		TriggerType:      models.TriggerTypeStatic,
	}); err != nil {
		t.Fatal(err)
	}
	r2 := &Runner{
		Ctx: n9e2,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "数商云", Balance: 9, HasVoucher: true},
		}},
		Now: func() time.Time { return now },
	}
	stats, err = r2.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 0 {
		t.Fatalf("5-day CRITICAL interval after 2 days must not send: %+v", stats)
	}
}

func TestRunnerCustomWarnDoesNotOverridePlatformCritical(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	s.SendMode = SendModeOff
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
	cfg := &models.BalanceAlertConfig{
		BillingAccountID:  "ba1",
		Enabled:           true,
		CurrentState:      StateNormal,
		ThresholdMode:     models.ThresholdModeCustom,
		ThresholdFixedUSD: 100,
	}
	if err := models.BalanceAlertConfigUpsert(n9e, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai)
	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "数商云", Balance: 50, HasVoucher: true},
		}},
		Now: func() time.Time { return now },
	}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 {
		t.Fatalf("custom WARN should send: %+v", stats)
	}
	recs, err := models.BalanceAlertRecordGets(n9e, "ba1", "", "", 10)
	if err != nil || len(recs) != 1 || recs[0].Level != StateWarn {
		t.Fatalf("want WARN record: %+v err=%v", recs, err)
	}

	r.Store = fakeStore{accts: []Account{
		{ID: "ba1", Name: "数商云", Balance: 8, HasVoucher: true},
	}}
	r.Now = func() time.Time { return now.Add(24 * time.Hour) }
	stats, err = r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 {
		t.Fatalf("platform CRITICAL should still send: %+v", stats)
	}
	recs, err = models.BalanceAlertRecordGets(n9e, "ba1", StateCritical, "", 10)
	if err != nil || len(recs) == 0 {
		t.Fatalf("want CRITICAL: %+v err=%v", recs, err)
	}
}

func TestRunnerWarnDisabledStillSendsCritical(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	s.SendMode = SendModeOff
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
	if err := models.BalanceAlertConfigUpsert(n9e, &models.BalanceAlertConfig{
		BillingAccountID: "ba1",
		Enabled:          false,
		CurrentState:     StateNormal,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := models.BalanceAlertConfigGet(n9e, "ba1")
	if err != nil || got == nil || got.Enabled {
		t.Fatalf("precondition enabled=false: %+v err=%v", got, err)
	}
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai)
	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "数商云", Balance: 15, HasVoucher: true},
		}},
		Now: func() time.Time { return now },
	}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 0 {
		t.Fatalf("WARN off must not send at 15: %+v", stats)
	}
	cfg, _ := models.BalanceAlertConfigGet(n9e, "ba1")
	if cfg == nil || cfg.CurrentState != StateWarn {
		t.Fatalf("should track WARN state: %+v", cfg)
	}

	r.Store = fakeStore{accts: []Account{
		{ID: "ba1", Name: "数商云", Balance: 8, HasVoucher: true},
	}}
	stats, err = r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 {
		t.Fatalf("CRITICAL must send when WARN is off: %+v", stats)
	}
}

func TestRunnerCustomerUsesConfiguredReceivers(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	s.SendMode = SendModeCustomer
	s.SmsWebhook = "https://sms.example/send"
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
	cfg := &models.BalanceAlertConfig{
		BillingAccountID: "ba1",
		Enabled:          true,
		CurrentState:     StateNormal,
	}
	cfg.SetReceiverList([]string{"13900000000"})
	if err := models.BalanceAlertConfigUpsert(n9e, cfg); err != nil {
		t.Fatal(err)
	}
	httpClient := &fakeHTTP{}
	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "数商云", Balance: 15, HasVoucher: true, Phone: "13800000000"},
		}},
		HTTP:   httpClient,
		Sender: HTTPSender{HTTP: httpClient},
		Now:    func() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai) },
	}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 || len(httpClient.urls) != 1 {
		t.Fatalf("send: %+v urls=%v", stats, httpClient.urls)
	}
	payload, _ := httpClient.payloads[0].(map[string]string)
	if payload["phone"] != "13900000000" {
		t.Fatalf("receiver payload: %+v", httpClient.payloads[0])
	}
}

func TestRunnerSurgeIndependentDailyCap(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	s.SendMode = SendModeOff
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai)
	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{
			accts: []Account{
				{ID: "ba1", Name: "数商云", Balance: 1000, LastRecharge: recharge(1000)},
			},
			consumption: []Consumption{{AccountID: "ba1", Amount7D: 70, Amount3D: 30, AmountToday: 40}},
		},
		Now: func() time.Time { return now },
	}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 1 {
		t.Fatalf("surge should send: %+v", stats)
	}
	recs, err := models.BalanceAlertRecordGets(n9e, "ba1", "", "", 10)
	if err != nil || len(recs) != 1 || recs[0].TriggerType != models.TriggerTypeSurge {
		t.Fatalf("want SURGE record: %+v err=%v", recs, err)
	}
	stats, err = r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sent != 0 || stats.Cooldown != 1 {
		t.Fatalf("same-day surge cooldown: %+v", stats)
	}
}

func TestRunnerGloballyDisabledSkipsAll(t *testing.T) {
	n9e := testRunnerCtx(t)
	s := models.DefaultBalanceAlertSettings()
	off := false
	s.Enabled = &off
	if err := models.BalanceAlertSettingsPut(n9e, s, "root"); err != nil {
		t.Fatal(err)
	}
	httpClient := &fakeHTTP{}
	r := &Runner{
		Ctx: n9e,
		Store: fakeStore{accts: []Account{
			{ID: "ba1", Name: "数商云", Balance: 1, HasVoucher: true},
		}},
		HTTP:   httpClient,
		Sender: HTTPSender{HTTP: httpClient},
		Now:    func() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai) },
	}
	stats, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Disabled || stats.Prepaid != 0 || stats.Sent != 0 || len(httpClient.urls) != 0 {
		t.Fatalf("disabled should skip: %+v urls=%v", stats, httpClient.urls)
	}
	dyn, err := r.runDynamic(context.Background())
	if err != nil || !dyn.Disabled || dyn.Sent != 0 {
		t.Fatalf("dynamic disabled: %+v err=%v", dyn, err)
	}
	recs, err := models.BalanceAlertRecordGets(n9e, "", "", "", 20)
	if err != nil || len(recs) != 0 {
		t.Fatalf("no records when disabled: n=%d err=%v", len(recs), err)
	}
}
