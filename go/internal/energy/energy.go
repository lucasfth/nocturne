package energy

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Energy ports the Python Energy class. State is in memory only, as in Python.
type Energy struct {
	Config   *Config
	Provider Provider
	Clock    func() time.Time // aware "now"; Python default datetime.now(utc)

	mu          sync.Mutex // guards the fields below (Python asyncio.Lock + GIL)
	refreshing  bool
	rows        []Row
	lastSuccess *float64
	lastAttempt *float64
	lastPrice   *float64
	liveRate    *float64
}

// New mirrors Energy(config, provider=public_prices, clock=None).
func New(cfg *Config, p Provider, clock func() time.Time) *Energy {
	if p == nil {
		p = PublicPrices(NewClient())
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Energy{Config: cfg, Provider: p, Clock: clock}
}

func ts(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Rows and LastSuccess expose the cached data (for /admin energy output).
func (e *Energy) Rows() []Row           { e.mu.Lock(); defer e.mu.Unlock(); return append([]Row(nil), e.rows...) }
func (e *Energy) LastSuccess() *float64 { e.mu.Lock(); defer e.mu.Unlock(); return e.lastSuccess }

// Refresh ports Energy.refresh(): rate-limited by refresh_min, errors swallowed.
func (e *Energy) Refresh(ctx context.Context) {
	ec := e.Config.Energy
	if !ec.truthy() {
		return
	}
	now := ts(e.Clock())
	e.mu.Lock()
	if e.refreshing {
		e.mu.Unlock()
		return
	}
	if e.lastAttempt != nil && now-*e.lastAttempt < or(ec.RefreshMin, 30)*60 {
		e.mu.Unlock()
		return
	}
	e.lastAttempt = &now
	e.refreshing = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.refreshing = false; e.mu.Unlock() }()
	if ec.PriceArea == nil { // KeyError, swallowed by except Exception
		return
	}
	rows, err := e.Provider(ctx, *ec.PriceArea)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rows = rows
	p, err := e.currentRowPrice()
	if err != nil || p == nil {
		return
	}
	e.lastPrice, e.lastSuccess = p, &now
}

func spot(v any) (float64, error) {
	switch x := v.(type) {
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	case float64:
		return x, nil
	case string:
		return strconv.ParseFloat(x, 64)
	}
	return 0, fmt.Errorf("bad spot %v", v)
}

// currentRowPrice ports current_row_price(); caller holds mu.
func (e *Energy) currentRowPrice() (*float64, error) {
	now := e.Clock().In(Copenhagen)
	var match *Row
	for i, r := range e.rows {
		st, naive, err := parseISO(r.Time)
		if err != nil {
			return nil, err
		}
		res := 60 * time.Minute
		if r.Dataset == "DayAheadPrices" {
			res = 15 * time.Minute
		}
		var in bool
		if naive { // same ZoneInfo object: Python compares/adds in wall time
			nw, sw := wall(now), wall(st)
			in = !sw.After(nw) && nw.Before(sw.Add(res))
		} else {
			in = !st.After(now) && now.Before(st.Add(res))
		}
		if in {
			match = &e.rows[i]
		}
	}
	if match == nil {
		return nil, nil
	}
	s, err := spot(match.Spot)
	if err != nil {
		return nil, err
	}
	p := s/1000 + e.Config.Energy.get(func(c *EnergyConfig) *float64 { return c.TariffDKKPerKWh }, 1.2)
	return &p, nil
}

// SetLiveRate publishes FX updates without mutating immutable Config.
func (e *Energy) SetLiveRate(v float64) { e.mu.Lock(); defer e.mu.Unlock(); e.liveRate = &v }
func (e *Energy) rate() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.liveRate != nil {
		return *e.liveRate
	}
	if e.Config.LiveDKKPerBTC != nil {
		return *e.Config.LiveDKKPerBTC
	}
	return e.Config.DKKPerBTC
}
