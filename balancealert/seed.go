package balancealert

import (
	"encoding/json"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"

	"github.com/toolkits/pkg/logger"
)

type seedTpl struct {
	Name               string
	Ident              string
	NotifyChannelIdent string
	Content            map[string]string
	Weight             int
}

var seedTemplates = []seedTpl{
	{
		Name:               "余额预警-试点钉钉",
		Ident:              "balance-alert-pilot-dingtalk",
		NotifyChannelIdent: "dingtalk",
		Weight:             0,
		Content: map[string]string{
			"title":   "预付费余额预警",
			"content": "{{ index $event.AnnotationsJSON \"severity_emoji\" }} 告警级别：{{ index $event.AnnotationsJSON \"level\" }}级\n告警时间：{{ timeformat $event.TriggerTime }}\n企业名称：{{ index $event.AnnotationsJSON \"enterprise_name\" }}\n事由签名：{{ index $event.AnnotationsJSON \"copy\" }}\n当次触发时值：{{ $value }}",
		},
	},
	{
		Name:               "余额预警-短信-WARN",
		Ident:              "balance-alert-sms-warn",
		NotifyChannelIdent: "ali-sms",
		Weight:             0,
		Content:            aliyunBalanceSmsTpl,
	},
	{
		Name:               "余额预警-短信-CRITICAL",
		Ident:              "balance-alert-sms-critical",
		NotifyChannelIdent: "ali-sms",
		Weight:             0,
		Content:            aliyunBalanceSmsTpl,
	},
	{
		Name:               "余额预警-短信-体验额度",
		Ident:              "balance-alert-sms-voucher",
		NotifyChannelIdent: "ali-sms",
		Weight:             0,
		Content:            aliyunBalanceSmsTpl,
	},
	{
		Name:               "余额预警-短信-动态预警",
		Ident:              "balance-alert-dynamic-sms",
		NotifyChannelIdent: "ali-sms",
		Weight:             0,
		Content:            aliyunBalanceSmsTpl,
	},
}

// aliyunBalanceSmsTpl fills Aliyun template SMS_512500774 variables ${name} ${value}.
var aliyunBalanceSmsTpl = map[string]string{
	"name":  `{{ index $event.AnnotationsJSON "enterprise_name" }}`,
	"value": `¥{{ index $event.AnnotationsJSON "balance" }}`,
}

const (
	seedPilotNotifyRuleName    = "余额预警-试点钉钉"
	seedCustomerNotifyRuleName = "余额预警-客户短信"
	aliyunSMSChannelIdent      = "ali-sms"
	aliyunSMSChannelName       = "阿里云短信"
)

type seedNotifyRuleSpec struct {
	Name          string
	ChannelIdent  string
	TemplateIdent string
}

func SeedNotifyRules(n9e *ctx.Context) {
	seedAliyunSMSChannelIfMissing(n9e)
	for _, spec := range []seedNotifyRuleSpec{
		{Name: seedPilotNotifyRuleName, ChannelIdent: models.Dingtalk, TemplateIdent: "balance-alert-pilot-dingtalk"},
		{Name: seedCustomerNotifyRuleName, ChannelIdent: aliyunSMSChannelIdent, TemplateIdent: "balance-alert-sms-warn"},
	} {
		if err := seedOneNotifyRule(n9e, spec); err != nil {
			logger.Errorf("balancealert seed: notify rule %s: %v", spec.Name, err)
		}
	}
}

func seedAliyunSMSChannelIfMissing(n9e *ctx.Context) {
	ch, err := models.NotifyChannelGet(n9e, "ident = ?", aliyunSMSChannelIdent)
	if err != nil {
		logger.Errorf("balancealert seed: query channel %s: %v", aliyunSMSChannelIdent, err)
		return
	}
	if ch == nil {
		ch, err = models.NotifyChannelGet(n9e, "name = ?", aliyunSMSChannelName)
		if err != nil {
			logger.Errorf("balancealert seed: query channel name %s: %v", aliyunSMSChannelName, err)
			return
		}
	}
	if ch != nil {
		return
	}
	now := time.Now().Unix()
	ncc := &models.NotifyChannelConfig{
		Name:        aliyunSMSChannelName,
		Ident:       aliyunSMSChannelIdent,
		Enable:      true,
		RequestType: "http",
		Weight:      8,
		CreateAt:    now,
		UpdateAt:    now,
		CreateBy:    "system",
		UpdateBy:    "system",
		ParamConfig: &models.NotifyParamConfig{
			UserInfo: &models.UserInfo{ContactKey: "phone"},
		},
		RequestConfig: &models.RequestConfig{
			HTTPRequestConfig: &models.HTTPRequestConfig{
				URL:           "https://dysmsapi.aliyuncs.com",
				Method:        "POST",
				Timeout:       10000,
				RetryTimes:    2,
				RetryInterval: 100,
				Headers:       map[string]string{"Content-Type": "application/json"},
				Request: models.RequestDetail{
					Parameters: map[string]string{
						"PhoneNumbers":    "{{ $sendto }}",
						"SignName":        "",
						"TemplateCode":    "SMS_512500774",
						"TemplateParam":   `{"name":"","value":""}`,
						"AccessKeyId":     "",
						"AccessKeySecret": "",
					},
				},
			},
		},
	}
	if err := models.Insert(n9e, ncc); err != nil {
		logger.Errorf("balancealert seed: create channel %s: %v", aliyunSMSChannelIdent, err)
		return
	}
	logger.Infof("balancealert seed: created channel %s (%s)", aliyunSMSChannelIdent, aliyunSMSChannelName)
}

func seedOneNotifyRule(n9e *ctx.Context, spec seedNotifyRuleSpec) error {
	existing, err := models.NotifyRuleGet(n9e, "name = ?", spec.Name)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	channel, err := models.NotifyChannelGet(n9e, "ident = ?", spec.ChannelIdent)
	if err != nil {
		return err
	}
	if channel == nil {
		logger.Infof("balancealert seed: skip rule %s, channel ident %s not found", spec.Name, spec.ChannelIdent)
		return nil
	}
	tpl, err := models.MessageTemplateGet(n9e, "ident = ?", spec.TemplateIdent)
	if err != nil {
		return err
	}
	if tpl == nil {
		logger.Infof("balancealert seed: skip rule %s, template ident %s not found", spec.Name, spec.TemplateIdent)
		return nil
	}
	used, err := notifyRuleUsesChannel(n9e, channel.ID)
	if err != nil {
		return err
	}
	if used != "" {
		logger.Infof("balancealert seed: skip rule %s, channel %s already used by %s", spec.Name, spec.ChannelIdent, used)
		return nil
	}
	now := time.Now().Unix()
	rule := &models.NotifyRule{
		Name:         spec.Name,
		Enable:       true,
		UserGroupIds: []int64{},
		NotifyConfigs: []models.NotifyConfig{{
			ChannelID:  channel.ID,
			TemplateID: tpl.ID,
			Params:     map[string]interface{}{},
			Severities: []int{1, 2, 3},
		}},
		CreateAt: now,
		UpdateAt: now,
		CreateBy: "system",
		UpdateBy: "system",
	}
	if err := models.Insert(n9e, rule); err != nil {
		return err
	}
	logger.Infof("balancealert seed: created notify rule %s channel_id=%d template_id=%d", spec.Name, channel.ID, tpl.ID)
	return nil
}

func notifyRuleUsesChannel(n9e *ctx.Context, channelID int64) (string, error) {
	lst, err := models.NotifyRulesGet(n9e, "", nil)
	if err != nil {
		return "", err
	}
	for _, r := range lst {
		for _, nc := range r.NotifyConfigs {
			if nc.ChannelID == channelID {
				return r.Name, nil
			}
		}
	}
	return "", nil
}

func SeedMessageTemplates(n9e *ctx.Context) {
	for _, s := range seedTemplates {
		existing, err := models.MessageTemplateGet(n9e, "ident = ?", s.Ident)
		if err != nil {
			logger.Errorf("balancealert seed: query template %s: %v", s.Ident, err)
			continue
		}
		if existing != nil {
			if !seedContentEqual(existing.Content, s.Content) {
				contentJSON, err := json.Marshal(s.Content)
				if err != nil {
					logger.Errorf("balancealert seed: marshal content for %s: %v", s.Ident, err)
					continue
				}
				updates := map[string]interface{}{
					"content":   string(contentJSON),
					"update_at": time.Now().Unix(),
					"update_by": "system",
				}
				if err := models.DB(n9e).Model(existing).Updates(updates).Error; err != nil {
					logger.Errorf("balancealert seed: update template %s: %v", s.Ident, err)
					continue
				}
				logger.Infof("balancealert seed: updated template %s (%s)", s.Ident, s.Name)
			}
			continue
		}
		now := time.Now().Unix()
		tpl := &models.MessageTemplate{
			Name:               s.Name,
			Ident:              s.Ident,
			NotifyChannelIdent: s.NotifyChannelIdent,
			Content:            s.Content,
			Weight:             s.Weight,
			CreateAt:           now,
			UpdateAt:           now,
			CreateBy:           "system",
			UpdateBy:           "system",
		}
		if err := models.DB(n9e).Create(tpl).Error; err != nil {
			logger.Errorf("balancealert seed: create template %s: %v", s.Ident, err)
			continue
		}
		logger.Infof("balancealert seed: created template %s (%s)", s.Ident, s.Name)
	}
}

func seedContentEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
