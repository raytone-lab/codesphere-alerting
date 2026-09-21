package balancealert

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ccfos/nightingale/v6/datasource/postgresql"
	"github.com/ccfos/nightingale/v6/dskit/postgres"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"gorm.io/gorm"
)

// PrepaidSelectSQL is FR-01: enterprise, active, not deleted, no current available credit.
const PrepaidSelectSQL = `
SELECT
  p.id,
  COALESCE(NULLIF(p.enterprise_name, ''), p.name, p.id) AS name,
  COALESCE(w.balance_usd, 0) AS balance_usd,
  lr.amount_usd AS last_recharge_usd,
  (v.billing_account_id IS NOT NULL) AS has_voucher,
  COALESCE(u.phone, '') AS phone
FROM (
  SELECT a.id, a.name, a.enterprise_name, a.owner_user_id
  FROM billing_accounts a
  WHERE a.type = 'enterprise'
    AND a.status = 'active'
    AND a.deleted_at IS NULL
    AND NOT EXISTS (
      SELECT 1 FROM billing_account_credit_configs c
      WHERE c.billing_account_id = a.id
        AND c.status = 'ACTIVE'
        AND c.limit_usd > 0
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
LEFT JOIN users u ON u.id = p.owner_user_id
`

type Account struct {
	ID           string
	Name         string
	Balance      float64
	LastRecharge *float64
	HasVoucher   bool
	Phone        string
}

type Store interface {
	ListPrepaid(ctx context.Context) ([]Account, error)
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
		return fmt.Errorf("billing SELECT denied (GRANT SELECT on billing tables to the datasource user, or set billing_user/billing_password to table owner ruidong_billing): %w", err)
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
		if err := rows.Scan(&a.ID, &a.Name, &a.Balance, &last, &a.HasVoucher, &a.Phone); err != nil {
			return nil, err
		}
		if last.Valid {
			v := last.Float64
			a.LastRecharge = &v
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func OpenStore(n9e *ctx.Context, s models.BalanceAlertSettings) (Store, error) {
	ds, err := resolveDatasource(n9e, s)
	if err != nil {
		return nil, err
	}
	_ = ds.Decrypt()
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
	if strings.TrimSpace(s.BillingUser) != "" {
		shard.User = strings.TrimSpace(s.BillingUser)
	}
	if s.BillingPassword != "" {
		shard.Password = s.BillingPassword
	}
	dbName := s.BillingDatabase
	if dbName == "" {
		_, dbName = postgres.SplitHostDatabase(shard.Addr)
	}
	if dbName == "" {
		dbName = "ruidong_billing"
	}
	conn, err := shard.NewConn(context.Background(), dbName)
	if err != nil {
		return nil, fmt.Errorf("connect billing db %s: %w", dbName, err)
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
