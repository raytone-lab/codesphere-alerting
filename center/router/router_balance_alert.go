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
	"github.com/toolkits/pkg/logger"
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

	prev, err := models.BalanceAlertSettingsGet(rt.Ctx)
	if err != nil {
		ginx.NewRender(c).Message(err)
		return
	}
	if err := models.BalanceAlertSettingsPut(rt.Ctx, s, username); err != nil {
		ginx.NewRender(c).Message(err)
		return
	}
	// 告警停用期间发生的失败不会被投递；开关一打开就补报，否则「从未成功
	// 刷新」要等到下一次失败（最长一天后）才重新可见。
	//
	// 不再二次读取 settings 判断新状态：NotifyPendingExcludeOps 自己首行就检查
	// 开关，多读一次只会多一个被静默吞掉的错误点。
	if !prev.AlertEnabled() {
		balancealert.NotifyPendingExcludeOpsAsync(rt.Ctx)
	}
	ginx.NewRender(c).Message(nil)
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
	if stats.ExcludeBlocked {
		// fail closed 是策略降级，不是请求失败：把标志交给页面渲染成明确原因，
		// 而不是把原始 Go 错误当成一条 toast 弹出来（ginx 的 Data 在 err != nil
		// 时会丢掉 dat，页面就永远看不到 exclude_blocked）。
		if err != nil {
			logger.Warningf("balancealert run: %v", err)
			stats.Errors = append(stats.Errors, err.Error())
		}
		ginx.NewRender(c).Data(stats, nil)
		return
	}
	ginx.NewRender(c).Data(stats, err)
}

// balanceAlertExcludeStateGet 暴露排除名单刷新健康度。页面用它显示「上次刷新」
// 与「从未成功」的红色提示 —— fail closed 时运营必须能一眼看出当前没在圈人。
//
// list_total 是名单当前行数（名单规模），与判定轮次的 Stats.Excluded（本轮命中）
// 是两个口径：名单不按账户状态过滤，可能含已暂停/已删除账户，所以「名单 400 家、
// 本轮只排除 12 家」是正常的。两者并列展示才能解释这个差异。
func (rt *Router) balanceAlertExcludeStateGet(c *gin.Context) {
	state, err := models.BalanceAlertExcludeStateGet(rt.Ctx)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}
	_, listTotal, err := models.BalanceAlertCreditExcludeReasonCounts(rt.Ctx)
	if err != nil {
		// 这是诊断端点：计数失败不该让「名单是否可用」也查不到。
		// 退回上次成功刷新时记录的行数，并在响应里保留 last_error。
		logger.Warningf("balancealert exclude-state: count list rows: %v", err)
		listTotal = state.Total
	}
	ginx.NewRender(c).Data(struct {
		models.BalanceAlertExcludeState
		Ready     bool `json:"ready"`
		ListTotal int  `json:"list_total"`
	}{state, state.Ready(), listTotal}, nil)
}

// balanceAlertExcludeRefresh 手动全量刷新，成功后返回名单规模与 reason 分布，
// 供试点期运营当场核对判定准确性。
//
// 失败只回显在页面上，不推钉钉：点按钮的人就在页面上，再推一条只是重复告知。
func (rt *Router) balanceAlertExcludeRefresh(c *gin.Context) {
	res, err := balancealert.RefreshCreditExcludes(rt.Ctx, balancealert.TriggerManual)
	ginx.NewRender(c).Data(res, err)
}

// excludeEnrich 是名单明细页对计费库的两次补充查询结果。
//
// 二者都是**装饰性**的：骨架（谁在名单里、主因、刷新时间）已经在 alerting 自己的
// 库里查到了。任何一侧失败都只降级成一句提示，绝不让整页打不开。
type excludeEnrich struct {
	Briefs map[string]balancealert.AccountBrief
	Hits   map[string]balancealert.CreditExcludeHits
	Err    string
}

// excludeEnrichTimeout 是这两次补充查询共用的硬上限。
//
// 明细页的骨架在 alerting 自己的库里，计费库只用来补企业名与命中标记。但 OpenStore
// 建连是拨号 + Ping，一次卡住的拨号最长可到 connect_timeout（默认 60s），比 HTTP 写
// 超时（40s）还长。若同步等待，计费库一挂，最需要看上一份名单的那一刻反而打不开页面。
// 因此这里异步等待并设上限：超时就放弃补充，照常返回明细。
const excludeEnrichTimeout = 3 * time.Second

// balanceAlertExcludesGet 返回排除名单明细，供运营页解释「哪些企业被排除、因为什么」。
//
// 骨架（谁在名单里、主因、刷新时间）来自 alerting 自己的库；企业名、账户状态与
// 三条条件的逐条命中是对计费库的**尽力而为**补充：
//   - 补不上（计费库不可达、权限缺失、超时）只让 enrich_error 非空，接口仍然返回 200。
//     明细是诊断入口，恰恰在计费库出问题时最需要能看到上一份名单。
//   - 这一步不在 15 分钟判定轮次里：只按主键取几百行身份 + 一次与刷新同源的聚合，
//     不回扫消费明细，也不写计费库。
func (rt *Router) balanceAlertExcludesGet(c *gin.Context) {
	limit := ginx.QueryInt(c, "limit", models.MaxBalanceAlertCreditExcludeList)
	rows, err := models.BalanceAlertCreditExcludeGets(rt.Ctx, limit)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}

	// 计数单独取一次：明细可能被 limit 截断，而统计必须始终是全量口径，
	// 否则「全部 4 家」会在名单变大后变成「全部 2000 家」。
	reasons, listTotal, err := models.BalanceAlertCreditExcludeReasonCounts(rt.Ctx)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
		return
	}

	state, err := models.BalanceAlertExcludeStateGet(rt.Ctx)
	if err != nil {
		// 状态读不出来不该连明细一起看不到；窗口天数留空由页面显示「—」。
		logger.Warningf("balancealert excludes: read state: %v", err)
		state = models.BalanceAlertExcludeState{}
	}

	windowDays := rt.excludeDetailWindow(state)
	enrich := rt.lookupExcludeEnrich(rows, windowDays)

	type hits struct {
		OpenDebt         bool `json:"open_debt"`
		RecentAdvance    bool `json:"recent_advance"`
		RecentSettlement bool `json:"recent_settlement"`
		// Any 由后端算，不让页面从三个 bool 自己推：口径只留一处。
		Any bool `json:"any"`
	}
	type item struct {
		BillingAccountID string    `json:"billing_account_id"`
		Reason           string    `json:"reason"`
		RefreshedAt      time.Time `json:"refreshed_at"`
		// Found=false 表示计费库查不到这个账户（可能已被物理删除），
		// 与「补充查询失败」不同：后者会让 enrich_error 非空，此时 found 无意义。
		Found          bool   `json:"found"`
		EnterpriseName string `json:"enterprise_name,omitempty"`
		AccountType    string `json:"account_type,omitempty"`
		AccountStatus  string `json:"account_status,omitempty"`
		Deleted        bool   `json:"deleted"`
		Quarantined    bool   `json:"quarantined"`
		// Hits 是**按当前计费库实时重算**的三条命中，可能比名单新。
		// 三条皆假 = 现在已不该排除（名单还没到下次刷新）。
		Hits hits `json:"hits"`
	}
	items := make([]item, 0, len(rows))
	for _, r := range rows {
		it := item{
			BillingAccountID: r.BillingAccountID,
			Reason:           r.Reason,
			RefreshedAt:      r.RefreshedAt,
		}
		if b, ok := enrich.Briefs[r.BillingAccountID]; ok {
			it.Found = true
			it.EnterpriseName = b.Name
			it.AccountType = b.Type
			it.AccountStatus = b.Status
			it.Deleted = b.Deleted
			it.Quarantined = b.Quarantined
		}
		if h, ok := enrich.Hits[r.BillingAccountID]; ok {
			it.Hits = hits{
				OpenDebt:         h.OpenDebt,
				RecentAdvance:    h.RecentAdvance,
				RecentSettlement: h.RecentSettlement,
				Any:              h.Any(),
			}
		}
		items = append(items, it)
	}

	ginx.NewRender(c).Data(struct {
		ListTotal     int            `json:"list_total"`
		WindowDays    int            `json:"window_days"`
		LastSuccessAt *time.Time     `json:"last_success_at"`
		Reasons       map[string]int `json:"reasons"`
		Ready         bool           `json:"ready"`
		Truncated     bool           `json:"truncated"`
		Items         []item         `json:"items"`
		EnrichError   string         `json:"enrich_error"`
	}{
		ListTotal:     listTotal,
		WindowDays:    windowDays,
		LastSuccessAt: state.LastSuccessAt,
		Reasons:       reasons,
		Ready:         state.Ready(),
		Truncated:     listTotal > len(items),
		Items:         items,
		EnrichError:   enrich.Err,
	}, nil)
}

// excludeDetailWindow 决定明细页重算命中时用哪个窗口。
//
// 用**上次刷新时落库**的窗口，而不是当前配置：明细页要解释的是「名单当时是怎么判的」，
// 拿一个改过的新窗口去重算，得到的不一致全是噪声。上次刷新没记录窗口（旧状态）时才
// 退回当前配置（WindowDays 已经处理 nil 与越界收敛）。
func (rt *Router) excludeDetailWindow(state models.BalanceAlertExcludeState) int {
	if state.WindowDays > 0 {
		return state.WindowDays
	}
	settings, err := models.BalanceAlertSettingsGet(rt.Ctx)
	if err != nil {
		logger.Warningf("balancealert excludes: read settings for window: %v", err)
		return models.DefaultCreditAdvanceWindowDays
	}
	return settings.WindowDays()
}

// lookupExcludeEnrich 尽力补企业名、账户状态与逐条命中，返回可直接渲染的结果与提示。
//
// 两次查询放在**同一个** goroutine 里串行执行而不是各起一个：它们共用同一个连接池，
// 并行只会让超时预算更难解释，而总耗时本来就是几十毫秒量级。
func (rt *Router) lookupExcludeEnrich(rows []models.BalanceAlertCreditExclude, windowDays int) excludeEnrich {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.BillingAccountID != "" {
			ids = append(ids, r.BillingAccountID)
		}
	}
	if len(ids) == 0 {
		return excludeEnrich{}
	}

	settings, err := models.BalanceAlertSettingsGet(rt.Ctx)
	if err != nil {
		logger.Warningf("balancealert excludes: read settings: %v", err)
		return excludeEnrich{Err: "读取预警设置失败，已省略企业名称、账户状态与命中标记"}
	}

	type result struct {
		briefs map[string]balancealert.AccountBrief
		hits   map[string]balancealert.CreditExcludeHits
		err    error
	}
	// 缓冲 1：超时后 goroutine 仍能写完并退出，不会因无人接收而永久阻塞。
	ch := make(chan result, 1)
	go func() {
		briefs, err := balancealert.LookupAccountBriefs(rt.Ctx, settings, ids)
		if err != nil {
			ch <- result{err: err}
			return
		}
		hits, err := balancealert.LookupCreditExcludeHits(rt.Ctx, settings, windowDays)
		ch <- result{briefs: briefs, hits: hits, err: err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			logger.Warningf("balancealert excludes: enrich from billing: %v", r.err)
			// 名字与命中是两批数据，一批失败就都留空：页面据此统一收起这两类列，
			// 而不是一半有值一半空白。
			return excludeEnrich{Err: "计费库查询失败，已省略企业名称、账户状态与命中标记：" + r.err.Error()}
		}
		return excludeEnrich{Briefs: r.briefs, Hits: r.hits}
	case <-time.After(excludeEnrichTimeout):
		logger.Warningf("balancealert excludes: enrich timed out after %s", excludeEnrichTimeout)
		return excludeEnrich{Err: fmt.Sprintf(
			"计费库 %d 秒内未返回，已省略企业名称、账户状态与命中标记（名单本身仍然准确）",
			int(excludeEnrichTimeout/time.Second))}
	}
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

// myExcludeView 把「该账户是否在授信排除名单内」翻译成自助页需要的两个字段。
//
// 只暴露布尔值与一句客户可读文案，不暴露 reason 细分类别：OPEN_DEBT /
// RECENT_SETTLEMENT 这类内部归因对客户既不可操作也是困惑源，只留在运营页。
func (rt *Router) myExcludeView(accountID string) (bool, string, error) {
	excluded, err := models.BalanceAlertCreditExcludeHas(rt.Ctx, accountID)
	if err != nil {
		return false, "", err
	}
	if !excluded {
		return false, "", nil
	}
	return true, models.ExcludeNoticeCustomer, nil
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
	excluded, notice, err := rt.myExcludeView(acc.ID)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
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
		// 排除态：配置仍然回显（让客户看到当前值），但前端必须整块只读禁用。
		"excluded":       excluded,
		"exclude_notice": notice,
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
	platformTh, platformMode, _ := balancealert.ComputeThreshold(acc.LastRecharge, acc.HasVoucher, settings.VoucherThresholdUSD, settings.LastRechargePct)
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
	// 排除态下拒绝写入：允许保存但永不生效是最差的组合——客户会以为设置成功了。
	// 复用 GET 的同一句文案，不另造状态码。
	if excluded, notice, err := rt.myExcludeView(acc.ID); err != nil {
		ginx.NewRender(c).Message(err)
		return
	} else if excluded {
		ginx.NewRender(c).Message(notice + "，暂不可修改")
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
		platformTh, _, skip := balancealert.ComputeThreshold(acc.LastRecharge, acc.HasVoucher, settings.VoucherThresholdUSD, settings.LastRechargePct)
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
	excluded, notice, err := rt.myExcludeView(acc.ID)
	if err != nil {
		ginx.NewRender(c).Data(nil, err)
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
		// 与 my-config 同源同值：两个页面说法不一致比不说更糟。
		"excluded":       excluded,
		"exclude_notice": notice,
	}
	if cfg != nil {
		out["current_state"] = cfg.CurrentState
		out["enabled"] = cfg.Enabled
		out["threshold_mode"] = cfg.ThresholdMode
		out["dynamic_enabled"] = cfg.DynamicEnabled
		out["last_alert_at"] = cfg.LastAlertAt
	}
	settings, _ := models.BalanceAlertSettingsGet(rt.Ctx)
	platformTh, _, _ := balancealert.ComputeThreshold(acc.LastRecharge, acc.HasVoucher, settings.VoucherThresholdUSD, settings.LastRechargePct)
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
