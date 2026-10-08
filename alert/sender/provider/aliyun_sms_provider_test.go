package provider

import (
	"strings"
	"testing"
)

func TestAliyunSMSBizError(t *testing.T) {
	if err := aliyunSMSBizError([]byte(`{"Code":"OK","Message":"OK"}`)); err != nil {
		t.Fatalf("OK: %v", err)
	}
	if err := aliyunSMSBizError([]byte(`not json`)); err != nil {
		t.Fatalf("non-json should not fail send: %v", err)
	}
	err := aliyunSMSBizError([]byte(`{"Code":"isv.SMS_TEMPLATE_ILLEGAL","Message":"模板不合法"}`))
	if err == nil || !strings.Contains(err.Error(), "SMS_TEMPLATE_ILLEGAL") {
		t.Fatalf("want biz error, got %v", err)
	}
}
