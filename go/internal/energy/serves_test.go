package energy

import (
	"github.com/lucasfth/nocturne/go/internal/policy"
	"testing"
	"time"
)

func TestServesMatchesState(t *testing.T) {
	old := Monotonic
	Monotonic = func() float64 { return 100 }
	defer func() { Monotonic = old }()
	active, idle := true, false
	for _, hour := range []int{7, 8, 16, 17} {
		for _, schedule := range []*Schedule{nil, {TZ: "UTC", Start: "08:00", End: "17:00"}, {TZ: "UTC", Start: "17:00", End: "08:00"}} {
			for _, jobs := range []int{0, 1, 3} {
				for _, ac := range []bool{false, true} {
					for _, battery := range []float64{4, 5, 20, 21, 25, 26, 100} {
						for _, user := range []*bool{nil, &active, &idle} {
							for _, thermal := range []string{"", "nominal", "fair", "serious", "critical"} {
								for _, seen := range []float64{55, 55.001, 100} {
									for _, ready := range []bool{false, true} {
										p := policy.Worker{OnAC: ac, BatteryPct: battery, ThermalState: thermal, UserActive: user, Seen: seen, Ready: ready, Jobs: jobs}
										e := New(&Config{Schedule: schedule}, nil, func() time.Time { return time.Date(2026, 10, 15, hour, 0, 0, 0, time.UTC) })
										_, ids := e.State(map[string]Worker{"w": {Policy: p}})
										if got := e.Serves(p); got != (len(ids) == 1) {
											t.Fatalf("hour=%d schedule=%+v policy=%+v got=%v ids=%v", hour, schedule, p, got, ids)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
}
