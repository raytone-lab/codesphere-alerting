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

	d := Decide(StateNormal, 15, th, now, SendHistory{})
	if d.NextState != StateWarn || d.SendLevel != StateWarn || d.Status != StatusSent {
		t.Fatalf("NORMAL->WARN send: %+v", d)
	}

	d = Decide(StateWarn, 16, th, now, SendHistory{WarnAt: now})
	if d.NextState != StateWarn || d.SendLevel != "" {
		t.Fatalf("stay WARN no resend: %+v", d)
	}

	d = Decide(StateWarn, 9, th, now, SendHistory{WarnAt: now})
	if d.NextState != StateCritical || d.SendLevel != StateCritical {
		t.Fatalf("WARN->CRITICAL upgrade: %+v", d)
	}

	d = Decide(StateWarn, 15, th, now, SendHistory{WarnAt: now})
	if d.Status == StatusSent {
		t.Fatalf("same-day WARN must not send again: %+v", d)
	}

	d = Decide(StateCritical, 9, th, now.Add(24*time.Hour), SendHistory{CriticalAt: now})
	if d.SendLevel != "" {
		t.Fatalf("CRITICAL day+1 should not repeat: %+v", d)
	}

	d = Decide(StateCritical, 9, th, now.Add(72*time.Hour), SendHistory{CriticalAt: now})
	if d.SendLevel != StateCritical || d.Status != StatusSent {
		t.Fatalf("CRITICAL after 3 days should repeat: %+v", d)
	}

	d = Decide(StateCritical, 31, th, now, SendHistory{CriticalAt: now})
	if d.NextState != StateNormal {
		t.Fatalf("hysteresis 1.5x recovers: %+v", d)
	}

	d = Decide(StateCritical, 25, th, now, SendHistory{CriticalAt: now})
	if d.NextState != StateCritical {
		t.Fatalf("between threshold and 1.5x stays CRITICAL: %+v", d)
	}
}
