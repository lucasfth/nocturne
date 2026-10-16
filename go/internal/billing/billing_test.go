package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func h(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }

// fixture mirrors the pytest `c` fixture: legacy api_keys.json with key
// 'test' (10000 msat) and prices.json {"price_per_token_msat":1}.
func fixture(t *testing.T) (*Billing, string) {
	t.Helper()
	d := t.TempDir()
	w := func(n, s string) {
		if err := os.WriteFile(filepath.Join(d, n), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w("api_keys.json", `{"test": {"hashed_secret": "`+h("api")+`", "credit_msat": 10000, "rate_limit_per_minute": 10, "max_concurrent": 10, "allow_thinking": false}}`)
	w("prices.json", `{"price_per_token_msat":1}`)
	b, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, d
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func bal(t *testing.T, b *Billing, k string) int64 {
	t.Helper()
	key := must[*Key](t)(b.Key(k))
	if key == nil {
		t.Fatalf("no key %s", k)
	}
	return key.CreditMsat
}

func TestMigrationAndKeyLookup(t *testing.T) {
	b, d := fixture(t)
	if _, err := os.Stat(filepath.Join(d, "api_keys.json")); !os.IsNotExist(err) {
		t.Fatal("api_keys.json not retired")
	}
	if st, err := os.Stat(filepath.Join(d, "api_keys.json.migrated")); err != nil || st.Mode().Perm() != 0o400 {
		t.Fatalf("migrated file: %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Join(d, "ledger.sqlite")); st.Mode().Perm() != 0o600 {
		t.Fatal("ledger perms")
	}
	k := must[*Key](t)(b.Key("test"))
	if *k != (Key{h("api"), 10000, 10, 10, false}) {
		t.Fatalf("key %+v", k)
	}
	if id, ok, _ := b.KeyForDigest(h("api")); !ok || id != "test" {
		t.Fatal("digest lookup")
	}
	if _, ok, _ := b.KeyForDigest(h("nope")); ok {
		t.Fatal("bogus digest matched")
	}
	if must[*Key](t)(b.Key("missing")) != nil {
		t.Fatal("missing key")
	}
	r := must[Row](t)(b.row("PRAGMA journal_mode"))
	if str(r["journal_mode"]) != "wal" {
		t.Fatalf("journal_mode %v", r)
	}
	if r := must[Row](t)(b.row("PRAGMA synchronous")); i64(r["synchronous"]) != 2 {
		t.Fatalf("synchronous %v", r)
	}
	if r := must[Row](t)(b.row("PRAGMA foreign_keys")); i64(r["foreign_keys"]) != 1 {
		t.Fatalf("foreign_keys %v", r)
	}
}

// test_billing_idempotency
func TestBillingIdempotency(t *testing.T) {
	b, _ := fixture(t)
	before := bal(t, b, "test")
	if !must[bool](t)(b.Reserve("r", "test", 8, 20)) || bal(t, b, "test") != before-22 {
		t.Fatal("reserve")
	}
	must[bool](t)(b.Reserve("r", "test", 8, 20)) // idempotent, no second hold
	if bal(t, b, "test") != before-22 {
		t.Fatal("double reserve charged")
	}
	b.Finish("r", 2, 3, false)
	b.Finish("r", 2, 3, false)
	if bal(t, b, "test") != before-5 {
		t.Fatalf("after finish %d", bal(t, b, "test"))
	}
	tail := must[[]Row](t)(b.Tail())
	var ev []string
	for _, r := range tail {
		ev = append(ev, str(r["event"]))
	}
	if len(ev) != 3 || ev[0] != "reserve" || ev[1] != "settle" || ev[2] != "release" {
		t.Fatalf("events %v", ev)
	}
	if m := str(tail[1]["meta_json"]); m != `{"prompt_tokens": 2, "completion_tokens": 3}` {
		t.Fatalf("meta_json %q", m)
	}
	if i64(tail[1]["amount_msat"]) != 5 || i64(tail[2]["amount_msat"]) != 17 || str(tail[2]["meta_json"]) != "{}" {
		t.Fatal("amounts")
	}
	if !must[bool](t)(b.Reserve("cancel", "test", 4, 20)) {
		t.Fatal("reserve cancel")
	}
	b.Finish("cancel", 0, 0, true)
	if bal(t, b, "test") != before-5 {
		t.Fatal("cancel did not refund")
	}
}

// test_concurrent_duplicate_finals
func TestConcurrentDuplicateFinals(t *testing.T) {
	b, _ := fixture(t)
	before := bal(t, b, "test")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if ok, err := b.Reserve("duplicate", "test", 8, 20); !ok || err != nil {
				t.Error("reserve", err)
			}
		})
	}
	wg.Wait()
	for range 20 {
		wg.Go(func() { b.Finish("duplicate", 2, 3, false) })
	}
	wg.Wait()
	if bal(t, b, "test") != before-5 {
		t.Fatal(bal(t, b, "test"))
	}
}
