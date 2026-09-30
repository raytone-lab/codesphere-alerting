package models_test

import (
	"testing"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestUserAddSetsLastActiveTime(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c := &ctx.Context{DB: db, IsCenter: true}

	u := &models.User{
		Username: "13800001003",
		Nickname: "13800001003",
		Phone:    "13800001003",
		Password: "x",
		Roles:    "Standard",
	}
	if err := u.Add(c); err != nil {
		t.Fatal(err)
	}
	got, err := models.UserGetByUsername(c, "13800001003")
	if err != nil || got == nil {
		t.Fatalf("get: %v %#v", err, got)
	}
	if got.LastActiveTime == 0 {
		t.Fatal("last_active_time still 0; UI formats that as 1970-01-01")
	}
	if got.LastActiveTime != got.CreateAt {
		t.Fatalf("last_active_time=%d create_at=%d", got.LastActiveTime, got.CreateAt)
	}
}
