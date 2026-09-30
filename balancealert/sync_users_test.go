package balancealert

import (
	"context"
	"testing"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testSyncCtx(t *testing.T) *ctx.Context {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Configs{}, &models.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	n9e := &ctx.Context{DB: db, IsCenter: true}
	if err := models.ConfigsSet(n9e, models.SALT, "testsalt"); err != nil {
		t.Fatalf("salt: %v", err)
	}
	return n9e
}

func TestUsernameFromBilling(t *testing.T) {
	cases := []struct {
		email, phone, want string
	}{
		{"enterprise-v2-demo@example.com", "", "enterprise-v2-demo@example.com"},
		{"", " 13800001003 ", "13800001003"},
		{"  Demo@Example.com ", "13800001003", "13800001003"},
		{"test_alert_001@codesphere.test", "13800000001", "13800000001"},
		{"", "", ""},
	}
	for _, c := range cases {
		got := usernameFromBilling(c.email, c.phone)
		if got != c.want {
			t.Errorf("usernameFromBilling(%q,%q)=%q want %q", c.email, c.phone, got, c.want)
		}
	}
}

func TestSyncBillingUsersCreatesThenIsIdempotent(t *testing.T) {
	n9e := testSyncCtx(t)
	store := fakeStore{billingUsers: []BillingUser{{
		ID:       "user_v2_demo_001",
		Email:    "enterprise-v2-demo@example.com",
		Nickname: "V2账单演示企业主",
	}}}

	res, err := SyncBillingUsers(context.Background(), n9e, store, "root")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 || res.Updated != 0 {
		t.Fatalf("first sync: %+v", res)
	}
	if res.InitialPassword != SyncedUserDefaultPassword {
		t.Fatalf("password: %q", res.InitialPassword)
	}
	if SyncedUserDefaultPassword != "root1234" {
		t.Fatalf("default password must be root1234, got %q", SyncedUserDefaultPassword)
	}

	u, err := models.UserGetByUsername(n9e, "enterprise-v2-demo@example.com")
	if err != nil || u == nil {
		t.Fatalf("user: %v %#v", err, u)
	}
	if u.Email != "enterprise-v2-demo@example.com" || u.Nickname != "V2账单演示企业主" {
		t.Fatalf("profile: %+v", u)
	}
	if u.Roles != SyncedUserRole || u.Belong != SyncedUserBelong {
		t.Fatalf("role/belong: %q %q", u.Roles, u.Belong)
	}

	again, err := SyncBillingUsers(context.Background(), n9e, store, "root")
	if err != nil {
		t.Fatal(err)
	}
	if again.Created != 0 || again.Updated != 1 {
		t.Fatalf("second sync should update not create: %+v", again)
	}
	lst, err := models.UserGetAll(n9e)
	if err != nil {
		t.Fatal(err)
	}
	if len(lst) != 1 {
		t.Fatalf("incremental must not duplicate, n=%d", len(lst))
	}
}

func TestSyncBillingUsersUpdatesExistingAndSkipsAdmin(t *testing.T) {
	n9e := testSyncCtx(t)
	admin := &models.User{
		Username: "root",
		Nickname: "管理员",
		Phone:    "16601157293",
		Email:    "lmzhucd@isoftstone.com",
		Password: "x",
		Roles:    models.AdminRole,
	}
	if err := admin.Add(n9e); err != nil {
		t.Fatal(err)
	}
	std := &models.User{
		Username: "13800000001",
		Nickname: "13800000001",
		Phone:    "13800000001",
		Password: "x",
		Roles:    SyncedUserRole,
	}
	if err := std.Add(n9e); err != nil {
		t.Fatal(err)
	}

	store := fakeStore{billingUsers: []BillingUser{
		{ID: "stone", Phone: "16601157293", Nickname: "管理员"},
		{ID: "user_test_alert_001", Email: "test_alert_001@codesphere.test", Phone: "13800000001", Nickname: "测试告警企业主一"},
		{ID: "empty"},
	}}
	res, err := SyncBillingUsers(context.Background(), n9e, store, "root")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 || res.Updated != 1 || res.SkippedAdmin != 1 || res.SkippedNoContact != 1 {
		t.Fatalf("stats: %+v", res)
	}

	got, err := models.UserGetByUsername(n9e, "13800000001")
	if err != nil || got == nil {
		t.Fatalf("user: %v", err)
	}
	if got.Email != "test_alert_001@codesphere.test" || got.Nickname != "测试告警企业主一" {
		t.Fatalf("updated: %+v", got)
	}

	root, err := models.UserGetByUsername(n9e, "root")
	if err != nil || root == nil {
		t.Fatal(err)
	}
	if root.Email != "lmzhucd@isoftstone.com" || root.Roles != models.AdminRole {
		t.Fatalf("admin mutated: %+v", root)
	}
}

func TestSyncBillingUsersPrefersPhoneUsername(t *testing.T) {
	n9e := testSyncCtx(t)
	store := fakeStore{billingUsers: []BillingUser{{
		ID:       "user_test_alert_001",
		Email:    "test_alert_001@codesphere.test",
		Phone:    "13800000001",
		Nickname: "测试告警企业主一",
	}}}

	res, err := SyncBillingUsers(context.Background(), n9e, store, "root")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 1 {
		t.Fatalf("create: %+v", res)
	}

	byPhone, err := models.UserGetByUsername(n9e, "13800000001")
	if err != nil || byPhone == nil {
		t.Fatalf("phone username: %v %#v", err, byPhone)
	}
	if byPhone.Email != "test_alert_001@codesphere.test" || byPhone.Phone != "13800000001" {
		t.Fatalf("profile: %+v", byPhone)
	}
	byEmail, err := models.UserGetByUsername(n9e, "test_alert_001@codesphere.test")
	if err != nil {
		t.Fatal(err)
	}
	if byEmail != nil {
		t.Fatalf("must not create email username, got %+v", byEmail)
	}
}

func TestSyncBillingUsersRenamesEmailUsernameToPhone(t *testing.T) {
	n9e := testSyncCtx(t)
	old := &models.User{
		Username: "test_alert_001@codesphere.test",
		Nickname: "old",
		Email:    "test_alert_001@codesphere.test",
		Password: "x",
		Roles:    SyncedUserRole,
		Belong:   SyncedUserBelong,
	}
	if err := old.Add(n9e); err != nil {
		t.Fatal(err)
	}

	store := fakeStore{billingUsers: []BillingUser{{
		ID:       "user_test_alert_001",
		Email:    "test_alert_001@codesphere.test",
		Phone:    "13800000001",
		Nickname: "测试告警企业主一",
	}}}
	res, err := SyncBillingUsers(context.Background(), n9e, store, "root")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 || res.Updated != 1 {
		t.Fatalf("rename in place: %+v", res)
	}

	got, err := models.UserGetByUsername(n9e, "13800000001")
	if err != nil || got == nil {
		t.Fatalf("renamed: %v %#v", err, got)
	}
	if got.Email != "test_alert_001@codesphere.test" || got.Nickname != "测试告警企业主一" {
		t.Fatalf("profile: %+v", got)
	}
	stale, err := models.UserGetByUsername(n9e, "test_alert_001@codesphere.test")
	if err != nil {
		t.Fatal(err)
	}
	if stale != nil {
		t.Fatalf("old email username still present: %+v", stale)
	}
}
