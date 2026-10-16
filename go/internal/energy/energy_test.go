package energy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lucasfth/nocturne/go/internal/policy"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

func f(v float64) *float64 { return &v }
func s(v string) *string   { return &v }

func cfg() *Config {
	return &Config{
		Schedule: &Schedule{TZ: "Europe/Copenhagen", Start: "17:30", End: "08:00", GraceMin: f(20)},
		Energy: &EnergyConfig{PriceArea: s("DK2"), RefreshMin: f(30), WattsPerWorker: f(30), TariffDKKPerKWh: f(1.2),
			Tiers: &Tiers{CheapMax: f(1), NormalMax: f(2.5)}},
		DKKPerBTC: 100000,
	}
}

// fixture mirrors energy(c, hour, price) from tests/test_energy.py.
func fixture(c *Config, hour int, price any) (*Energy, time.Time) {
	now := time.Date(2026, 10, 10, hour, 0, 0, 0, time.UTC)
	fake := func(context.Context, string) ([]Row, error) {
		return []Row{{Time: pyjson.Isoformat(now), Spot: price, Dataset: "Elspotprices"}}, nil
	}
	return New(c, fake, func() time.Time { return now }), now
}

func TestClosed503RetryAfter(t *testing.T) {
	e, _ := fixture(cfg(), 10, int64(500))
	e.Refresh(context.Background())
	err := e.Admit(nil, 1000, 10, 10, false)
	if err == nil || err.Status != 503 || err.Headers["Retry-After"] != "19800" {
		t.Fatalf("got %+v", err)
	}
	if st, serving := e.State(map[string]Worker{"w": okWorker()}); st.WindowOpen || len(serving) != 0 {
		t.Fatal("closed window must not serve")
	}
}

func TestExpensiveRefusal(t *testing.T) {
	e, _ := fixture(cfg(), 19, int64(1000000))
	e.Refresh(context.Background())
	if err := e.Admit(nil, 1000, 10, 10, false); err == nil || err.Headers["X-Reason"] != "energy-price" {
		t.Fatalf("got %+v", err)
	}
}

func okWorker() Worker {
	no := false
	return Worker{Policy: policy.Worker{OnAC: true, BatteryPct: 90, ThermalState: "nominal", UserActive: &no, Seen: Monotonic(), Ready: true}}
}

func TestNormalCap(t *testing.T) {
	e, _ := fixture(cfg(), 19, int64(500))
	ws := map[string]Worker{}
	for _, id := range []string{"0", "1", "2", "3", "4"} {
		ws[id] = okWorker()
	}
	e.Refresh(context.Background())
	st, serving := e.State(ws)
	if st.Tier != "normal" || st.ServingWorkers != 5 || len(serving) != 5 {
		t.Fatalf("%+v %v", st, serving)
	}
}

func TestProviderFailureFallback(t *testing.T) {
	e, now := fixture(cfg(), 19, int64(2000))
	e.Refresh(context.Background())
	if st, _ := e.State(nil); st.Tier != "expensive" {
		t.Fatal(st.Tier)
	}
	e.Provider = func(context.Context, string) ([]Row, error) { return nil, errors.New("offline") }
	e.Clock = func() time.Time { return now.Add(5 * time.Hour) }
	e.Refresh(context.Background())
	if st, _ := e.State(nil); st.DKKPerKWh == nil || *st.DKKPerKWh != 3.2 {
		t.Fatalf("%+v", st)
	}
	e.Clock = func() time.Time { return now.Add(7 * time.Hour) }
	e.Refresh(context.Background())
	if st, _ := e.State(nil); st.Tier != "normal" || st.DKKPerKWh != nil {
		t.Fatalf("%+v", st)
	}
}
