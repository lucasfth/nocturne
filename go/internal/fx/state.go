package fx

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

// New mirrors Fx(config, data): dkkPerBTC is float(config.get('dkk_per_btc', 100000)).
func New(dkkPerBTC float64, dataDir string, p Provider, clock func() float64) *Fx {
	if p == nil {
		p = Coinbase(NewClient())
	}
	if clock == nil {
		clock = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	}
	f := &Fx{Path: filepath.Join(dataDir, "fx.json"), Provider: p, Clock: clock, rate: dkkPerBTC, source: "config"}
	f.load()
	return f
}

// jsonNum decodes a Python json number as int64 or float64.
func jsonNum(v any) (any, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, false
	}
	if !strings.ContainsAny(string(n), ".eE") {
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	x, err := n.Float64()
	return x, err == nil
}

func (f *Fx) load() {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var d map[string]any
	if dec.Decode(&d) != nil {
		return
	}
	r, ok := jsonNum(d["dkk_per_btc"])
	u, hasU := d["updated"]
	if !ok || !hasU || !(num(r) > 1e4 && num(r) < 1e8) {
		return
	}
	if u != nil {
		if u, ok = jsonNum(u); !ok {
			return // deviation: Python would accept and fail later in status()
		}
	}
	f.rate, f.updated, f.source = r, u, "cache"
}

// Rate is the current DKK per BTC.
func (f *Fx) Rate() float64 { f.mu.Lock(); defer f.mu.Unlock(); return num(f.rate) }

// Refresh ports Fx.refresh(every): true when a new rate was accepted. A
// returned error is only a cache write failure (Python raises there).
func (f *Fx) Refresh(ctx context.Context, every float64) (bool, error) {
	now := f.Clock()
	f.mu.Lock()
	if f.lastAttempt != nil && now-*f.lastAttempt < every {
		f.mu.Unlock()
		return false, nil
	}
	f.lastAttempt = &now
	f.mu.Unlock()
	rate, err := f.Provider(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil || !(rate > 1e4 && rate < 1e8) {
		return false, nil
	}
	// Ignore a single wild jump (>30%) vs. the previous live value.
	if f.source != "config" && math.Abs(rate/num(f.rate)-1) > .3 && now-num(f.updated) < 3600 {
		return false, nil
	}
	f.rate, f.updated, f.source = rate, now, "coinbase"
	body := pyjson.MustMarshal(pyjson.Object{{Key: "dkk_per_btc", Val: rate}, {Key: "updated", Val: now}})
	if err := os.WriteFile(f.Path, []byte(body), 0o600); err != nil {
		return true, err
	}
	return true, os.Chmod(f.Path, 0o600)
}

// RefreshAndReprice runs Refresh and, on success, calls reprice with the new
// rate (app.py: if await fx.refresh(): bill.reprice(fx.rate)).
func (f *Fx) RefreshAndReprice(ctx context.Context, every float64, reprice Repricer) (bool, error) {
	ok, err := f.Refresh(ctx, every)
	if ok && reprice != nil {
		reprice(f.Rate())
	}
	return ok, err
}

// Status ports Fx.status() with Python key order.
func (f *Fx) Status() pyjson.Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	var age any
	if f.updated != nil {
		age = pyjson.RoundInt(f.Clock() - num(f.updated))
	}
	return pyjson.Object{{Key: "dkk_per_btc", Val: f.rate}, {Key: "source", Val: f.source}, {Key: "age_s", Val: age}}
}
