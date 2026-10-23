package balancealert

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 排除名单口径的**唯一**验收缝。
//
// 为什么必须是真库：excludeScanSQL 依赖 PostgreSQL 的 make_interval 与
// `?::int` 占位符，共存的单测跑在 glebarez/sqlite（CGO_ENABLED=0）上，连这条
// SQL 都执行不了。只断言「SQL 字符串里包含某段文字」等于在测实现，且会给人
// 「口径已被测试锁定」的假象——历史上 TC-01~TC-09/11/13 就是这么「覆盖」的。
//
// 用法：
//
//	$env:BALANCEALERT_PG_DSN = "host=127.0.0.1 port=5432 user=... dbname=ruidong_billing sslmode=disable"
//	go test ./balancealert/ -run TestExclude -v
//
// 未设置 DSN 时整批跳过并打印原因：本仓库没有 CI，跳过必须显眼，不能把
// 「没跑」当成「通过」。每次运行在同一个 DSN 内建一个临时 schema，结束后
// DROP SCHEMA ... CASCADE，不污染开发库。
const pgTestDSNEnv = "BALANCEALERT_PG_DSN"

func pgTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(pgTestDSNEnv))
	if dsn == "" {
		t.Skipf("跳过真库用例：未设置 %s；sqlite 单测无法执行 make_interval，不能替代 TC-01~TC-14 的口径断言", pgTestDSNEnv)
	}

	schema := fmt.Sprintf("ba_exclude_test_%d", time.Now().UnixNano())
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("connect %s: %v", pgTestDSNEnv, err)
	}
	// 注册顺序即执行顺序的逆序：先关会话、再删 schema、最后关 admin。
	t.Cleanup(func() {
		if sqlDB, err := admin.DB(); err == nil {
			sqlDB.Close()
		}
	})
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create temp schema: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop temp schema %s: %v", schema, err)
		}
	})

	// search_path 作为连接参数下发，而不是单独执行一次 SET：连接池里的每条连接
	// 都需要它，否则后续语句会随机落到 public schema。仅支持 key=value 形式的
	// DSN（URL 形式请先转换成 key=value）。
	db, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("connect temp schema: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	createBillingSchema(t, db)
	return db
}

// createBillingSchema 只建与本特性相关的列，够 excludeScanSQL / PrepaidSelectSQL
// 跑通即可。刻意不 import 计费侧迁移：这里测的是 alerting 对计费库的读口径。
func createBillingSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	stmts := []string{
		`CREATE TABLE billing_accounts (
			id varchar(128) PRIMARY KEY,
			name varchar(255) NOT NULL DEFAULT '',
			enterprise_name varchar(255) NOT NULL DEFAULT '',
			owner_user_id varchar(128),
			type varchar(32) NOT NULL,
			status varchar(32) NOT NULL,
			deleted_at timestamptz,
			provisioning_quarantined_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE balance_consumption_allocations (
			id bigserial PRIMARY KEY,
			billing_account_id varchar(128),
			approved_credit_usd numeric(20,12) NOT NULL DEFAULT 0,
			unapproved_debt_usd numeric(20,12) NOT NULL DEFAULT 0,
			settled_usd numeric(20,12),
			created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE enterprise_credit_settlements (
			id bigserial PRIMARY KEY,
			billing_account_id varchar(128) NOT NULL,
			debt_source varchar(64) NOT NULL,
			occurred_at timestamptz NOT NULL
		)`,
		`CREATE TABLE account_wallets (
			billing_account_id varchar(128) PRIMARY KEY,
			balance_usd numeric(20,12) NOT NULL DEFAULT 0
		)`,
		// 计费侧的信用配置表：本特性的口径刻意不读它（只看垫付/核销行为），
		// 建出来是为了让 TC-06 能构造「已开通信用账期但从未垫付」的真实形态。
		`CREATE TABLE billing_account_credit_configs (
			billing_account_id varchar(128) PRIMARY KEY,
			status varchar(32) NOT NULL,
			approved_credit_usd numeric(20,12) NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE balance_transactions (
			id bigserial PRIMARY KEY,
			billing_account_id varchar(128) NOT NULL,
			type varchar(32) NOT NULL,
			amount_usd numeric(20,12) NOT NULL DEFAULT 0,
			description text,
			created_at timestamptz NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE users (
			id varchar(128) PRIMARY KEY,
			email varchar(255),
			phone varchar(64),
			nickname varchar(255),
			deleted_at timestamptz,
			is_active boolean DEFAULT true
		)`,
	}
	for _, sql := range stmts {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatalf("create fixture table: %v", err)
		}
	}
}

func daysAgo(n int) time.Time {
	return time.Now().Add(-time.Duration(n) * 24 * time.Hour)
}

// advance 登记一条信用垫付（approved_credit_usd > 0）。
func advance(t *testing.T, db *gorm.DB, account string, approved, settled float64, age int) {
	t.Helper()
	if err := db.Exec(`INSERT INTO balance_consumption_allocations
		(billing_account_id, approved_credit_usd, unapproved_debt_usd, settled_usd, created_at)
		VALUES (?, ?, 0, ?, ?)`, account, approved, settled, daysAgo(age)).Error; err != nil {
		t.Fatalf("insert advance %s: %v", account, err)
	}
}

// balanceOnly 登记一条普通余额消费分摊：approved_credit_usd = 0。
func balanceOnly(t *testing.T, db *gorm.DB, account string, age int) {
	t.Helper()
	if err := db.Exec(`INSERT INTO balance_consumption_allocations
		(billing_account_id, approved_credit_usd, unapproved_debt_usd, settled_usd, created_at)
		VALUES (?, 0, 0, NULL, ?)`, account, daysAgo(age)).Error; err != nil {
		t.Fatalf("insert balance-only row %s: %v", account, err)
	}
}

// unapprovedOnly 登记一条只有未批准穿零欠款的分摊：本期不参与判定。
func unapprovedOnly(t *testing.T, db *gorm.DB, account string, age int) {
	t.Helper()
	if err := db.Exec(`INSERT INTO balance_consumption_allocations
		(billing_account_id, approved_credit_usd, unapproved_debt_usd, settled_usd, created_at)
		VALUES (?, 0, 12.5, NULL, ?)`, account, daysAgo(age)).Error; err != nil {
		t.Fatalf("insert unapproved row %s: %v", account, err)
	}
}

// creditConfig 登记一条「已开通信用账期」的配置，并把钱包预置为正数。
//
// 计费侧确实有这张表，但本特性刻意不读它：口径只看垫付/核销行为（见 billing.go
// 对旧配置口径的说明）。TC-06 保留这条数据，正好把「配置为 ACTIVE 但从未垫付
// 不构成排除」钉死——否则这条用例和 TC-05 只是名字不同。
func creditConfig(t *testing.T, db *gorm.DB, account string, balance float64) {
	t.Helper()
	if err := db.Exec(`INSERT INTO billing_account_credit_configs
		(billing_account_id, status, approved_credit_usd) VALUES (?, 'ACTIVE', 1000)`,
		account).Error; err != nil {
		t.Fatalf("insert credit config %s: %v", account, err)
	}
	if err := db.Exec(`INSERT INTO account_wallets (billing_account_id, balance_usd) VALUES (?, ?)`,
		account, balance).Error; err != nil {
		t.Fatalf("insert wallet %s: %v", account, err)
	}
}

func settlement(t *testing.T, db *gorm.DB, account, source string, age int) {
	t.Helper()
	if err := db.Exec(`INSERT INTO enterprise_credit_settlements
		(billing_account_id, debt_source, occurred_at) VALUES (?, ?, ?)`,
		account, source, daysAgo(age)).Error; err != nil {
		t.Fatalf("insert settlement %s: %v", account, err)
	}
}

func account(t *testing.T, db *gorm.DB, id, typ, status string, quarantined bool) {
	t.Helper()
	accountOwned(t, db, id, typ, status, quarantined, "")
}

func accountOwned(t *testing.T, db *gorm.DB, id, typ, status string, quarantined bool, ownerID string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO billing_accounts
		(id, name, enterprise_name, owner_user_id, type, status, provisioning_quarantined_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, id, id, ownerID, typ, status, quarantinedAt(quarantined)).Error; err != nil {
		t.Fatalf("insert account %s: %v", id, err)
	}
}

func user(t *testing.T, db *gorm.DB, id, email, phone string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO users (id, email, phone) VALUES (?, ?, ?)`,
		id, email, phone).Error; err != nil {
		t.Fatalf("insert user %s: %v", id, err)
	}
}

func quarantinedAt(q bool) interface{} {
	if q {
		return time.Now()
	}
	return nil
}

// scanExcludes 走与生产完全相同的路径：同一条 SQL、同样的 GORM 占位符改写。
func scanExcludes(t *testing.T, db *gorm.DB, windowDays int) map[string]string {
	t.Helper()
	rows, err := db.Raw(excludeScanSQL, windowDays, windowDays).Rows()
	if err != nil {
		t.Fatalf("run excludeScanSQL: %v", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		out[id] = reason
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	return out
}

// scanHits 与扫描走同一条 SQL 路径，只把外层的 CASE 换成 bool_or。
func scanHits(t *testing.T, db *gorm.DB, windowDays int) map[string]CreditExcludeHits {
	t.Helper()
	rows, err := db.Raw(excludeHitsSQL, windowDays, windowDays).Rows()
	if err != nil {
		t.Fatalf("run excludeHitsSQL: %v", err)
	}
	defer rows.Close()

	out := map[string]CreditExcludeHits{}
	for rows.Next() {
		var id string
		var h CreditExcludeHits
		if err := rows.Scan(&id, &h.OpenDebt, &h.RecentAdvance, &h.RecentSettlement); err != nil {
			t.Fatalf("scan hits row: %v", err)
		}
		out[id] = h
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate hits rows: %v", err)
	}
	return out
}

// TestExcludeScanAgainstPostgres 覆盖 TC-01~TC-09 与 TC-14 的判定口径。
func TestExcludeScanAgainstPostgres(t *testing.T) {
	db := pgTestDB(t)
	const window = 90

	// TC-01 授信运营企业：存在未清垫付债务。
	account(t, db, "tc01", "enterprise", "active", false)
	advance(t, db, "tc01", 100, 40, 200)

	// TC-02 授信企业对账代充：债务已全部核销，最近垫付在窗口内。
	account(t, db, "tc02", "enterprise", "active", false)
	advance(t, db, "tc02", 100, 100, 10)

	// TC-03 授信回归预付费：债务清零，最后垫付与最后核销都超窗。
	account(t, db, "tc03", "enterprise", "active", false)
	advance(t, db, "tc03", 50, 50, 120)
	settlement(t, db, "tc03", "ENTERPRISE_CREDIT", 100)

	// TC-04 回归过渡期：债务清零，最后垫付 45 天前。
	account(t, db, "tc04", "enterprise", "active", false)
	advance(t, db, "tc04", 30, 30, 45)

	// TC-05 真预付费企业：从无垫付记录。
	account(t, db, "tc05", "enterprise", "active", false)

	// TC-06 新授信未穿 0：已开通信用账期、钱包为正、从无垫付。
	// 结论必须与 TC-05 相同——「有信用配置」本身不是排除条件，否则会把所有
	// 刚开通授信、还没用过的企业一起排除。
	account(t, db, "tc06", "enterprise", "active", false)
	creditConfig(t, db, "tc06", 500)

	// TC-08 近窗纯余额消费：窗口内有普通消费分摊，但 approved_credit_usd = 0。
	account(t, db, "tc08", "enterprise", "active", false)
	balanceOnly(t, db, "tc08", 3)
	balanceOnly(t, db, "tc08", 40)

	// TC-09 非信用核销：穿 0 后充值产生 NEGATIVE_BALANCE，窗口内无信用垫付。
	account(t, db, "tc09", "enterprise", "active", false)
	settlement(t, db, "tc09", "NEGATIVE_BALANCE", 5)

	// TC-14 核销延长排除期：最后垫付已超窗，20 天前做了对账代充。
	// 这是「核销是独立第三类排除条件」的反例：只看垫付会把它判回预付费。
	account(t, db, "tc14", "enterprise", "active", false)
	advance(t, db, "tc14", 80, 80, 180)
	settlement(t, db, "tc14", "ENTERPRISE_CREDIT", 20)

	// 未批准穿零欠款本期不参与判定。
	account(t, db, "unapproved", "enterprise", "active", false)
	unapprovedOnly(t, db, "unapproved", 5)

	// 多条件命中只落主因：OPEN_DEBT > RECENT_ADVANCE > RECENT_SETTLEMENT。
	account(t, db, "precedence", "enterprise", "active", false)
	advance(t, db, "precedence", 100, 10, 5)
	settlement(t, db, "precedence", "ENTERPRISE_CREDIT", 5)

	got := scanExcludes(t, db, window)

	want := map[string]string{
		"tc01":       models.ExcludeReasonOpenDebt,
		"tc02":       models.ExcludeReasonRecentAdvance,
		"tc04":       models.ExcludeReasonRecentAdvance,
		"tc14":       models.ExcludeReasonRecentSettlement,
		"precedence": models.ExcludeReasonOpenDebt,
	}
	for id, reason := range want {
		if got[id] != reason {
			t.Errorf("%s: reason=%q, want %q (list=%v)", id, got[id], reason, got)
		}
	}
	for _, id := range []string{"tc03", "tc05", "tc06", "tc08", "tc09", "unapproved"} {
		if reason, ok := got[id]; ok {
			t.Errorf("%s must not be excluded, got reason=%q", id, reason)
		}
	}
}

// TestExcludeHitsAgreeWithScan 锁定「明细页的逐条命中」与「刷新的主因」同源。
//
// 两者共用 excludeActivitySQL，但外层查询不同：一个有 CASE，一个有 bool_or。
// 这条用例是唯一能证明它们没有漂的地方——顺带也证明了下列容易读错的事实：
// 一家企业可以同时命中多条，reason 只反映最高的一条，所以
// 「核销计数为 0」绝不代表核销条件没被命中。
func TestExcludeHitsAgreeWithScan(t *testing.T) {
	db := pgTestDB(t)
	const window = 90

	// 三条全中：既有未清债务、窗口内又有新垫付、窗口内还有核销。
	account(t, db, "all3", "enterprise", "active", false)
	advance(t, db, "all3", 100, 10, 5)
	settlement(t, db, "all3", "ENTERPRISE_CREDIT", 5)

	// 只命中核销：最后垫付超窗，但 20 天前对账代充过（TC-14 形态）。
	account(t, db, "only_settle", "enterprise", "active", false)
	advance(t, db, "only_settle", 80, 80, 180)
	settlement(t, db, "only_settle", "ENTERPRISE_CREDIT", 20)

	// 只命中未清债务：垫付在窗口外、从未核销。
	account(t, db, "only_debt", "enterprise", "active", false)
	advance(t, db, "only_debt", 100, 40, 200)

	// 什么都不命中：只有非信用核销。
	account(t, db, "none", "enterprise", "active", false)
	settlement(t, db, "none", "NEGATIVE_BALANCE", 5)

	scan := scanExcludes(t, db, window)
	hits := scanHits(t, db, window)

	// 1) 两边覆盖的账户集合必须完全一致（都是「至少命中一条」的账户）。
	for id := range scan {
		if _, ok := hits[id]; !ok {
			t.Errorf("%s is excluded but has no hits row", id)
		}
	}
	for id := range hits {
		if _, ok := scan[id]; !ok {
			t.Errorf("%s has hits but is not excluded", id)
		}
	}

	wantHits := map[string]CreditExcludeHits{
		"all3":        {OpenDebt: true, RecentAdvance: true, RecentSettlement: true},
		"only_settle": {RecentSettlement: true},
		"only_debt":   {OpenDebt: true},
	}
	for id, want := range wantHits {
		if hits[id] != want {
			t.Errorf("%s: hits=%+v, want %+v", id, hits[id], want)
		}
	}
	if _, ok := hits["none"]; ok {
		t.Errorf("non-credit settlement must not count as a hit: %+v", hits["none"])
	}

	// 2) reason 必须是「命中集合里优先级最高的那一条」，而不是随便取一个。
	if reason := scan["all3"]; reason != models.ExcludeReasonOpenDebt {
		t.Errorf("all3 reason=%q, want OPEN_DEBT (highest of three hits)", reason)
	}
	if reason := scan["only_settle"]; reason != models.ExcludeReasonRecentSettlement {
		t.Errorf("only_settle reason=%q, want RECENT_SETTLEMENT", reason)
	}

	// 3) all3 同时命中了核销，但 reason 不是它：这条断言记录的就是那个
	//    「reason 分布看不出核销条件是否生效」的坑，防止有人据此删掉第三段。
	if !hits["all3"].RecentSettlement {
		t.Error("all3 must hit RECENT_SETTLEMENT even though reason says OPEN_DEBT")
	}
}

// TestExcludeScanWindowIsRolling 锁定「滚动 N×24 小时」：同一条垫付在 89 天和
// 91 天两种窗口下必须给出不同结论，否则窗口参数就是装饰品。
func TestExcludeScanWindowIsRolling(t *testing.T) {
	db := pgTestDB(t)
	account(t, db, "edge", "enterprise", "active", false)
	advance(t, db, "edge", 20, 20, 89)

	if got := scanExcludes(t, db, 90); got["edge"] != models.ExcludeReasonRecentAdvance {
		t.Fatalf("89-day-old advance must be inside a 90-day window, got %v", got)
	}
	if got := scanExcludes(t, db, 30); len(got) != 0 {
		t.Fatalf("89-day-old advance must be outside a 30-day window, got %v", got)
	}
}

// TestExcludeScanIsReadOnly 是 TC-13 的可执行版本：把整个扫描放进只读事务，
// 只要 SQL 里混进任何写操作，Postgres 会直接拒绝——比断言字符串里没有
// "INSERT " 可靠得多。
func TestExcludeScanIsReadOnly(t *testing.T) {
	db := pgTestDB(t)
	account(t, db, "ro", "enterprise", "active", false)
	advance(t, db, "ro", 10, 0, 1)

	tx := db.Begin()
	defer tx.Rollback()
	if err := tx.Exec("SET TRANSACTION READ ONLY").Error; err != nil {
		t.Fatalf("set read only: %v", err)
	}
	rows, err := tx.Raw(excludeScanSQL, 90, 90).Rows()
	if err != nil {
		t.Fatalf("scan must be read-only: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected the read-only scan to return the excluded account")
	}
	var id, reason string
	if err := rows.Scan(&id, &reason); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if id != "ro" || reason != models.ExcludeReasonOpenDebt {
		t.Fatalf("got id=%q reason=%q", id, reason)
	}
}

// TestExcludeCandidateSetExcludesPersonalAndQuarantined 覆盖 TC-07 与候选集口径：
// 只有企业、active、未删除、未隔离的账户才是预警候选。
//
// 自助页定位账户用的是另一条 SQL（ResolveAccountByEmailOrPhoneSQL），两条必须
// 一致，否则会出现「页面说你在池里、圈选却跳过你」。因此不比对 SQL 文本，而是
// 直接跑两条 SQL 断言同一个被隔离账户在两处都不可见。
func TestExcludeCandidateSetExcludesPersonalAndQuarantined(t *testing.T) {
	db := pgTestDB(t)
	user(t, db, "u-quarantined", "q@example.com", "")
	user(t, db, "u-ok", "ok@example.com", "")
	user(t, db, "u-personal", "p@example.com", "")

	accountOwned(t, db, "personal", "personal", "active", false, "u-personal")
	accountOwned(t, db, "quarantined", "enterprise", "active", true, "u-quarantined")
	accountOwned(t, db, "suspended", "enterprise", "suspended", false, "")
	accountOwned(t, db, "ok", "enterprise", "active", false, "u-ok")

	rows, err := db.Raw(PrepaidSelectSQL).Rows()
	if err != nil {
		t.Fatalf("run PrepaidSelectSQL: %v", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	for rows.Next() {
		var (
			id, name string
			balance  float64
			last     *float64
			voucher  bool
			phone    string
			created  time.Time
		)
		if err := rows.Scan(&id, &name, &balance, &last, &voucher, &phone, &created); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(seen) != 1 || !seen["ok"] {
		t.Fatalf("candidate set must contain only the active enterprise, got %v", seen)
	}

	// 同一个被隔离账户在自助页定位里也必须查不到。
	if id, ok := resolveAccountID(t, db, "q@example.com"); ok {
		t.Fatalf("quarantined account must not be resolvable from self-service, got %q", id)
	}
	if id, ok := resolveAccountID(t, db, "ok@example.com"); !ok || id != "ok" {
		t.Fatalf("active account should resolve, got %q ok=%v", id, ok)
	}
}

// resolveAccountID 执行生产用的自助页定位 SQL；第二个返回值表示是否命中。
func resolveAccountID(t *testing.T, db *gorm.DB, email string) (string, bool) {
	t.Helper()
	var row struct {
		ID   string
		Name string
	}
	res := db.Raw(ResolveAccountByEmailOrPhoneSQL, email, email, "", "").Scan(&row)
	if res.Error != nil {
		t.Fatalf("run ResolveAccountByEmailOrPhoneSQL: %v", res.Error)
	}
	return row.ID, res.RowsAffected > 0
}

// TestExcludeReplaceIsAtomic 覆盖 TC-11：整份替换在单事务内完成。
//
// 不用重写的 DELETE/INSERT 做替身，而是直接调用生产函数：让第二段写入违反
// reason varchar(32)，如果两段不在同一事务，DELETE 已经提交、旧名单当场清空
// （那一刻全部授信企业都会被当预付费）。断言「失败后旧名单原样存在」即可把
// 拆分实现的回归钉死；成功路径再确认整份换掉。
func TestExcludeReplaceIsAtomic(t *testing.T) {
	db := pgTestDB(t)
	c := &ctx.Context{DB: db, IsCenter: true}
	if err := db.AutoMigrate(&models.BalanceAlertCreditExclude{}); err != nil {
		t.Fatalf("migrate list table: %v", err)
	}
	now := time.Now()
	seed := []models.BalanceAlertCreditExclude{
		{BillingAccountID: "old-a", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: now},
		{BillingAccountID: "old-b", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: now},
	}
	if err := models.BalanceAlertCreditExcludeReplace(c, seed); err != nil {
		t.Fatalf("seed old list: %v", err)
	}

	bad := []models.BalanceAlertCreditExclude{
		{BillingAccountID: "new-a", Reason: models.ExcludeReasonRecentAdvance, RefreshedAt: now},
		{BillingAccountID: "new-b", Reason: strings.Repeat("X", 64), RefreshedAt: now},
	}
	if err := models.BalanceAlertCreditExcludeReplace(c, bad); err == nil {
		t.Fatal("expected the oversized reason to fail the whole replace")
	}
	ids, err := models.BalanceAlertCreditExcludeIDs(c)
	if err != nil {
		t.Fatalf("read list after failed replace: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("failed replace must leave the old list intact, got %v", ids)
	}
	if _, ok := ids["old-a"]; !ok {
		t.Fatalf("failed replace must leave the old list intact, got %v", ids)
	}

	if err := models.BalanceAlertCreditExcludeReplace(c, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "new-a", Reason: models.ExcludeReasonRecentSettlement, RefreshedAt: now},
	}); err != nil {
		t.Fatalf("successful replace: %v", err)
	}
	ids, err = models.BalanceAlertCreditExcludeIDs(c)
	if err != nil {
		t.Fatalf("read list after replace: %v", err)
	}
	if _, ok := ids["new-a"]; !ok || len(ids) != 1 {
		t.Fatalf("replace must swap the whole list, got %v", ids)
	}
}
