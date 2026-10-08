package router

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/balancealert"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/authing"
	"github.com/ccfos/nightingale/v6/pkg/ginx"

	"github.com/gin-gonic/gin"
)

func (rt *Router) balanceAlertNotifyRulesGet(c *gin.Context) {
	lst, err := models.NotifyRulesGet(rt.Ctx, "", nil)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	type item struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Enable bool   `json:"enable"`
	}
	out := make([]item, 0, len(lst))
	for _, r := range lst {
		if r == nil {
			continue
		}
		out = append(out, item{ID: r.ID, Name: r.Name, Enable: r.Enable})
	}
	ginx.NewRender(c).Data(out, nil)
}

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

func (rt *Router) balanceAlertMissReports(c *gin.Context) {
	res, err := balancealert.ReconMissReports(rt.Ctx, ginx.QueryInt(c, "days", 7))
	ginx.NewRender(c).Data(res, err)
}

func (rt *Router) balanceAlertConfigGet(c *gin.Context) {
	accountID := ginx.UrlParamStr(c, "account_id")
	cfg, err := models.BalanceAlertConfigGet(rt.Ctx, accountID)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	if cfg == nil {
		ginx.NewRender(c).Data(nil, nil)
		return
	}
	ginx.NewRender(c).Data(cfg, nil)
}

func (rt *Router) balanceAlertConfigPut(c *gin.Context) {
	accountID := ginx.UrlParamStr(c, "account_id")
	var body struct {
		Enabled           *bool    `json:"enabled"`
		ThresholdMode     string   `json:"threshold_mode"`
		ThresholdFixedUSD float64  `json:"threshold_fixed_usd"`
		Receivers         []string `json:"receivers"`
		DynamicEnabled    bool     `json:"dynamic_enabled"`
	}
	ginx.BindJSON(c, &body)

	if body.ThresholdMode == "" {
		body.ThresholdMode = models.ThresholdModeAuto
	}
	if body.ThresholdMode != models.ThresholdModeAuto && body.ThresholdMode != models.ThresholdModeCustom {
		ginx.Bomb(400, "threshold_mode must be AUTO or CUSTOM")
	}
	if body.ThresholdMode == models.ThresholdModeCustom && body.ThresholdFixedUSD <= 0 {
		ginx.Bomb(400, "threshold_fixed_usd must be > 0 when threshold_mode is CUSTOM")
	}

	existing, err := models.BalanceAlertConfigGet(rt.Ctx, accountID)
	if err != nil {
		ginx.NewRender(c).Message(err)
		return
	}
	enabled := true
	if existing != nil {
		enabled = existing.Enabled
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	cfg := models.BalanceAlertConfig{
		Enabled:           enabled,
		ThresholdMode:     body.ThresholdMode,
		ThresholdFixedUSD: body.ThresholdFixedUSD,
		DynamicEnabled:    body.DynamicEnabled,
	}
	cfg.SetReceiverList(body.Receivers)

	ginx.NewRender(c).Message(models.BalanceAlertConfigPut(rt.Ctx, accountID, cfg))
}

var phoneRe = regexp.MustCompile(`^1[3-9]\d{9}$`)

func loginContact(user *models.User) (email, phone string) {
	if user == nil {
		return "", ""
	}
	email = strings.TrimSpace(user.Email)
	phone = strings.TrimSpace(user.Phone)
	if phone == "" {
		u := strings.TrimSpace(user.Username)
		if phoneRe.MatchString(u) {
			phone = u
		}
	}
	return email, phone
}

const (
	myAccountNeedBind = "need_bind"
	myAccountNotFound = "not_found"
)

func (rt *Router) resolveMyBillingAccount(c *gin.Context) (*balancealert.MyAccount, string, error) {
	username := c.MustGet("username").(string)
	user, err := models.UserGetByUsername(rt.Ctx, username)
	if err != nil {
		return nil, "", err
	}
	if user == nil {
		return nil, myAccountNotFound, nil
	}
	email, phone := loginContact(user)
	if email == "" && phone == "" {
		return nil, myAccountNeedBind, nil
	}
	settings, err := models.BalanceAlertSettingsGet(rt.Ctx)
	if err != nil {
		return nil, "", err
	}
	store, err := balancealert.OpenStore(rt.Ctx, settings)
	if err != nil {
		return nil, "", err
	}
	acc, err := store.ResolveAccountByEmailOrPhone(c.Request.Context(), email, phone)
	if err != nil {
		return nil, "", err
	}
	if acc == nil {
		return nil, myAccountNotFound, nil
	}
	return acc, "", nil
}

func myAccountEmpty(reason string) map[string]interface{} {
	return map[string]interface{}{
		"billing_account_id": "",
		"reason":             reason,
	}
}

func myAccountMissingMsg(reason string) string {
	if reason == myAccountNeedBind {
		return "请先在个人中心绑定邮箱或手机号"
	}
	return "未找到关联的预付费企业账户"
}

func (rt *Router) balanceAlertMyConfigGet(c *gin.Context) {
	acc, reason, err := rt.resolveMyBillingAccount(c)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	if acc == nil {
		ginx.NewRender(c).Data(myAccountEmpty(reason), nil)
		return
	}
	cfg, err := models.BalanceAlertConfigGet(rt.Ctx, acc.ID)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	out := map[string]interface{}{
		"billing_account_id":      acc.ID,
		"enterprise_name":         acc.Name,
		"current_phone":           acc.Phone,
		"balance_usd":             acc.Balance,
		"enabled":                 true,
		"threshold_mode":          models.ThresholdModeAuto,
		"threshold_fixed_usd":     float64(0),
		"threshold_usd":           float64(0),
		"platform_threshold_usd":  float64(0),
		"platform_threshold_mode": "",
		"receivers":               []string{},
		"dynamic_enabled":         false,
		"current_state":           models.BalanceAlertStateNormal,
	}
	if cfg != nil {
		out["enabled"] = cfg.Enabled
		out["threshold_mode"] = cfg.ThresholdMode
		out["threshold_fixed_usd"] = cfg.ThresholdFixedUSD
		out["receivers"] = cfg.ReceiverList()
		out["dynamic_enabled"] = cfg.DynamicEnabled
		out["current_state"] = cfg.CurrentState
	}
	settings, _ := models.BalanceAlertSettingsGet(rt.Ctx)
	platformTh, platformMode, _ := balancealert.ComputeThreshold(acc.LastRecharge, acc.HasVoucher, settings.VoucherThresholdUSD)
	out["platform_threshold_usd"] = platformTh
	out["platform_threshold_mode"] = platformMode
	out["threshold_usd"] = platformTh
	if cfg != nil && cfg.IsCustomThreshold() {
		out["threshold_usd"] = cfg.ThresholdFixedUSD
	}
	receivers, _ := out["receivers"].([]string)
	if len(receivers) == 0 && strings.TrimSpace(acc.Phone) != "" {
		out["receivers"] = []string{strings.TrimSpace(acc.Phone)}
	}
	ginx.NewRender(c).Data(out, nil)
}

func (rt *Router) balanceAlertMyConfigPut(c *gin.Context) {
	acc, reason, err := rt.resolveMyBillingAccount(c)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	if acc == nil {
		ginx.NewRender(c).Message(myAccountMissingMsg(reason))
		return
	}
	var body struct {
		Enabled           *bool    `json:"enabled"`
		ThresholdMode     string   `json:"threshold_mode"`
		ThresholdFixedUSD float64  `json:"threshold_fixed_usd"`
		Receivers         []string `json:"receivers"`
		DynamicEnabled    bool     `json:"dynamic_enabled"`
	}
	ginx.BindJSON(c, &body)

	if body.ThresholdMode == "" {
		body.ThresholdMode = models.ThresholdModeAuto
	}
	if body.ThresholdMode != models.ThresholdModeAuto && body.ThresholdMode != models.ThresholdModeCustom {
		ginx.NewRender(c).Message("threshold_mode must be AUTO or CUSTOM")
		return
	}
	if body.ThresholdMode == models.ThresholdModeCustom && body.ThresholdFixedUSD <= 0 {
		ginx.NewRender(c).Message("自定义警戒线金额必须大于 0")
		return
	}
	if body.ThresholdMode == models.ThresholdModeCustom {
		settings, err := models.BalanceAlertSettingsGet(rt.Ctx)
		if err != nil {
			ginx.NewRender(c).Message(err)
			return
		}
		platformTh, _, skip := balancealert.ComputeThreshold(acc.LastRecharge, acc.HasVoucher, settings.VoucherThresholdUSD)
		if !skip && platformTh > 0 && body.ThresholdFixedUSD <= platformTh {
			ginx.NewRender(c).Message(fmt.Sprintf("自定义警戒线必须高于平台默认线（当前 %.2f 元）", platformTh))
			return
		}
	}
	if len(body.Receivers) > models.MaxBalanceAlertReceivers {
		ginx.NewRender(c).Message("接收手机号最多 5 个")
		return
	}
	for _, phone := range body.Receivers {
		if !phoneRe.MatchString(strings.TrimSpace(phone)) {
			ginx.NewRender(c).Message("手机号格式不正确：" + phone)
			return
		}
	}

	trimmed := make([]string, 0, len(body.Receivers))
	for _, p := range body.Receivers {
		if p = strings.TrimSpace(p); p != "" {
			trimmed = append(trimmed, p)
		}
	}

	existing, err := models.BalanceAlertConfigGet(rt.Ctx, acc.ID)
	if err != nil {
		ginx.NewRender(c).Message(err)
		return
	}
	enabled := true
	oldPhones := map[string]struct{}{}
	if existing != nil {
		enabled = existing.Enabled
		for _, p := range existing.ReceiverList() {
			oldPhones[p] = struct{}{}
		}
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	owner := strings.TrimSpace(acc.Phone)
	now := time.Now()
	for _, p := range trimmed {
		if _, ok := oldPhones[p]; ok {
			continue
		}
		if p == owner {
			continue
		}
		if !models.BalanceAlertOTPVerified(rt.Ctx, acc.ID, p, now) {
			ginx.NewRender(c).Message("新增手机号需先完成短信验证：" + p)
			return
		}
	}

	cfg := models.BalanceAlertConfig{
		Enabled:           enabled,
		ThresholdMode:     body.ThresholdMode,
		ThresholdFixedUSD: body.ThresholdFixedUSD,
		DynamicEnabled:    body.DynamicEnabled,
	}
	cfg.SetReceiverList(trimmed)

	ginx.NewRender(c).Message(models.BalanceAlertConfigPut(rt.Ctx, acc.ID, cfg))
}

func (rt *Router) balanceAlertMyRecordsGet(c *gin.Context) {
	acc, _, err := rt.resolveMyBillingAccount(c)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	if acc == nil {
		ginx.NewRender(c).Data([]models.BalanceAlertRecord{}, nil)
		return
	}
	lst, err := models.BalanceAlertRecordGets(rt.Ctx,
		acc.ID,
		ginx.QueryStr(c, "level", ""),
		"",
		ginx.QueryInt(c, "limit", 50),
	)
	ginx.NewRender(c).Data(lst, err)
}

func (rt *Router) balanceAlertMyStatusGet(c *gin.Context) {
	acc, reason, err := rt.resolveMyBillingAccount(c)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	if acc == nil {
		ginx.NewRender(c).Data(myAccountEmpty(reason), nil)
		return
	}
	cfg, err := models.BalanceAlertConfigGet(rt.Ctx, acc.ID)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	out := map[string]interface{}{
		"balance_usd":            acc.Balance,
		"current_state":          models.BalanceAlertStateNormal,
		"enabled":                true,
		"threshold_mode":         models.ThresholdModeAuto,
		"threshold_usd":          0,
		"platform_threshold_usd": 0,
		"dynamic_enabled":        false,
		"last_alert_at":          nil,
	}
	if cfg != nil {
		out["current_state"] = cfg.CurrentState
		out["enabled"] = cfg.Enabled
		out["threshold_mode"] = cfg.ThresholdMode
		out["dynamic_enabled"] = cfg.DynamicEnabled
		out["last_alert_at"] = cfg.LastAlertAt
	}
	settings, _ := models.BalanceAlertSettingsGet(rt.Ctx)
	platformTh, _, _ := balancealert.ComputeThreshold(acc.LastRecharge, acc.HasVoucher, settings.VoucherThresholdUSD)
	out["platform_threshold_usd"] = platformTh
	out["threshold_usd"] = platformTh
	if cfg != nil && cfg.IsCustomThreshold() {
		out["threshold_usd"] = cfg.ThresholdFixedUSD
	}
	ginx.NewRender(c).Data(out, nil)
}

func (rt *Router) balanceAlertMyOTPSend(c *gin.Context) {
	acc, reason, err := rt.resolveMyBillingAccount(c)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	if acc == nil {
		ginx.NewRender(c).Message(myAccountMissingMsg(reason))
		return
	}
	var body struct {
		Phone string `json:"phone"`
	}
	ginx.BindJSON(c, &body)
	phone := strings.TrimSpace(body.Phone)
	if !phoneRe.MatchString(phone) {
		ginx.NewRender(c).Message("手机号格式不正确")
		return
	}
	if err := models.BalanceAlertOTPBegin(rt.Ctx, acc.ID, phone, time.Now()); err != nil {
		ginx.NewRender(c).Message(err.Error())
		return
	}
	if rt.AuthingOTP == nil {
		ginx.NewRender(c).Message(authing.ErrNotConfigured.Error())
		return
	}
	if err := rt.AuthingOTP.SendLoginSMS(phone); err != nil {
		ginx.NewRender(c).Message(err.Error())
		return
	}
	ginx.NewRender(c).Message("")
}

func (rt *Router) balanceAlertMyOTPVerify(c *gin.Context) {
	acc, reason, err := rt.resolveMyBillingAccount(c)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	if acc == nil {
		ginx.NewRender(c).Message(myAccountMissingMsg(reason))
		return
	}
	var body struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	ginx.BindJSON(c, &body)
	phone := strings.TrimSpace(body.Phone)
	if !phoneRe.MatchString(phone) {
		ginx.NewRender(c).Message("手机号格式不正确")
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		ginx.NewRender(c).Message("请输入验证码")
		return
	}
	if rt.AuthingOTP == nil {
		ginx.NewRender(c).Message(authing.ErrNotConfigured.Error())
		return
	}
	if err := rt.AuthingOTP.VerifyPhoneCode(phone, body.Code); err != nil {
		ginx.NewRender(c).Message(err.Error())
		return
	}
	if err := models.BalanceAlertOTPMarkVerified(rt.Ctx, acc.ID, phone, time.Now()); err != nil {
		ginx.NewRender(c).Message(err.Error())
		return
	}
	ginx.NewRender(c).Message("")
}
