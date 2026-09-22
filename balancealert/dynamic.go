package balancealert

import (
	"fmt"
	"math"
)

const (
	DynamicDaysThreshold = 7.0
	SurgeMultiplier      = 3.0
	SurgeMinAmountUSD    = 10.0
)

// Consumption holds consumption data for an account over 7-day and 3-day windows.
type Consumption struct {
	AccountID string
	Amount7D  float64
	Amount3D  float64
}

// RemainingDays calculates how many days the balance can sustain at the current consumption rate.
// Returns math.Inf(1) if no consumption, 0 if balance <= 0.
func RemainingDays(balance float64, c *Consumption) float64 {
	if balance <= 0 || c == nil {
		return 0
	}
	d7 := c.Amount7D / 7.0
	d3 := c.Amount3D / 3.0
	d := math.Max(d3, d7)
	if d == 0 {
		return math.Inf(1)
	}
	return balance / d
}

// DynamicResult holds the result of dynamic alert evaluation.
type DynamicResult struct {
	ShouldAlert  bool
	Days         float64
	TriggerType  string
}

// EvaluateDynamic implements PRD 10.2: dynamic alert based on consumption velocity.
// Returns ShouldAlert=true if remaining days < 7.
func EvaluateDynamic(balance float64, c *Consumption) DynamicResult {
	if balance <= 0 || c == nil {
		return DynamicResult{ShouldAlert: false, Days: 0}
	}
	days := RemainingDays(balance, c)
	if days < DynamicDaysThreshold {
		return DynamicResult{
			ShouldAlert: true,
			Days:        days,
			TriggerType: "DYNAMIC",
		}
	}
	return DynamicResult{ShouldAlert: false, Days: days}
}

// EvaluateSurge implements PRD 10.3: detect abnormal consumption spike.
// Returns true if today's consumption > 3x the 7-day daily average AND > $10.
func EvaluateSurge(todayAmount float64, c *Consumption) bool {
	if c == nil || c.Amount7D == 0 {
		return false
	}
	d7 := c.Amount7D / 7.0
	return todayAmount > SurgeMultiplier*d7 && todayAmount > SurgeMinAmountUSD
}

// FormatRemainingDays formats remaining days for display.
func FormatRemainingDays(days float64) string {
	if math.IsInf(days, 1) {
		return "无限"
	}
	if days < 1 {
		return "不足1天"
	}
	return fmt.Sprintf("%.1f天", days)
}
