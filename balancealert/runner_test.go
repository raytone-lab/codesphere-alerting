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
	accts       []Account
	consumption []Consumption
	err         error
}

func (f fakeStore) ListPrepaid(ctx context.Context) ([]Account, error) {
	return f.accts, f.err
}

func (f fakeStore) ListConsumption(ctx context.Context) ([]Consumption, error) {
	return f.consumption, f.err
}

func (f fakeStore) ResolveAccountByUsername(ctx context.Context, username string) (*MyAccount, error) {
	return nil, nil
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
