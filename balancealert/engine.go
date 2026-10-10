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
	DefaultLastRechargePct  = 5
	DefaultCriticalRepeat   = 72 * time.Hour
)

var locShanghai = mustLoadShanghai()

func mustLoadShanghai() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

func lastRechargeRatio(pct float64) float64 {
	if pct <= 0 || pct > 100 {
		pct = DefaultLastRechargePct
	}
	return pct / 100
}

// ComputeThreshold implements FR-02. skip=true means no recharge and no voucher line.
func ComputeThreshold(lastRechargeUSD *float64, hasVoucher bool, voucherThresholdUSD, lastRechargePct float64) (threshold float64, mode string, skip bool) {
	if lastRechargeUSD != nil {
		return *lastRechargeUSD * lastRechargeRatio(lastRechargePct), ModeLastRecharge5Pct, false
	}
	if hasVoucher && voucherThresholdUSD > 0 {
		return voucherThresholdUSD, ModeVoucherFixed, false
	}
	return 0, "", true
}

func startOfShanghaiDay(now time.Time) time.Time {
	y, m, d := now.In(locShanghai).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, locShanghai)
}

func DesiredLevel(balance, threshold float64) string {
	return DesiredLevelWith(balance, threshold, threshold)
}

// DesiredLevelWith splits WARN and CRITICAL lines. PRD 10.1: custom 警戒线
// only affects WARN; CRITICAL always uses the platform default line (half / <= 0).
func DesiredLevelWith(balance, warnTh, platformTh float64) string {
	if platformTh <= 0 {
		platformTh = warnTh
	}
	if warnTh <= 0 {
		warnTh = platformTh
	}
	if balance <= 0 || (platformTh > 0 && balance < platformTh*CriticalHalfFactor) {
		return StateCritical
	}
	if warnTh > 0 && balance < warnTh {
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

// Decide implements FR-03 debounce: hysteresis, daily cap, CRITICAL repeat.
// repeatAfter is the CRITICAL re-notify interval; zero uses DefaultCriticalRepeat (3 days).
func Decide(current string, balance, threshold float64, now time.Time, hist SendHistory, repeatAfter time.Duration) Decision {
	return DecideWith(current, balance, threshold, threshold, now, hist, repeatAfter, true)
}

// DecideWith is Decide plus PRD 10.1: warnTh may be a higher enterprise line;
// platformTh is always the auto line used for CRITICAL. warnEnabled=false
// suppresses WARN sends but never CRITICAL.
func DecideWith(current string, balance, warnTh, platformTh float64, now time.Time, hist SendHistory, repeatAfter time.Duration, warnEnabled bool) Decision {
	if repeatAfter <= 0 {
		repeatAfter = DefaultCriticalRepeat
	}
	if current == "" {
		current = StateNormal
	}
	if platformTh <= 0 {
		platformTh = warnTh
	}
	if warnTh <= 0 {
		warnTh = platformTh
	}
	if warnTh <= 0 && platformTh <= 0 {
		return Decision{NextState: current}
	}

	recoverTh := warnTh
	if recoverTh <= 0 {
		recoverTh = platformTh
	}
	if recoverTh > 0 && balance >= recoverTh*HysteresisFactor {
		return Decision{NextState: StateNormal}
	}

	desired := DesiredLevelWith(balance, warnTh, platformTh)

	switch current {
	case StateNormal:
		if desired == StateNormal {
			return Decision{NextState: StateNormal}
		}
		if desired == StateWarn && !warnEnabled {
			return Decision{NextState: StateWarn}
		}
		return sendOrSkip(desired, now, hist)
	case StateWarn:
		if desired == StateCritical {
			return sendOrSkip(StateCritical, now, hist)
		}
		return Decision{NextState: StateWarn}
	default:
		if desired == StateCritical || current == StateCritical {
			if canSendToday(hist.CriticalAt, now) && (hist.CriticalAt.IsZero() || now.Sub(hist.CriticalAt) >= repeatAfter) {
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
