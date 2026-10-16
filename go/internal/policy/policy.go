// Package policy is a port of gateway/policy.py.
//
// Battery policy: serve on AC or on battery; on battery stop at <=20% and
// only (re)start above 25% (hysteresis).
package policy

// Thresholds from policy.py.
const (
	StopPct  = 20
	StartPct = 25
)

// Worker mirrors the subset of the Python worker dict read by the policy.
// Go zero values match Python's dict.get defaults except UserActive,
// whose Python default is True (nil here means "missing").
type Worker struct {
	OnAC         bool
	BatteryPct   float64 // missing => 0
	ThermalState string  // missing => ""
	UserActive   *bool   // missing => true
	Seen         float64 // monotonic seconds; missing => 0
	Ready        bool
	Jobs         int // len(w['jobs']); 0 is falsy
}

func (w Worker) userActive() bool { return w.UserActive == nil || *w.UserActive }

// BatteryOK ports battery_ok(w, starting).
func BatteryOK(w Worker, starting bool) bool {
	if w.OnAC {
		return w.BatteryPct >= 5
	}
	if starting {
		return w.BatteryPct > StartPct
	}
	return w.BatteryPct > StopPct
}

// Eligible ports eligible(w, now); now is monotonic seconds.
func Eligible(w Worker, now float64) bool {
	return BatteryOK(w, w.Jobs == 0) &&
		(w.ThermalState == "nominal" || w.ThermalState == "fair") &&
		!w.userActive() && now-w.Seen < 45 && w.Ready
}

// Unsafe ports unsafe(w).
func Unsafe(w Worker) bool {
	return !BatteryOK(w, false) || w.userActive() ||
		w.ThermalState == "serious" || w.ThermalState == "critical"
}
