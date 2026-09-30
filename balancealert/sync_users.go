package balancealert

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/ormx"
)

const (
	SyncedUserBelong          = "billing"
	SyncedUserRole            = "Standard"
	SyncedUserDefaultPassword = "root1234"
)

type SyncUsersResult struct {
	Created          int      `json:"created"`
	Updated          int      `json:"updated"`
	SkippedAdmin     int      `json:"skipped_admin"`
	SkippedNoContact int      `json:"skipped_no_contact"`
	InitialPassword  string   `json:"initial_password,omitempty"`
	Errors           []string `json:"errors,omitempty"`
}

func usernameFromBilling(email, phone string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	phone = strings.TrimSpace(phone)
	if phone != "" {
		return phone
	}
	return email
}

func SyncBillingUsers(std context.Context, n9e *ctx.Context, store Store, actor string) (SyncUsersResult, error) {
	var res SyncUsersResult
	if store == nil {
		return res, fmt.Errorf("billing store is nil")
	}
	src, err := store.ListBillingUsers(std)
	if err != nil {
		return res, err
	}
	if actor == "" {
		actor = "billing-sync"
	}

	for _, bu := range src {
		email := strings.ToLower(strings.TrimSpace(bu.Email))
		phone := strings.TrimSpace(bu.Phone)
		nickname := strings.TrimSpace(bu.Nickname)
		username := usernameFromBilling(email, phone)
		if username == "" {
			res.SkippedNoContact++
			continue
		}

		existing, err := findBillingSyncUser(n9e, username, email, phone)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", username, err))
			continue
		}
		if existing != nil {
			existing.RolesLst = strings.Fields(existing.Roles)
			if existing.IsAdmin() {
				res.SkippedAdmin++
				continue
			}
			if err := updateFromBilling(n9e, existing, username, email, phone, nickname, actor); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", username, err))
				continue
			}
			res.Updated++
			continue
		}

		if err := createFromBilling(n9e, username, email, phone, nickname, actor); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", username, err))
			continue
		}
		res.Created++
	}

	if res.Created > 0 {
		res.InitialPassword = SyncedUserDefaultPassword
	}
	return res, nil
}

func findBillingSyncUser(n9e *ctx.Context, username, email, phone string) (*models.User, error) {
	u, err := models.UserGetByUsername(n9e, username)
	if err != nil || u != nil {
		return u, err
	}
	if email != "" {
		u, err = models.UserGet(n9e, "LOWER(email) = ?", email)
		if err != nil || u != nil {
			return u, err
		}
	}
	if phone != "" {
		return models.UserGet(n9e, "phone = ?", phone)
	}
	return nil, nil
}

func updateFromBilling(n9e *ctx.Context, u *models.User, username, email, phone, nickname, actor string) error {
	fields := []interface{}{"nickname", "email", "phone", "belong", "update_by", "update_at"}
	if nickname != "" {
		u.Nickname = nickname
	}
	if email != "" {
		u.Email = email
	}
	if phone != "" {
		u.Phone = phone
	}
	if username != "" && username != u.Username {
		taken, err := models.UserGetByUsername(n9e, username)
		if err != nil {
			return err
		}
		if taken == nil || taken.Id == u.Id {
			u.Username = username
			fields = append(fields, "username")
		}
	}
	u.Belong = SyncedUserBelong
	u.UpdateBy = actor
	u.UpdateAt = time.Now().Unix()
	return u.Update(n9e, fields[0], fields[1:]...)
}

func createFromBilling(n9e *ctx.Context, username, email, phone, nickname, actor string) error {
	pass, err := models.CryptoPass(n9e, SyncedUserDefaultPassword)
	if err != nil {
		return err
	}
	if nickname == "" {
		nickname = username
	}
	u := &models.User{
		Username: username,
		Password: pass,
		Nickname: nickname,
		Phone:    phone,
		Email:    email,
		Roles:    SyncedUserRole,
		Contacts: ormx.JSONObj("{}"),
		Belong:   SyncedUserBelong,
		CreateBy: actor,
		UpdateBy: actor,
	}
	return u.Add(n9e)
}
