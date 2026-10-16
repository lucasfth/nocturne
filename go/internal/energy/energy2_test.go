package energy

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestWindowBoundaryAndGrace(t *testing.T) {
	for _, c := range []struct {
		h, m int
		open bool
	}{{6, 0, false}, {15, 0, false}, {15, 30, true}, {23, 0, true}} {
		e, _ := fixture(cfg(), c.h, int64(500))
		e.Clock = func() time.Time { return time.Date(2026, 10, 10, c.h, c.m, 0, 0, time.UTC) }
		if o, _, _ := e.Window(); o != c.open {
			t.Errorf("%02d:%02d UTC open=%v", c.h, c.m, o)
		}
	}
}

func TestBulkSkipsReserveMarginBelowCeiling(t *testing.T) {
	c := cfg()
	e, _ := fixture(c, 19, int64(2200)) // 3.4 DKK/kWh, expensive
	e.Refresh(context.Background())
	if st, _ := e.State(nil); st.Tier != "expensive" {
		t.Fatal(st.Tier)
	}
	if err := e.Admit(nil, 1, 1000, 4096, true); err != nil {
		t.Fatal(err)
	}
	if err := e.Admit(nil, 1, 1000, 4096, false); err == nil {
		t.Fatal("paid tiny revenue must be refused")
	}
	c.Energy.BulkMaxDKKKWh = f(3.0)
	if err := e.Admit(nil, 1, 1000, 4096, true); err == nil || err.Headers["X-Reason"] != "energy-price" {
		t.Fatalf("%+v", err)
	}
}

func TestPaidAdmittedAtEveningPriceRefusedOnSpike(t *testing.T) {
	msat := func(i, o float64) int64 { return int64(math.RoundToEven((i*0.5 + o*2.0) / 1e6 * 1e11 / 100000)) }
	e, _ := fixture(cfg(), 19, int64(2200))
	e.Refresh(context.Background())
	if err := e.Admit(nil, msat(8000, 4096), 8000, 4096, false); err != nil {
		t.Fatal(err)
	}
	e, _ = fixture(cfg(), 19, int64(6000))
	e.Refresh(context.Background())
	if err := e.Admit(nil, msat(8000, 4096), 8000, 4096, false); err == nil {
		t.Fatal("spike must refuse")
	}
}

func TestRefreshRateLimitAndNoEnergy(t *testing.T) {
	calls := 0
	e, now := fixture(cfg(), 19, int64(500))
	e.Provider = func(context.Context, string) ([]Row, error) { calls++; return nil, nil }
	e.Refresh(context.Background())
	e.Clock = func() time.Time { return now.Add(29 * time.Minute) }
	e.Refresh(context.Background())
	e.Clock = func() time.Time { return now.Add(30 * time.Minute) }
	e.Refresh(context.Background())
	if calls != 2 {
		t.Fatal(calls)
	}
	c := cfg()
	c.Energy = nil
	c.Schedule = nil
	e, _ = fixture(c, 10, int64(1))
	e.Refresh(context.Background())
	if st, _ := e.State(nil); !st.WindowOpen || st.PriceArea != "DK2" || st.Tier != "normal" || len(e.Rows()) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestCurrentRowResolution(t *testing.T) {
	e, _ := fixture(cfg(), 19, int64(0))
	e.Provider = func(context.Context, string) ([]Row, error) {
		return []Row{{"2026-10-10T21:00:00", int64(100), "DayAheadPrices"}, {"2026-10-10T21:15:00", 300.5, "DayAheadPrices"},
			{"2026-10-10T20:00:00", "900", "Elspotprices"}}, nil
	}
	e.Clock = func() time.Time { return time.Date(2026, 10, 10, 19, 20, 0, 0, time.UTC) } // 21:20 CEST
	e.Refresh(context.Background())
	if st, _ := e.State(nil); st.DKKPerKWh == nil || math.Abs(*st.DKKPerKWh-1.5005) > 1e-12 {
		t.Fatalf("%+v", st)
	}
}
