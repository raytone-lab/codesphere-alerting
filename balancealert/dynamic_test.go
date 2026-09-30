package balancealert

import (
	"testing"
	"time"
)

func TestRemainingDaysUsesAccountAge(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, locShanghai)
	opened := now.Add(-24 * time.Hour) // ~2 calendar days
	c := &Consumption{Amount7D: 20, Amount3D: 20}
	// 2-day-old: d7=20/2=10, d3=20/2=10, days=100/10=10
	got := RemainingDays(100, c, opened, now)
	if got < 9.9 || got > 10.1 {
		t.Fatalf("young account remaining days: %v", got)
	}
	// old account: d7=20/7, d3=20/3, max is 20/3, 100/(20/3)=15
	got = RemainingDays(100, c, time.Time{}, now)
	if got < 14.9 || got > 15.1 {
		t.Fatalf("old account remaining days: %v", got)
	}
}

func TestEvaluateSurge(t *testing.T) {
	c := &Consumption{Amount7D: 70} // daily avg 10
	if EvaluateSurge(20, c) {
		t.Fatal("20 is not > 3x10")
	}
	if !EvaluateSurge(40, c) {
		t.Fatal("40 > 30 and > 10 should surge")
	}
}
