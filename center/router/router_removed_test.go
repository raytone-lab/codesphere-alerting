package router

import "testing"

func TestIsRemovedSPA(t *testing.T) {
	blocked := []string{"/targets", "/targets/a", "/dashboards", "/metric/explorer", "/ai-config/llm-configs", "/recording-rules", "/trace/explorer", "/landing", "/home"}
	for _, p := range blocked {
		if !isRemovedSPA(p) {
			t.Errorf("want removed SPA %s", p)
		}
	}
	kept := []string{"/alert-rules", "/datasources", "/notification-rules", "/users", "/system/site-settings", "/system/balance-alert", "/components"}
	for _, p := range kept {
		if isRemovedSPA(p) {
			t.Errorf("kept SPA blocked: %s", p)
		}
	}
}

func TestIsRemovedAPI(t *testing.T) {
	blocked := []string{
		"/api/n9e/targets",
		"/api/n9e/target/extra-meta",
		"/api/n9e/boards",
		"/api/n9e/board/1",
		"/api/n9e/busi-group/1/boards",
		"/api/n9e/recording-rule/1",
		"/api/n9e/ai-llm-configs",
		"/api/n9e/embedded-product",
	}
	for _, p := range blocked {
		if !isRemovedAPI(p) {
			t.Errorf("want removed API %s", p)
		}
	}
	kept := []string{
		"/api/n9e/alert-rules/callbacks",
		"/api/n9e/busi-group/1/alert-rules",
		"/api/n9e/datasource/list",
		"/api/n9e/users",
		"/api/n9e/balance-alert/settings",
		"/api/n9e/notification-rules",
	}
	for _, p := range kept {
		if isRemovedAPI(p) {
			t.Errorf("kept API blocked: %s", p)
		}
	}
}
