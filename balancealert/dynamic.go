package balancealert

import (
	"fmt"
	"math"
	"time"
)

const (
	DynamicDaysThreshold = 7.0
	SurgeMultiplier      = 3.0
	SurgeMinAmountUSD    = 10.0
)

// Consumption holds consumption data for an account over 7-day and 3-day windows.
type Consumption struct {
	AccountID   string
	Amount7D    float64
	Amount3D    float64
	AmountToday float64
}

func consumptionWindowDays(opened, now time.Time, window int) float64 {
	if window <= 0 {
		return 1
	}
	if opened.IsZero() {
		return float64(window)
	}
	age := int(now.In(locShanghai).Sub(opened.In(locShanghai)).Hours()/24) + 1
	if age < 1 {
		age = 1
	}
	if age > window {
		return float64(window)
	}
	return float64(age)
}

// RemainingDays calculates how many days the balance can sustain at the current consumption rate.
// Accounts younger than 7/3 days divide by actual age (PRD 10.2).
func RemainingDays(balance float64, c *Consumption, openedAt, now time.Time) float64 {
	if balance <= 0 || c == nil {
		return 0
	}
	d7 := c.Amount7D / consumptionWindowDays(openedAt, now, 7)
	d3 := c.Amount3D / consumptionWindowDays(openedAt, now, 3)
	d := math.Max(d3, d7)
	if d == 0 {
		return math.Inf(1)
	}
	return balance / d
}

type DynamicResult struct {
	ShouldAlert bool
	Days        float64
	TriggerType string
}

func EvaluateDynamic(balance float64, c *Consumption, openedAt, now time.Time) DynamicResult {
	if balance <= 0 || c == nil {
		return DynamicResult{ShouldAlert: false, Days: 0}
	}
	days := RemainingDays(balance, c, openedAt, now)
	if days < DynamicDaysThreshold {
		return DynamicResult{
			ShouldAlert: true,
			Days:        days,
			TriggerType: "DYNAMIC",
		}
	}
	return DynamicResult{ShouldAlert: false, Days: days}
}

func EvaluateSurge(todayAmount float64, c *Consumption) bool {
	if c == nil || c.Amount7D == 0 {
		return false
	}
	d7 := c.Amount7D / 7.0
	return todayAmount > SurgeMultiplier*d7 && todayAmount > SurgeMinAmountUSD
}

func FormatRemainingDays(days float64) string {
	if math.IsInf(days, 1) {
		return "无限"
	}
	if days < 1 {
		return "不足1天"
	}
	return fmt.Sprintf("%.1f天", days)
}
