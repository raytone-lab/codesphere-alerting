package balancealert

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/datasource/postgresql"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"gorm.io/gorm"
)

// PrepaidSelectSQL is FR-01: enterprise, active, not deleted, no current available credit.
// Available-credit predicate mirrors codesphere-billing enterprise-account.service
// activeCreditLimitUsd: ACTIVE + limit>0 + terms_days>0 + effective/expires window.
const PrepaidSelectSQL = `
SELECT
  p.id,
  COALESCE(NULLIF(p.enterprise_name, ''), p.name, p.id) AS name,
  COALESCE(w.balance_usd, 0) AS balance_usd,
  lr.amount_usd AS last_recharge_usd,
  (v.billing_account_id IS NOT NULL) AS has_voucher,
  COALESCE(u.phone, '') AS phone,
  p.created_at
FROM (
  SELECT a.id, a.name, a.enterprise_name, a.owner_user_id, a.created_at
  FROM billing_accounts a
  WHERE a.type = 'enterprise'
    AND a.status = 'active'
    AND a.deleted_at IS NULL
    AND a.provisioning_quarantined_at IS NULL
    AND NOT EXISTS (
      SELECT 1 FROM billing_account_credit_configs c
      WHERE c.billing_account_id = a.id
        AND c.status = 'ACTIVE'
        AND c.limit_usd > 0
        AND c.terms_days IS NOT NULL
        AND c.terms_days > 0
        AND (c.effective_at IS NULL OR c.effective_at <= now())
        AND (c.expires_at IS NULL OR c.expires_at > now())
    )
) p
LEFT JOIN account_wallets w ON w.billing_account_id = p.id
LEFT JOIN (
  SELECT DISTINCT ON (billing_account_id)
         billing_account_id, amount_usd
  FROM balance_transactions
  WHERE type = 'RECHARGE'
  ORDER BY billing_account_id, created_at DESC
) lr ON lr.billing_account_id = p.id
LEFT JOIN (
  SELECT DISTINCT billing_account_id
  FROM balance_transactions
  WHERE type = 'ADJUST' AND description LIKE 'voucher:%'
) v ON v.billing_account_id = p.id
LEFT JOIN users u ON u.id = p.owner_user_id AND u.deleted_at IS NULL
`

// ConsumptionSelectSQL fetches 7-day and 3-day consumption totals per account.
const ConsumptionSelectSQL = `
SELECT
  billing_account_id,
  COALESCE(SUM(CASE WHEN created_at >= now() - INTERVAL '7 days' THEN amount_usd ELSE 0 END), 0) AS amount_7d,
  COALESCE(SUM(CASE WHEN created_at >= now() - INTERVAL '3 days' THEN amount_usd ELSE 0 END), 0) AS amount_3d,
  COALESCE(SUM(CASE WHEN (created_at AT TIME ZONE 'Asia/Shanghai')::date = (now() AT TIME ZONE 'Asia/Shanghai')::date THEN amount_usd ELSE 0 END), 0) AS amount_today
FROM balance_transactions
WHERE type = 'CONSUME'
  AND created_at >= now() - INTERVAL '7 days'
GROUP BY billing_account_id
`

type Account struct {
	ID           string
	Name         string
	Balance      float64
	LastRecharge *float64
	HasVoucher   bool
	Phone        string
	CreatedAt    time.Time
}

// ResolveAccountByEmailOrPhoneSQL finds the enterprise billing account owned by
// the billing user whose email and/or phone match. Placeholders are GORM `?`
// (Postgres dialect rewrites them to $n). Email match is case-insensitive.
const ResolveAccountByEmailOrPhoneSQL = `
SELECT a.id,
       COALESCE(NULLIF(a.enterprise_name, ''), a.name, a.id) AS name,
       COALESCE(w.balance_usd, 0) AS balance_usd,
       COALESCE(u.phone, '') AS phone,
       lr.amount_usd AS last_recharge_usd,
       (v.billing_account_id IS NOT NULL) AS has_voucher
FROM users u
JOIN billing_accounts a ON a.owner_user_id = u.id
  AND a.type = 'enterprise' AND a.status = 'active' AND a.deleted_at IS NULL
LEFT JOIN account_wallets w ON w.billing_account_id = a.id
LEFT JOIN (
  SELECT DISTINCT ON (billing_account_id)
         billing_account_id, amount_usd
  FROM balance_transactions
  WHERE type = 'RECHARGE'
  ORDER BY billing_account_id, created_at DESC
) lr ON lr.billing_account_id = a.id
LEFT JOIN (
  SELECT DISTINCT billing_account_id
  FROM balance_transactions
  WHERE type = 'ADJUST' AND description LIKE 'voucher:%'
) v ON v.billing_account_id = a.id
WHERE u.deleted_at IS NULL
  AND (
    (? <> '' AND LOWER(TRIM(COALESCE(u.email, ''))) = ?)
    OR (? <> '' AND TRIM(COALESCE(u.phone, '')) = ?)
  )
LIMIT 1
`

type MyAccount struct {
	ID           string
	Name         string
	Balance      float64
	Phone        string
	LastRecharge *float64
	HasVoucher   bool
}

type BillingUser struct {
	ID       string
	Email    string
	Phone    string
	Nickname string
}

// BillingUserSelectSQL copies active billing users that have email or phone
// so Nightingale 人员组织 can log in and match 我的余额告警.
const BillingUserSelectSQL = `
SELECT
  u.id,
  COALESCE(u.email, '') AS email,
  COALESCE(u.phone, '') AS phone,
  COALESCE(u.nickname, '') AS nickname
FROM users u
WHERE u.deleted_at IS NULL
  AND COALESCE(u.is_active, TRUE) = TRUE
  AND (
    TRIM(COALESCE(u.email, '')) <> ''
    OR TRIM(COALESCE(u.phone, '')) <> ''
  )
`

type Store interface {
	ListPrepaid(ctx context.Context) ([]Account, error)
	ListConsumption(ctx context.Context) ([]Consumption, error)
	ResolveAccountByEmailOrPhone(ctx context.Context, email, phone string) (*MyAccount, error)
	ListBillingUsers(ctx context.Context) ([]BillingUser, error)
}

type PGStore struct {
	db *gorm.DB
}

func wrapBillingSelectErr(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(err.Error(), "42501") || strings.Contains(msg, "permission denied") {
		return fmt.Errorf("billing SELECT denied (GRANT SELECT on billing tables to the datasource user): %w", err)
	}
	return err
}

func (s *PGStore) ListPrepaid(ctx context.Context) ([]Account, error) {
	rows, err := s.db.WithContext(ctx).Raw(PrepaidSelectSQL).Rows()
	if err != nil {
		return nil, wrapBillingSelectErr(err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		var last sql.NullFloat64
		var created sql.NullTime
		if err := rows.Scan(&a.ID, &a.Name, &a.Balance, &last, &a.HasVoucher, &a.Phone, &created); err != nil {
			return nil, err
		}
		if last.Valid {
			v := last.Float64
			a.LastRecharge = &v
		}
		if created.Valid {
			a.CreatedAt = created.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PGStore) ListConsumption(ctx context.Context) ([]Consumption, error) {
	rows, err := s.db.WithContext(ctx).Raw(ConsumptionSelectSQL).Rows()
	if err != nil {
		return nil, wrapBillingSelectErr(err)
	}
	defer rows.Close()

	var out []Consumption
	for rows.Next() {
		var c Consumption
		if err := rows.Scan(&c.AccountID, &c.Amount7D, &c.Amount3D, &c.AmountToday); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PGStore) ResolveAccountByEmailOrPhone(ctx context.Context, email, phone string) (*MyAccount, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	phone = strings.TrimSpace(phone)
	if email == "" && phone == "" {
		return nil, nil
	}
	var a MyAccount
	var last sql.NullFloat64
	row := s.db.WithContext(ctx).Raw(ResolveAccountByEmailOrPhoneSQL, email, email, phone, phone).Row()
	err := row.Scan(&a.ID, &a.Name, &a.Balance, &a.Phone, &last, &a.HasVoucher)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapBillingSelectErr(err)
	}
	if a.ID == "" {
		return nil, nil
	}
	if last.Valid {
		v := last.Float64
		a.LastRecharge = &v
	}
	return &a, nil
}

func (s *PGStore) ListBillingUsers(ctx context.Context) ([]BillingUser, error) {
	rows, err := s.db.WithContext(ctx).Raw(BillingUserSelectSQL).Rows()
	if err != nil {
		return nil, wrapBillingSelectErr(err)
	}
	defer rows.Close()

	var out []BillingUser
	for rows.Next() {
		var u BillingUser
		if err := rows.Scan(&u.ID, &u.Email, &u.Phone, &u.Nickname); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func OpenStore(n9e *ctx.Context, s models.BalanceAlertSettings) (Store, error) {
	ds, err := resolveDatasource(n9e, s)
	if err != nil {
		return nil, err
	}
	_ = ds.Decrypt()
	if ds.PluginType != postgresql.PostgreSQLType {
		return nil, fmt.Errorf("datasource %d is %s, need pgsql", ds.Id, ds.PluginType)
	}
	plug := new(postgresql.PostgreSQL)
	inst, err := plug.Init(ds.SettingsJson)
	if err != nil {
		return nil, fmt.Errorf("init pgsql datasource: %w", err)
	}
	pg, ok := inst.(*postgresql.PostgreSQL)
	if !ok || len(pg.Shards) == 0 {
		return nil, fmt.Errorf("pgsql datasource %d has no shards", ds.Id)
	}
	shard := *pg.Shards[0]
	conn, err := shard.NewConn(context.Background(), strings.TrimSpace(shard.DB))
	if err != nil {
		return nil, fmt.Errorf("connect billing db via datasource %d: %w", ds.Id, err)
	}
	return &PGStore{db: conn}, nil
}

func resolveDatasource(n9e *ctx.Context, s models.BalanceAlertSettings) (*models.Datasource, error) {
	if s.DatasourceID > 0 {
		return models.DatasourceGet(n9e, s.DatasourceID)
	}
	lst, err := models.GetDatasourcesGetsBy(n9e, "pgsql", "", "", "")
	if err != nil {
		return nil, err
	}
	for _, ds := range lst {
		if ds.Name == "ruidong_admin" {
			return ds, ds.DB2FE()
		}
	}
	if len(lst) > 0 {
		return lst[0], lst[0].DB2FE()
	}
	return nil, fmt.Errorf("no pgsql datasource configured for balance alert")
}
