package fx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lucasfth/nocturne/go/internal/billing"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

func const_(v float64) Provider { return func(context.Context) (float64, error) { return v, nil } }

// test_fx_reprice (fx part)
func TestFxReprice(t *testing.T) {
	dir := t.TempDir()
	clock := func() float64 { return 1760000000.5 }
	f := New(100000, dir, const_(5.0), clock)
	if ok, _ := f.Refresh(context.Background(), 600); ok || f.Rate() != 100000 {
		t.Fatal("implausible rate accepted")
	}
	f = New(100000, dir, const_(555000.0), clock)
	var repriced float64
	var _ Repricer = (&billing.Billing{}).Reprice // signature compatibility
	if ok, err := f.RefreshAndReprice(context.Background(), 600, func(r float64) { repriced = r }); !ok || err != nil || f.Rate() != 555000 || repriced != 555000 {
		t.Fatal(ok, err, f.Rate())
	}
	if src, _ := f.Status().Get("source"); src != "coinbase" {
		t.Fatal(src)
	}
	if g := New(1, dir, nil, clock); g.Rate() != 555000 {
		t.Fatal("cache not loaded")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "fx.json"))
	if string(raw) != `{"dkk_per_btc": 555000.0, "updated": 1760000000.5}` {
		t.Fatal(string(raw))
	}
	if st, _ := os.Stat(filepath.Join(dir, "fx.json")); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
}

func TestJumpGuardAndRateLimit(t *testing.T) {
	now := 1000.0
	clock := func() float64 { return now }
	rate := 500000.0
	f := New(100000, t.TempDir(), func(context.Context) (float64, error) { return rate, nil }, clock)
	ctx := context.Background()
	if ok, _ := f.Refresh(ctx, 600); !ok { // config source: no jump guard
		t.Fatal("first live rate")
	}
	rate, now = 700000, now+600
	if ok, _ := f.Refresh(ctx, 600); ok {
		t.Fatal("40% jump within an hour must be ignored")
	}
	now += 599
	if ok, _ := f.Refresh(ctx, 600); ok {
		t.Fatal("rate limited")
	}
	now = 1000 + 3600
	if ok, _ := f.Refresh(ctx, 600); !ok || f.Rate() != 700000 {
		t.Fatal("jump after an hour accepted")
	}
	now += 1.5
	if a, _ := f.Status().Get("age_s"); a != int64(2) {
		t.Fatal(a) // round(1.5) == 2 half-even
	}
	now += 1
	if a, _ := f.Status().Get("age_s"); a != int64(2) {
		t.Fatal(a) // round(2.5) == 2
	}
}

func TestCoinbaseAndCacheEdgeCases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"amount":"612345.67","base":"BTC"}}`))
	}))
	defer srv.Close()
	old := CoinbaseURL
	CoinbaseURL = srv.URL
	defer func() { CoinbaseURL = old }()
	if r, err := Coinbase(NewClient())(context.Background()); err != nil || r != 612345.67 {
		t.Fatal(r, err)
	}
	if NewClient().Transport.(*http.Transport).Proxy != nil {
		t.Fatal("proxy must be nil")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "fx.json")
	for body, want := range map[string]float64{`{"dkk_per_btc": 600000, "updated": null}`: 600000, `{"dkk_per_btc": 5}`: 100000, `garbage`: 100000, `{"dkk_per_btc": 600000}`: 100000} {
		os.WriteFile(p, []byte(body), 0o600)
		f := New(100000, dir, nil, func() float64 { return 0 })
		if f.Rate() != want {
			t.Errorf("%s: %v", body, f.Rate())
		}
		if body[0] == '{' && want == 600000 && pyjson.MustMarshal(f.Status()) != `{"dkk_per_btc": 600000, "source": "cache", "age_s": null}` {
			t.Error(pyjson.MustMarshal(f.Status()))
		}
	}
}
