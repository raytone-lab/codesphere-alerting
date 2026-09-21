package router

import (
	"strings"

	"github.com/ccfos/nightingale/v6/balancealert"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ginx"

	"github.com/gin-gonic/gin"
)

func (rt *Router) balanceAlertSettingsGet(c *gin.Context) {
	s, err := models.BalanceAlertSettingsGet(rt.Ctx)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	ginx.NewRender(c).Data(s.Public(), nil)
}

func (rt *Router) balanceAlertSettingsPut(c *gin.Context) {
	var s models.BalanceAlertSettings
	ginx.BindJSON(c, &s)
	username := c.MustGet("username").(string)
	ginx.NewRender(c).Message(models.BalanceAlertSettingsPut(rt.Ctx, s, username))
}

func (rt *Router) balanceAlertRecordsGet(c *gin.Context) {
	lst, err := models.BalanceAlertRecordGets(rt.Ctx,
		ginx.QueryStr(c, "billing_account_id", ""),
		ginx.QueryStr(c, "level", ""),
		ginx.QueryStr(c, "status", ""),
		ginx.QueryInt(c, "limit", 100),
	)
	ginx.NewRender(c).Data(lst, err)
}

func (rt *Router) balanceAlertConfigsGet(c *gin.Context) {
	lst, err := models.BalanceAlertConfigGets(rt.Ctx, ginx.QueryInt(c, "limit", 200))
	ginx.NewRender(c).Data(lst, err)
}

func (rt *Router) balanceAlertRecordReview(c *gin.Context) {
	id := ginx.UrlParamStr(c, "id")
	var f struct {
		OperatorReview string `json:"operator_review"`
	}
	ginx.BindJSON(c, &f)
	review := strings.ToUpper(strings.TrimSpace(f.OperatorReview))
	if review != "OK" && review != "FALSE_ALARM" {
		ginx.Bomb(400, "operator_review must be OK or FALSE_ALARM")
	}
	ginx.NewRender(c).Message(models.BalanceAlertRecordSetReview(rt.Ctx, id, review))
}

func (rt *Router) balanceAlertRun(c *gin.Context) {
	stats, err := balancealert.RunOnce(rt.Ctx, nil)
	ginx.NewRender(c).Data(stats, err)
}
