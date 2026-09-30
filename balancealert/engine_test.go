package balancealert

import (
	"testing"
	"time"
)

func TestComputeThreshold(t *testing.T) {
	recharge := 1000.0
	th, mode, skip := ComputeThreshold(&recharge, true, 20)
	if skip || mode != ModeLastRecharge5Pct || th != 50 {
		t.Fatalf("recharge: th=%v mode=%s skip=%v", th, mode, skip)
	}

	th, mode, skip = ComputeThreshold(nil, true, 20)
	if skip || mode != ModeVoucherFixed || th != 20 {
		t.Fatalf("voucher: th=%v mode=%s skip=%v", th, mode, skip)
	}

	_, _, skip = ComputeThreshold(nil, false, 20)
	if !skip {
		t.Fatal("no recharge and no voucher should skip")
	}
}

func TestDesiredLevel(t *testing.T) {
	if DesiredLevel(-0.04, 20) != StateCritical {
		t.Fatal("balance <= 0 must be CRITICAL")
	}
	if DesiredLevel(9, 20) != StateCritical {
		t.Fatal("below half threshold must be CRITICAL")
	}
	if DesiredLevel(15, 20) != StateWarn {
		t.Fatal("below threshold must be WARN")
	}
	if DesiredLevel(20, 20) != StateNormal {
		t.Fatal("at threshold is not WARN")
	}
}

func TestDecideHysteresisAndRateLimit(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai)
	th := 20.0

	d := Decide(StateNormal, 15, th, now, SendHistory{}, 0)
	if d.NextState != StateWarn || d.SendLevel != StateWarn || d.Status != StatusSent {
		t.Fatalf("NORMAL->WARN send: %+v", d)
	}

	d = Decide(StateWarn, 16, th, now, SendHistory{WarnAt: now}, 0)
	if d.NextState != StateWarn || d.SendLevel != "" {
		t.Fatalf("stay WARN no resend: %+v", d)
	}

	d = Decide(StateWarn, 9, th, now, SendHistory{WarnAt: now}, 0)
	if d.NextState != StateCritical || d.SendLevel != StateCritical {
		t.Fatalf("WARN->CRITICAL upgrade: %+v", d)
	}

	d = Decide(StateWarn, 15, th, now, SendHistory{WarnAt: now}, 0)
	if d.Status == StatusSent {
		t.Fatalf("same-day WARN must not send again: %+v", d)
	}

	d = Decide(StateCritical, 9, th, now.Add(24*time.Hour), SendHistory{CriticalAt: now}, 0)
	if d.SendLevel != "" {
		t.Fatalf("CRITICAL day+1 should not repeat: %+v", d)
	}

	d = Decide(StateCritical, 9, th, now.Add(72*time.Hour), SendHistory{CriticalAt: now}, 0)
	if d.SendLevel != StateCritical || d.Status != StatusSent {
		t.Fatalf("CRITICAL after 3 days should repeat: %+v", d)
	}

	d = Decide(StateCritical, 9, th, now.Add(48*time.Hour), SendHistory{CriticalAt: now}, 24*time.Hour)
	if d.SendLevel != StateCritical || d.Status != StatusSent {
		t.Fatalf("CRITICAL after configured 1 day should repeat: %+v", d)
	}

	d = Decide(StateCritical, 9, th, now.Add(48*time.Hour), SendHistory{CriticalAt: now}, 5*24*time.Hour)
	if d.SendLevel != "" {
		t.Fatalf("CRITICAL before configured 5 days must not repeat: %+v", d)
	}

	d = Decide(StateCritical, 31, th, now, SendHistory{CriticalAt: now}, 0)
	if d.NextState != StateNormal {
		t.Fatalf("hysteresis 1.5x recovers: %+v", d)
	}

	d = Decide(StateCritical, 25, th, now, SendHistory{CriticalAt: now}, 0)
	if d.NextState != StateCritical {
		t.Fatalf("between threshold and 1.5x stays CRITICAL: %+v", d)
	}
}

func TestDesiredLevelCustomWarnKeepsPlatformCritical(t *testing.T) {
	// platform 20 → CRITICAL below 10; custom WARN line 100.
	if DesiredLevelWith(8, 100, 20) != StateCritical {
		t.Fatal("below half of platform line must stay CRITICAL")
	}
	if DesiredLevelWith(50, 100, 20) != StateWarn {
		t.Fatal("below custom WARN line, above platform CRITICAL, is WARN")
	}
	if DesiredLevelWith(100, 100, 20) != StateNormal {
		t.Fatal("at custom WARN line is not WARN")
	}
}

func TestDecideCustomWarnDoesNotSendWhenDisabled(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai)
	d := DecideWith(StateNormal, 50, 100, 20, now, SendHistory{}, 0, false)
	if d.NextState != StateWarn || d.SendLevel != "" {
		t.Fatalf("WARN off should track state without send: %+v", d)
	}
	d = DecideWith(StateNormal, 8, 100, 20, now, SendHistory{}, 0, false)
	if d.SendLevel != StateCritical {
		t.Fatalf("CRITICAL must still send when WARN is off: %+v", d)
	}
}
