package router

import (
	"reflect"
	"testing"
)

func TestFilterSidebarPerms(t *testing.T) {
	in := []string{"/alert-rules", "/dashboards", "/system/balance-alert", "/embedded-products"}
	got := filterSidebarPerms(in)
	want := []string{"/alert-rules", "/system/balance-alert"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestEnsureMyBalanceAlertPerm(t *testing.T) {
	got := ensureMyBalanceAlertPerm([]string{"/system/balance-alert"})
	if len(got) != 2 || got[1] != myBalanceAlertMenuPerm {
		t.Fatalf("missing inject: %v", got)
	}
	again := ensureMyBalanceAlertPerm(got)
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("duplicate inject: %v", again)
	}
}
