package energy

import (
	"sort"
	"time"

	"github.com/lucasfth/nocturne/go/internal/policy"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

// Staleness is the 6 h limit for using the last successful price.
const Staleness = 21600

// Worker is what State/Admit read from a worker: the policy view plus
// tokens_per_sec (nil = missing, Python default 20).
type Worker struct {
	Policy       policy.Worker
	TokensPerSec *float64
}

// Status is the dict returned by Energy.state()[0].
type Status struct {
	WindowOpen      bool
	NextWindowStart string // isoformat()
	PriceArea       string
	DKKPerKWh       *float64
	Tier            string
	EligibleWorkers int
	ServingWorkers  int
}

// Object renders Status with Python's key order (for pyjson.Marshal).
func (s Status) Object() pyjson.Object {
	var p any
	if s.DKKPerKWh != nil {
		p = *s.DKKPerKWh
	}
	return pyjson.Object{{Key: "window_open", Val: s.WindowOpen}, {Key: "next_window_start", Val: s.NextWindowStart},
		{Key: "price_area", Val: s.PriceArea}, {Key: "dkk_per_kwh", Val: p}, {Key: "tier", Val: s.Tier},
		{Key: "eligible_workers", Val: s.EligibleWorkers}, {Key: "serving_workers", Val: s.ServingWorkers}}
}

// Monotonic is time.monotonic() for policy.Eligible (overridable in tests).
var Monotonic = func() float64 { return float64(time.Since(start)) / 1e9 }

var start = time.Now().Add(-time.Hour)

// State ports Energy.state(workers): status and sorted serving worker ids.
func (e *Energy) State(workers map[string]Worker) (Status, []string) {
	ec := e.Config.Energy
	opened, next, _ := e.Window()
	e.mu.Lock()
	var price *float64
	if ec.truthy() {
		price, _ = e.currentRowPrice() // Python would raise on a malformed row
	}
	now := ts(e.Clock())
	if price == nil && e.lastSuccess != nil && now-*e.lastSuccess <= Staleness {
		price = e.lastPrice
	}
	if e.lastSuccess != nil && now-*e.lastSuccess > Staleness {
		price = nil
	}
	e.mu.Unlock()
	tier := "normal"
	if price != nil {
		var t *Tiers
		if ec != nil {
			t = ec.Tiers
		}
		if t == nil {
			t = &Tiers{}
		}
		switch {
		case *price <= or(t.CheapMax, 1):
			tier = "cheap"
		case *price <= or(t.NormalMax, 2.5):
		default:
			tier = "expensive"
		}
	}
	mono := Monotonic()
	ids := []string{}
	for id, w := range workers {
		if policy.Eligible(w.Policy, mono) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	serving := []string{}
	if opened {
		serving = ids
	}
	return Status{opened, pyjson.Isoformat(next), ec.area(), price, tier, len(ids), len(serving)}, serving
}

// Serves answers the same per-worker membership decision as State without
// materializing/sorting the fleet or computing unrelated price/admin output.
// Use the same energy monotonic clock as State (not the coordinator clock).
func (e *Energy) Serves(w policy.Worker) bool {
	opened, _, _ := e.Window()
	return opened && policy.Eligible(w, Monotonic())
}
