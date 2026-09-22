package cconf

import (
	"fmt"
	"path"

	"github.com/toolkits/pkg/file"
	"gopkg.in/yaml.v2"
)

var Operations = Operation{}

type Operation struct {
	Ops []Ops `yaml:"ops"`
}

type Ops struct {
	Name  string     `yaml:"name" json:"name"`
	Cname string     `yaml:"cname" json:"cname"`
	Ops   []SingleOp `yaml:"ops" json:"ops"`
}

// SingleOp Name 为 op 名称；Cname 为展示名称，默认英文
type SingleOp struct {
	Name  string `yaml:"name" json:"name"`
	Cname string `yaml:"cname" json:"cname"`
}

func TransformNames(name []string, nameToName map[string]string) []string {
	var ret []string
	for _, n := range name {
		if v, has := nameToName[n]; has {
			ret = append(ret, v)
		}
	}
	return ret
}

func LoadOpsYaml(configDir string, opsYamlFile string) error {
	fp := opsYamlFile
	if fp == "" {
		fp = path.Join(configDir, "ops.yaml")
	}
	if !file.IsExist(fp) {
		return nil
	}

	hash, _ := file.MD5(fp)
	if hash == "2f91a9ed265cf2024e266dc1d538ee77" {
		// ops.yaml 是老的默认文件，删除
		file.Remove(fp)
		return nil
	}

	return file.ReadYaml(fp, &Operations)
}

func GetAllOps(ops []Ops) []SingleOp {
	var ret []SingleOp
	for _, op := range ops {
		ret = append(ret, op.Ops...)
	}
	return ret
}

func MergeOperationConf() error {
	var opsBuiltIn Operation
	err := yaml.Unmarshal([]byte(builtInOps), &opsBuiltIn)
	if err != nil {
		return fmt.Errorf("cannot parse builtInOps: %s", err.Error())
	}
	configOpsMap := make(map[string]struct{})
	for _, op := range Operations.Ops {
		configOpsMap[op.Name] = struct{}{}
	}
	//If the opBu.Name is not a constant in the target (Operations.Ops), add Ops from the built-in options
	for _, opBu := range opsBuiltIn.Ops {
		if _, has := configOpsMap[opBu.Name]; !has {
			Operations.Ops = append(Operations.Ops, opBu)
		}
	}
	return nil
}

const (
	builtInOps = `
ops:
- name: balance-alert
  cname: 预付费余额预警
  ops:
    - name: /system/balance-alert
      cname: 预付费余额预警
    - name: /system/enterprise-balance
      cname: 企业余额预警配置

- name: alerting
  cname: Alerting
  ops:
    - name: /alert-rules
      cname: Alerting Rule - View
    - name: /alert-rules/add
      cname: Alerting Rule - Add
    - name: /alert-rules/put
      cname: Alerting Rule - Modify
    - name: /alert-rules/del
      cname: Alerting Rule - Delete
    - name: /alert-mutes
      cname: Mutting Rule - View
    - name: /alert-mutes/add
      cname: Mutting Rule - Add
    - name: /alert-mutes/put
      cname: Mutting Rule - Modify
    - name: /alert-mutes/del
      cname: Mutting Rule - Delete
    - name: /alert-subscribes
      cname: Subscribing Rule - View
    - name: /alert-subscribes/add
      cname: Subscribing Rule - Add
    - name: /alert-subscribes/put
      cname: Subscribing Rule - Modify
    - name: /alert-subscribes/del
      cname: Subscribing Rule - Delete
    - name: /job-tpls
      cname: Self-healing-Script - View
    - name: /job-tpls/add
      cname: Self-healing-Script - Add
    - name: /job-tpls/put
      cname: Self-healing-Script - Modify
    - name: /job-tpls/del
      cname: Self-healing-Script - Delete
    - name: /job-tasks
      cname: Self-healing-Job - View
    - name: /job-tasks/add
      cname: Self-healing-Job - Add
    - name: /job-tasks/put
      cname: Self-healing-Job - Modify
    - name: /alert-cur-events
      cname: Active Event - View
    - name: /alert-cur-events/del
      cname: Active Event - Delete
    - name: /alert-his-events
      cname: Historical Event - View

- name: Notification
  cname: Notification
  ops:
    - name: /notification-rules
      cname: Notification Rule - View
    - name: /notification-rules/add
      cname: Notification Rule - Add
    - name: /notification-rules/put
      cname: Notification Rule - Modify
    - name: /notification-rules/del
      cname: Notification Rule - Delete
    - name: /notification-channels
      cname: Media Type - View
    - name: /notification-channels/add
      cname: Media Type - Add
    - name: /notification-channels/put
      cname: Media Type - Modify
    - name: /notification-channels/del
      cname: Media Type - Delete
    - name: /notification-templates
      cname: Message Template - View
    - name: /notification-templates/add
      cname: Message Template - Add
    - name: /notification-templates/put
      cname: Message Template - Modify
    - name: /notification-templates/del
      cname: Message Template - Delete
    - name: /event-pipelines
      cname: Event Pipeline - View
    - name: /event-pipelines/add
      cname: Event Pipeline - Add
    - name: /event-pipelines/put
      cname: Event Pipeline - Modify
    - name: /event-pipelines/del
      cname: Event Pipeline - Delete
    - name: /help/notification-settings # 用于控制老版本的通知设置菜单是否展示
      cname: Notification Settings - View
    - name: /help/notification-tpls # 用于控制老版本的通知模板菜单是否展示
      cname: Notification Templates - View

- name: Integrations
  cname: Integrations
  ops:
    - name: /datasources # 用于控制能否看到数据源列表页面的菜单。只有 Admin 才能修改、删除数据源
      cname: Data Source - View
    - name: /help/source # 兼容当前嵌入前端的数据源菜单 key
      cname: Data Source - View (legacy menu)
    - name: /components
      cname: Component - View
    - name: /built-in-components # 兼容当前嵌入前端的模板中心菜单 key
      cname: Component - View (legacy menu)
    - name: /components/add
      cname: Component - Add
    - name: /components/put
      cname: Component - Modify
    - name: /components/del
      cname: Component - Delete

- name: Organization
  cname: Organization
  ops:
    - name: /users
      cname: User - View
    - name: /users/add
      cname: User - Add
    - name: /users/put
      cname: User - Modify
    - name: /users/del
      cname: User - Delete
    - name: /user-groups
      cname: Team - View
    - name: /user-groups/add
      cname: Team - Add
    - name: /user-groups/put
      cname: Team - Modify
    - name: /user-groups/del
      cname: Team - Delete
    - name: /busi-groups
      cname: Business Group - View
    - name: /busi-groups/add
      cname: Business Group - Add
    - name: /busi-groups/put
      cname: Business Group - Modify
    - name: /busi-groups/del
      cname: Business Group - Delete
    - name: /roles
      cname: Role - View
    - name: /roles/add
      cname: Role - Add
    - name: /roles/put
      cname: Role - Modify
    - name: /roles/del
      cname: Role - Delete
    - name: /permissions # 兼容当前嵌入前端的权限管理菜单 key
      cname: Role - View (legacy menu)
    - name: /contacts # 兼容当前嵌入前端的联系方式菜单 key
      cname: Contact - View

- name: System Settings
  cname: System Settings
  ops:
    - name: /system/site-settings # 仅用于控制能否展示菜单，只有 Admin 才能修改、删除
      cname: View Site Settings
    - name: /site-settings # 兼容当前嵌入前端的站点设置菜单 key
      cname: View Site Settings (legacy menu)
    - name: /system/variable-settings
      cname: View Variable Settings
    - name: /help/variable-configs # 兼容当前嵌入前端的变量配置菜单 key
      cname: View Variable Settings (legacy menu)
    - name: /system/sso-settings
      cname: View SSO Settings
    - name: /help/sso # 兼容当前嵌入前端的单点登录菜单 key
      cname: View SSO Settings (legacy menu)
    - name: /system/alerting-engines
      cname: View Alerting Engines
    - name: /help/servers # 兼容当前嵌入前端的告警引擎菜单 key
      cname: View Alerting Engines (legacy menu)

`
)
