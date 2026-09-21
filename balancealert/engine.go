package balancealert

import "time"

const (
	StateNormal   = "NORMAL"
	StateWarn     = "WARN"
	StateCritical = "CRITICAL"

	ModeLastRecharge5Pct = "LAST_RECHARGE_5PCT"
	ModeVoucherFixed     = "VOUCHER_FIXED"

	SendModeOff      = "OFF"
	SendModePilot    = "PILOT"
	SendModeCustomer = "CUSTOMER"

	StatusSent            = "SENT"
	StatusSkippedCooldown = "SKIPPED_COOLDOWN"
	StatusFailed          = "FAILED"

	DefaultVoucherThreshold = 20.0
	HysteresisFactor        = 1.5
	CriticalHalfFactor      = 0.5
	LastRechargePct         = 0.05
	CriticalRepeatAfter     = 72 * time.Hour
)

var locShanghai = mustLoadShanghai()

func mustLoadShanghai() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

// ComputeThreshold implements FR-02. skip=true means no recharge and no voucher line.
func ComputeThreshold(lastRechargeUSD *float64, hasVoucher bool, voucherThresholdUSD float64) (threshold float64, mode string, skip bool) {
	if lastRechargeUSD != nil {
		return *lastRechargeUSD * LastRechargePct, ModeLastRecharge5Pct, false
	}
	if hasVoucher && voucherThresholdUSD > 0 {
		return voucherThresholdUSD, ModeVoucherFixed, false
	}
	return 0, "", true
}

func DesiredLevel(balance, threshold float64) string {
	if balance <= 0 || balance < threshold*CriticalHalfFactor {
		return StateCritical
	}
	if balance < threshold {
		return StateWarn
	}
	return StateNormal
}

type SendHistory struct {
	WarnAt     time.Time
	CriticalAt time.Time
}

type Decision struct {
	NextState string
	SendLevel string
	Status    string
}

func sameCivilDay(a, b time.Time) bool {
	a = a.In(locShanghai)
	b = b.In(locShanghai)
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func canSendToday(last time.Time, now time.Time) bool {
	if last.IsZero() {
		return true
	}
	return !sameCivilDay(last, now)
}

// Decide implements FR-03 debounce: hysteresis, daily cap, CRITICAL 3-day repeat.
func Decide(current string, balance, threshold float64, now time.Time, hist SendHistory) Decision {
	if current == "" {
		current = StateNormal
	}
	if threshold <= 0 {
		return Decision{NextState: current}
	}

	if balance >= threshold*HysteresisFactor {
		return Decision{NextState: StateNormal}
	}

	desired := DesiredLevel(balance, threshold)

	switch current {
	case StateNormal:
		if desired == StateNormal {
			return Decision{NextState: StateNormal}
		}
		return sendOrSkip(desired, now, hist)
	case StateWarn:
		if desired == StateCritical {
			return sendOrSkip(StateCritical, now, hist)
		}
		return Decision{NextState: StateWarn}
	default:
		if desired == StateCritical || current == StateCritical {
			if canSendToday(hist.CriticalAt, now) && (hist.CriticalAt.IsZero() || now.Sub(hist.CriticalAt) >= CriticalRepeatAfter) {
				return Decision{NextState: StateCritical, SendLevel: StateCritical, Status: StatusSent}
			}
			if !hist.CriticalAt.IsZero() && !canSendToday(hist.CriticalAt, now) {
				return Decision{NextState: StateCritical, Status: StatusSkippedCooldown}
			}
			return Decision{NextState: StateCritical}
		}
		return Decision{NextState: StateCritical}
	}
}

func sendOrSkip(level string, now time.Time, hist SendHistory) Decision {
	last := hist.WarnAt
	if level == StateCritical {
		last = hist.CriticalAt
	}
	if canSendToday(last, now) {
		return Decision{NextState: level, SendLevel: level, Status: StatusSent}
	}
	return Decision{NextState: level, Status: StatusSkippedCooldown}
}
