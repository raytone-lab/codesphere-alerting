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
		Content: map[string]string{
			"content": `【签名】{{ index $event.AnnotationsJSON "enterprise_name" }}您好，您的账户余额为{{ index $event.AnnotationsJSON "balance" }}元，为避免影响业务调用请及时充值。`,
		},
	},
	{
		Name:               "余额预警-短信-CRITICAL",
		Ident:              "balance-alert-sms-critical",
		NotifyChannelIdent: "ali-sms",
		Weight:             0,
		Content: map[string]string{
			"content": `【签名】{{ index $event.AnnotationsJSON "enterprise_name" }}您好，您的账户余额已耗尽/即将耗尽，服务即将暂停，请立即充值恢复。`,
		},
	},
	{
		Name:               "余额预警-短信-体验额度",
		Ident:              "balance-alert-sms-voucher",
		NotifyChannelIdent: "ali-sms",
		Weight:             0,
		Content: map[string]string{
			"content": `【签名】{{ index $event.AnnotationsJSON "enterprise_name" }}您好，您的体验额度即将用完，充值后可继续使用服务。`,
		},
	},
	{
		Name:               "余额预警-短信-动态预警",
		Ident:              "balance-alert-dynamic-sms",
		NotifyChannelIdent: "ali-sms",
		Weight:             0,
		Content: map[string]string{
			"content": `【签名】{{ index $event.AnnotationsJSON "enterprise_name" }}您好，按当前消耗速度，您的账户余额仅剩{{ index $event.AnnotationsJSON "remaining_days" }}，请及时充值以避免服务中断。`,
		},
	},
}

func SeedMessageTemplates(n9e *ctx.Context) {
	for _, s := range seedTemplates {
		existing, err := models.MessageTemplateGet(n9e, "ident = ?", s.Ident)
		if err != nil {
			logger.Errorf("balancealert seed: query template %s: %v", s.Ident, err)
			continue
		}
		if existing != nil {
			if existing.Content["content"] != s.Content["content"] {
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
