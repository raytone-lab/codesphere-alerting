package balancealert

import (
	"testing"

	"github.com/ccfos/nightingale/v6/models"
)

func TestNotifyRuleIDFor(t *testing.T) {
	s := models.BalanceAlertSettings{
		SendMode:             SendModeOff,
		PilotNotifyRuleID:    11,
		CustomerNotifyRuleID: 22,
	}
	if got := notifyRuleIDFor(s); got != 0 {
		t.Fatalf("OFF: %d", got)
	}
	s.SendMode = SendModePilot
	if got := notifyRuleIDFor(s); got != 11 {
		t.Fatalf("PILOT: %d", got)
	}
	s.SendMode = SendModeCustomer
	if got := notifyRuleIDFor(s); got != 22 {
		t.Fatalf("CUSTOMER: %d", got)
	}
}

func TestBuildSyntheticEvent(t *testing.T) {
	ev := buildSyntheticEvent(SendRequest{
		Mode: SendModePilot, Level: StateCritical, ThresholdMode: ModeVoucherFixed,
		AccountID: "ba1", Name: "数商云", Balance: -0.04, Threshold: 20, Phone: "138",
	})
	if ev.Severity != 1 || ev.AnnotationsJSON["level"] != StateCritical {
		t.Fatalf("event: sev=%d ann=%v", ev.Severity, ev.AnnotationsJSON)
	}
	if ev.TagsMap["product"] != "balance_alert" {
		t.Fatalf("tags: %v", ev.TagsMap)
	}
	if ev.AnnotationsJSON["pilot_line"] == "" || ev.AnnotationsJSON["copy"] == "" {
		t.Fatalf("missing rendered copy fields: %v", ev.AnnotationsJSON)
	}
}
