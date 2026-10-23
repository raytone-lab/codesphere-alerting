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
