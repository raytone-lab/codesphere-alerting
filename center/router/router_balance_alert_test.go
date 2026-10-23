package router

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testBalanceAlertRouterCtx(t *testing.T) *ctx.Context {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.Configs{}, &models.BalanceAlertCreditExclude{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &ctx.Context{DB: db, IsCenter: true}
}

// decodeData 调一个只读 handler 并解出 dat 字段。
func decodeData(t *testing.T, h gin.HandlerFunc) map[string]interface{} {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	h(c)

	var body struct {
		Dat map[string]interface{} `json:"dat"`
		Err string                 `json:"err"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	if body.Err != "" {
		t.Fatalf("unexpected err: %s", body.Err)
	}
	return body.Dat
}

// TestBalanceAlertExcludeStateExposesListTotal 锁定运营页的两个口径同时可见：
// list_total 是名单行数，State.Total 是上次刷新写入的行数，二者可能短暂不同
// （例如名单被手工改过），页面才有解释「名单 400、本轮排除 12」的依据。
func TestBalanceAlertExcludeStateExposesListTotal(t *testing.T) {
	n9e := testBalanceAlertRouterCtx(t)
	at := time.Date(2026, 10, 22, 7, 30, 0, 0, time.Local)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt: &at,
		WindowDays:    90,
		Total:         9, // 与名单真实行数不同，证明两个字段各有来源
		Reasons:       map[string]int{models.ExcludeReasonOpenDebt: 9},
	}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "a", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: at},
		{BillingAccountID: "b", Reason: models.ExcludeReasonRecentAdvance, RefreshedAt: at},
	}); err != nil {
		t.Fatal(err)
	}

	rt := &Router{Ctx: n9e}
	dat := decodeData(t, rt.balanceAlertExcludeStateGet)

	if ready, _ := dat["ready"].(bool); !ready {
		t.Fatalf("state with a success must be ready: %v", dat)
	}
	if got, _ := dat["list_total"].(float64); got != 2 {
		t.Fatalf("list_total must be the table row count, got %v", dat["list_total"])
	}
	if got, _ := dat["total"].(float64); got != 9 {
		t.Fatalf("total must stay the last refresh's count, got %v", dat["total"])
	}
	if _, ok := dat["last_success_at"]; !ok {
		t.Fatalf("last_success_at must stay exposed: %v", dat)
	}

	// 从未成功刷新：ready 必须为假，页面据此显示阻断提示。
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{}, "root"); err != nil {
		t.Fatal(err)
	}
	dat = decodeData(t, rt.balanceAlertExcludeStateGet)
	if ready, _ := dat["ready"].(bool); ready {
		t.Fatalf("never-refreshed state must not be ready: %v", dat)
	}
}

// TestBalanceAlertExcludesGetDegradesWhenBillingUnreachable 锁定明细接口的降级契约：
//
// 企业名、账户状态、逐条命中都要连计费库才能拿到，但明细的骨架（谁在名单里、主因）
// 在 alerting 自己的库里。计费库不可达时恰恰是最需要看上一份名单的时刻，所以接口
// 必须仍然返回 200 与正确的骨架，只把 enrich_error 置为非空。
//
// 这里通过「不配置任何 pgsql 数据源」制造失败：OpenStore 会在 resolveDatasource
// 阶段就报错，不需要真去连一个不存在的地址。
func TestBalanceAlertExcludesGetDegradesWhenBillingUnreachable(t *testing.T) {
	n9e := testBalanceAlertRouterCtx(t)
	at := time.Date(2026, 10, 23, 7, 30, 0, 0, time.Local)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		LastSuccessAt: &at,
		WindowDays:    90,
		Total:         2,
		Reasons:       map[string]int{models.ExcludeReasonOpenDebt: 2},
	}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "ba-1", Reason: models.ExcludeReasonOpenDebt, RefreshedAt: at},
		{BillingAccountID: "ba-2", Reason: models.ExcludeReasonRecentAdvance, RefreshedAt: at},
	}); err != nil {
		t.Fatal(err)
	}

	rt := &Router{Ctx: n9e}
	dat := decodeData(t, rt.balanceAlertExcludesGet)

	if got, _ := dat["list_total"].(float64); got != 2 {
		t.Fatalf("list_total must survive a billing failure, got %v", dat["list_total"])
	}
	if ready, _ := dat["ready"].(bool); !ready {
		t.Fatalf("a refreshed list must stay ready: %v", dat)
	}
	// 窗口用上次刷新落库的值，而不是重新读配置：这次状态里是 90，配置默认也是 90，
	// 所以另设一个不同的值来证明来源。
	if got, _ := dat["window_days"].(float64); got != 90 {
		t.Fatalf("window_days = %v, want the refresh-time value 90", dat["window_days"])
	}
	if msg, _ := dat["enrich_error"].(string); msg == "" {
		t.Fatalf("billing failure must be reported through enrich_error: %v", dat)
	}

	items, _ := dat["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("skeleton rows must still be returned, got %v", dat["items"])
	}
	first, _ := items[0].(map[string]interface{})
	if first["billing_account_id"] != "ba-1" {
		t.Fatalf("rows must be ordered by account id, got %v", first)
	}
	if first["reason"] != models.ExcludeReasonOpenDebt {
		t.Fatalf("reason must be the materialized one, got %v", first["reason"])
	}
	// found 必须是 false 而不是「未知」：补充查询整体失败时不能谎称账户存在。
	if found, _ := first["found"].(bool); found {
		t.Fatalf("found must be false when briefs could not be enriched: %v", first)
	}
}

// TestBalanceAlertExcludesGetWindowComesFromRefreshState 锁定「明细用上次刷新的窗口」。
// 拿当前配置去重算命中，会让改过窗口之后的每一次对比都是噪声。
func TestBalanceAlertExcludesGetWindowComesFromRefreshState(t *testing.T) {
	n9e := testBalanceAlertRouterCtx(t)
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{
		WindowDays: 30, // 与配置默认的 90 不同
	}, "root"); err != nil {
		t.Fatal(err)
	}
	rt := &Router{Ctx: n9e}
	dat := decodeData(t, rt.balanceAlertExcludesGet)
	if got, _ := dat["window_days"].(float64); got != 30 {
		t.Fatalf("window_days must come from the refresh state, got %v", dat["window_days"])
	}

	// 状态里没有记录窗口（旧版本状态）：退回当前配置，而不是显示 0。
	if err := models.BalanceAlertExcludeStatePut(n9e, models.BalanceAlertExcludeState{}, "root"); err != nil {
		t.Fatal(err)
	}
	dat = decodeData(t, rt.balanceAlertExcludesGet)
	if got, _ := dat["window_days"].(float64); got != float64(models.DefaultCreditAdvanceWindowDays) {
		t.Fatalf("window_days must fall back to settings, got %v", dat["window_days"])
	}
}

// TestMyExcludeViewCustomerCopy 锁定客户侧契约：只给布尔值与一句可读文案，
// 且文案不泄露内部归因（OPEN_DEBT / RECENT_SETTLEMENT 对客户不可操作）。
func TestMyExcludeViewCustomerCopy(t *testing.T) {
	n9e := testBalanceAlertRouterCtx(t)
	at := time.Now()
	if err := models.BalanceAlertCreditExcludeReplace(n9e, []models.BalanceAlertCreditExclude{
		{BillingAccountID: "ba-credit", Reason: models.ExcludeReasonRecentSettlement, RefreshedAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	rt := &Router{Ctx: n9e}

	excluded, notice, err := rt.myExcludeView("ba-credit")
	if err != nil {
		t.Fatal(err)
	}
	if !excluded || notice != models.ExcludeNoticeCustomer {
		t.Fatalf("excluded=%v notice=%q", excluded, notice)
	}

	excluded, notice, err = rt.myExcludeView("ba-prepaid")
	if err != nil {
		t.Fatal(err)
	}
	if excluded || notice != "" {
		t.Fatalf("a non-excluded account must get an empty notice, got %v/%q", excluded, notice)
	}
}
