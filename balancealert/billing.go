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

// billingQueryTimeout 是 alerting 读计费库的单次查询上限。
//
// alerting 对计费库只有只读权限，但慢查询或行锁同样能把调用方挂死：15 分钟轮次
// 会一直占着 runMu（后续轮次全部排队），手动刷新会挂住 HTTP 请求，漏报对账同理。
// 数据源的 Query 路径自带 60s 超时，但走 PGStore 的 Raw 查询绕过了那层，所以
// deadline 必须在这里加，而不是指望调用方传 Background。
const billingQueryTimeout = 60 * time.Second

// billingCtx 给跨库查询套统一超时；parent 为 nil 时回退 Background。
func billingCtx(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, billingQueryTimeout)
}

// PrepaidSelectSQL is FR-01 (v1.1) 的**候选集**：enterprise、active、未删除、
// 未被半开通隔离。授信活跃的剔除不在这里做——排除名单已物化在 alerting 自己的库，
// 跨库无法反连接，改由 applyCreditExcludes 用内存集合过滤（见 exclude.go）。
//
// 明确不回退 billing_account_credit_configs 的旧口径：配置状态恒为 ACTIVE，
// 会永久漏报事实已转预付费的企业；也不在此实时扫 balance_consumption_allocations（G4）。
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
  -- 必须与候选集（PrepaidSelectSQL）逐字一致：自助页说「你在预警池里」、圈选却
  -- 跳过这个账户，是最难排查的一类不一致。
  AND a.provisioning_quarantined_at IS NULL
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
	// ScanCreditExcludes 返回「账户 -> 排除主因」，只读计费库，供每日物化名单使用。
	ScanCreditExcludes(ctx context.Context, windowDays int) (map[string]string, error)
	// ScanCreditExcludeHits 返回「账户 -> 三条条件的命中情况」，只供运营页诊断。
	ScanCreditExcludeHits(ctx context.Context, windowDays int) (map[string]CreditExcludeHits, error)
	// ListAccountBriefs 按账户 ID 取身份与状态，只用于运营页给人看名字。
	ListAccountBriefs(ctx context.Context, ids []string) (map[string]AccountBrief, error)
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
	ctx, cancel := billingCtx(ctx)
	defer cancel()
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
	ctx, cancel := billingCtx(ctx)
	defer cancel()
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
	ctx, cancel := billingCtx(ctx)
	defer cancel()
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
	ctx, cancel := billingCtx(ctx)
	defer cancel()
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

// AccountBriefSelectSQL 按 ID 取账户身份，只服务运营页的名单明细。
//
// 刻意**不加**候选集过滤（type/status/deleted_at/隔离）：名单本身不按账户状态过滤，
// 所以名单里可能留着已暂停、已删除或半开通隔离的账户。把这些账户的真实状态显示出来，
// 才能解释「名单 400 家、本轮只排除 12 家」——若在这里过滤掉，页面就只剩下一份
// 与名单行数对不上的明细。
const AccountBriefSelectSQL = `
SELECT a.id,
       COALESCE(NULLIF(a.enterprise_name, ''), a.name, a.id) AS name,
       COALESCE(a.type, '') AS type,
       COALESCE(a.status, '') AS status,
       (a.deleted_at IS NOT NULL) AS deleted,
       (a.provisioning_quarantined_at IS NOT NULL) AS quarantined
FROM billing_accounts a
WHERE a.id IN (%s)
`

// AccountBrief 是运营页展示的账户身份，不含余额与任何金额。
type AccountBrief struct {
	ID          string
	Name        string
	Type        string
	Status      string
	Deleted     bool
	Quarantined bool
}

// accountBriefChunk 是单条 SQL 的 IN 参数上限。
//
// 与名单整份替换用的 CreateInBatches(500) 保持同一量级：PostgreSQL 的绑定参数上限
// 是 65535，500 既远低于上限，也让每条语句的执行计划足够简单。
const accountBriefChunk = 500

// ListAccountBriefs 批量取账户身份。返回 map 而不是切片：调用方按名单里的 ID 取，
// 查不到的账户（计费库已无此记录）自然缺席，由调用方决定怎么展示。
func (s *PGStore) ListAccountBriefs(ctx context.Context, ids []string) (map[string]AccountBrief, error) {
	out := make(map[string]AccountBrief, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ctx, cancel := billingCtx(ctx)
	defer cancel()

	for start := 0; start < len(ids); start += accountBriefChunk {
		end := start + accountBriefChunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		marks := make([]string, 0, len(batch))
		args := make([]interface{}, 0, len(batch))
		for _, id := range batch {
			marks = append(marks, "?")
			args = append(args, id)
		}

		rows, err := s.db.WithContext(ctx).
			Raw(fmt.Sprintf(AccountBriefSelectSQL, strings.Join(marks, ",")), args...).Rows()
		if err != nil {
			return nil, wrapBillingSelectErr(err)
		}
		for rows.Next() {
			var b AccountBrief
			if err := rows.Scan(&b.ID, &b.Name, &b.Type, &b.Status, &b.Deleted, &b.Quarantined); err != nil {
				rows.Close()
				return nil, err
			}
			if b.ID != "" {
				out[b.ID] = b
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LookupAccountBriefs 是名单明细的「补名字」一步，对页面是**尽力而为**的：
//
// 明细页的骨架来自 alerting 自己的库，计费库只用来补企业名与账户状态。计费库不可达
// 时应当仍然能看到「谁被排除、主因是什么」，因此这里把错误原样交给调用方去展示成
// 一句提示，而不是让整个接口失败。
func LookupAccountBriefs(n9e *ctx.Context, settings models.BalanceAlertSettings, ids []string) (map[string]AccountBrief, error) {
	store, err := OpenStore(n9e, settings)
	if err != nil {
		return nil, err
	}
	return store.ListAccountBriefs(context.Background(), ids)
}

// CreditExcludeHits 是某个账户对三条排除条件的命中情况，只用于运营诊断。
//
// 与物化表里的 reason 是两个层次：reason 是「按优先级选出的一个主因」，Hits 是
// 「三条各自是否成立」。一家企业可以同时命中三条，reason 只反映其中最高的那条。
type CreditExcludeHits struct {
	OpenDebt         bool
	RecentAdvance    bool
	RecentSettlement bool
}

// Any 表示至少命中一条；三条皆假说明该账户在当前数据下已不该被排除。
func (h CreditExcludeHits) Any() bool {
	return h.OpenDebt || h.RecentAdvance || h.RecentSettlement
}

// ScanCreditExcludeHits 按当前计费库数据重算三条条件的命中情况。
//
// 与刷新共用 excludeActivitySQL，所以口径不会漂。返回值只含命中至少一条的账户：
// 名单里查不到的账户即「当下已不该排除」，由调用方展示成空命中。
func (s *PGStore) ScanCreditExcludeHits(ctx context.Context, windowDays int) (map[string]CreditExcludeHits, error) {
	ctx, cancel := billingCtx(ctx)
	defer cancel()
	rows, err := s.db.WithContext(ctx).Raw(excludeHitsSQL, windowDays, windowDays).Rows()
	if err != nil {
		return nil, wrapBillingSelectErr(err)
	}
	defer rows.Close()

	out := make(map[string]CreditExcludeHits)
	for rows.Next() {
		var id string
		var h CreditExcludeHits
		if err := rows.Scan(&id, &h.OpenDebt, &h.RecentAdvance, &h.RecentSettlement); err != nil {
			return nil, err
		}
		if id != "" {
			out[id] = h
		}
	}
	return out, rows.Err()
}

// LookupCreditExcludeHits 与 LookupAccountBriefs 同样是「尽力而为」：命中标记只是
// 附加解释，拿不到时名单明细本身仍然要能打开。
func LookupCreditExcludeHits(n9e *ctx.Context, settings models.BalanceAlertSettings, windowDays int) (map[string]CreditExcludeHits, error) {
	store, err := OpenStore(n9e, settings)
	if err != nil {
		return nil, err
	}
	return store.ScanCreditExcludeHits(context.Background(), windowDays)
}

// ScanCreditExcludes 只读计费库：每日一次聚合，算出授信活跃账户与其主因。
// 本方法不写入计费库任何表，alerting 侧对该库不需要 INSERT/UPDATE/DELETE 权限。
func (s *PGStore) ScanCreditExcludes(ctx context.Context, windowDays int) (map[string]string, error) {
	ctx, cancel := billingCtx(ctx)
	defer cancel()
	rows, err := s.db.WithContext(ctx).Raw(excludeScanSQL, windowDays, windowDays).Rows()
	if err != nil {
		return nil, wrapBillingSelectErr(err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		out[id] = reason
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
