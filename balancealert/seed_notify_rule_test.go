package balancealert

import (
	"testing"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testSeedNotifyCtx(t *testing.T) *ctx.Context {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.NotifyChannelConfig{}, &models.MessageTemplate{}, &models.NotifyRule{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &ctx.Context{DB: db, IsCenter: true}
}

func insertChannel(t *testing.T, n9e *ctx.Context, name, ident string) *models.NotifyChannelConfig {
	t.Helper()
	ch := &models.NotifyChannelConfig{Name: name, Ident: ident, Enable: true}
	if err := models.Insert(n9e, ch); err != nil {
		t.Fatalf("insert channel %s: %v", ident, err)
	}
	got, err := models.NotifyChannelGet(n9e, "ident = ?", ident)
	if err != nil || got == nil {
		t.Fatalf("reload channel %s: %v %#v", ident, err, got)
	}
	return got
}

func TestSeedNotifyRulesResolvesByIdentNotSerialID(t *testing.T) {
	n9e := testSeedNotifyCtx(t)
	insertChannel(t, n9e, "dummy-a", "dummy-a")
	insertChannel(t, n9e, "dummy-b", "dummy-b")
	ding := insertChannel(t, n9e, "Dingtalk", models.Dingtalk)
	sms := insertChannel(t, n9e, aliyunSMSChannelName, aliyunSMSChannelIdent)
	if ding.ID == 1 || sms.ID == 2 {
		t.Fatalf("setup did not shift serial ids: dingtalk=%d ali-sms=%d", ding.ID, sms.ID)
	}

	SeedMessageTemplates(n9e)
	SeedNotifyRules(n9e)

	pilot, err := models.NotifyRuleGet(n9e, "name = ?", seedPilotNotifyRuleName)
	if err != nil || pilot == nil {
		t.Fatalf("pilot rule: %v %#v", err, pilot)
	}
	customer, err := models.NotifyRuleGet(n9e, "name = ?", seedCustomerNotifyRuleName)
	if err != nil || customer == nil {
		t.Fatalf("customer rule: %v %#v", err, customer)
	}
	if len(pilot.NotifyConfigs) != 1 || pilot.NotifyConfigs[0].ChannelID != ding.ID {
		t.Fatalf("pilot channel_id=%v want %d", pilot.NotifyConfigs, ding.ID)
	}
	if len(customer.NotifyConfigs) != 1 || customer.NotifyConfigs[0].ChannelID != sms.ID {
		t.Fatalf("customer channel_id=%v want %d", customer.NotifyConfigs, sms.ID)
	}
	if pilot.NotifyConfigs[0].TemplateID == 0 || customer.NotifyConfigs[0].TemplateID == 0 {
		t.Fatalf("template ids not resolved: pilot=%d customer=%d", pilot.NotifyConfigs[0].TemplateID, customer.NotifyConfigs[0].TemplateID)
	}
}

func TestSeedNotifyRulesIsIdempotentAndSkipsExistingChannelUse(t *testing.T) {
	n9e := testSeedNotifyCtx(t)
	ding := insertChannel(t, n9e, "Dingtalk", models.Dingtalk)
	insertChannel(t, n9e, aliyunSMSChannelName, aliyunSMSChannelIdent)
	SeedMessageTemplates(n9e)
	if err := models.Insert(n9e, &models.NotifyRule{
		Name:   "Dingtalk",
		Enable: true,
		NotifyConfigs: []models.NotifyConfig{{
			ChannelID:  ding.ID,
			TemplateID: 1,
			Params:     map[string]interface{}{},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	SeedNotifyRules(n9e)
	SeedNotifyRules(n9e)

	lst, err := models.NotifyRulesGet(n9e, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, r := range lst {
		names[r.Name]++
	}
	if names["Dingtalk"] != 1 {
		t.Fatalf("existing dingtalk rule: %+v", names)
	}
	if names[seedPilotNotifyRuleName] != 0 {
		t.Fatalf("should not duplicate dingtalk path: %+v", names)
	}
	if names[seedCustomerNotifyRuleName] != 1 {
		t.Fatalf("customer sms rule: %+v", names)
	}
}

func TestSeedNotifyRulesCreatesAliyunChannelOnEmptyDB(t *testing.T) {
	n9e := testSeedNotifyCtx(t)
	SeedNotifyRules(n9e)
	ch, err := models.NotifyChannelGet(n9e, "ident = ?", aliyunSMSChannelIdent)
	if err != nil || ch == nil {
		t.Fatalf("ali-sms channel: %v %#v", err, ch)
	}
	if ch.Name != aliyunSMSChannelName {
		t.Fatalf("name: %q", ch.Name)
	}
	SeedNotifyRules(n9e)
	again, err := models.NotifyChannelGet(n9e, "ident = ?", aliyunSMSChannelIdent)
	if err != nil || again == nil || again.ID != ch.ID {
		t.Fatalf("channel upserted unexpectedly: first=%d again=%v err=%v", ch.ID, again, err)
	}
}
