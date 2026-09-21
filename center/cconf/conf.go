package cconf

import (
	"time"

	"github.com/ccfos/nightingale/v6/pkg/httpx"
)

type Center struct {
	Plugins                []Plugin
	MetricsYamlFile        string
	OpsYamlFile            string
	BuiltinIntegrationsDir string
	// AgentsDir holds the bundled collector packages (categraf tar.gz) served
	// by /api/n9e/agents/categraf/download. Relative paths resolve against the
	// working directory.
	AgentsDir                 string
	I18NHeaderKey             string
	MetricDesc                MetricDescType
	AnonymousAccess           AnonymousAccess
	UseFileAssets             bool
	FlashDuty                 FlashDuty
	EventHistoryGroupView     bool
	CleanNotifyRecordDay      int
	CleanPipelineExecutionDay int
	// CleanAlertHisEventDay 历史告警事件保留天数，<= 0 表示永久保留不清理
	CleanAlertHisEventDay int
	MigrateBusiGroupLabel bool
	RSA                   httpx.RSAConfig
}

type Plugin struct {
	Id       int64  `json:"id"`
	Category string `json:"category"`
	Type     string `json:"plugin_type"`
	TypeName string `json:"plugin_type_name"`
}

type FlashDuty struct {
	Api     string
	Headers map[string]string
	Timeout time.Duration
}

type AnonymousAccess struct {
	PromQuerier bool
	AlertDetail bool
}

func (c *Center) PreCheck() {
	if len(c.Plugins) == 0 {
		c.Plugins = Plugins
	}
	if c.AgentsDir == "" {
		// 默认使用项目根路径下的 agents/categraf 目录（与 integrations 同级）
		c.AgentsDir = "agents/categraf"
	}
}
