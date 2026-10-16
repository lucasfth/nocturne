package billing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

// test_credit: zero balance refuses reservation (gateway maps it to 402).
func TestInsufficientCredit(t *testing.T) {
	b, _ := fixture(t)
	if err := b.Exec("UPDATE keys SET balance_msat=0 WHERE key_id='test'"); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.Reserve("r", "test", 5, 20); ok || err != nil {
		t.Fatal("reserve should fail", err)
	}
	if ok, _ := b.Reserve("r", "nokey", 5, 20); ok {
		t.Fatal("unknown key reserved")
	}
}

// test_fx_reprice (Billing part)
func TestReprice(t *testing.T) {
	b, _ := fixture(t)
	b.Reprice(500000) // no DKK prices configured: no-op
	if b.InputPrice != 1000 || b.OutputPrice != 1000 || b.Minimum != 0 {
		t.Fatal("reprice without dkk prices changed prices")
	}
	b.DKKPrices = map[string]float64{"input_dkk_per_million_tokens": 0.5, "output_dkk_per_million_tokens": 2.0, "min_charge_dkk_per_request": 0.01}
	b.Reprice(0)
	b.Reprice(500000)
	if b.OutputPrice != 400 || b.InputPrice != 100 || b.Minimum != 2000 {
		t.Fatal(b.InputPrice, b.OutputPrice, b.Minimum)
	}
	// round() ties to even: 0.0005 DKK/1k at 1e8 DKK/BTC -> 0.5 msat -> max(1,0)=1; 2.5 -> 2
	b.DKKPrices["input_dkk_per_million_tokens"] = 0.5
	b.Reprice(1e8)
	if b.InputPrice != 1 {
		t.Fatal(b.InputPrice)
	}
	b.DKKPrices["min_charge_dkk_per_request"] = 0.025
	b.Reprice(1e9)
	if b.Minimum != 2 {
		t.Fatal(b.Minimum)
	}
}

func TestQuoteCeilAndMinimum(t *testing.T) {
	b, _ := fixture(t)
	b.InputPrice, b.OutputPrice = 333, 777
	// ceil(9/4)=3 -> ceil((3*333+5*777)/1000)=ceil(4.884)=5
	if q := b.Quote(9, 5); q != 5 {
		t.Fatal(q)
	}
	b.Minimum = 50
	if q := b.Quote(9, 5); q != 50 {
		t.Fatal(q)
	}
}

// test_summary_excludes_test_money
func TestSummaryExcludesTestMoney(t *testing.T) {
	b, _ := fixture(t)
	if ok, _ := b.Reserve("r1", "test", 4, 20); !ok {
		t.Fatal("reserve")
	}
	b.Finish("r1", 1, 3, false)
	d := must[pyjson.Object](t)(b.Summary(0, []string{"test"}, "fake"))
	g := func(k string) any { v, _ := d.Get(k); return v }
	keys := g("keys").([]any)
	internal, _ := keys[0].(pyjson.Object).Get("internal")
	if g("earned_msat") != int64(0) || f64(g("test_usage_msat")) <= 0 || g("customers") != int64(0) || internal != true {
		t.Fatal(pyjson.MustMarshal(d))
	}
}

// test_open_signup_trial_and_limits (Billing part) + test_invoice_flow_and_invite
func TestTrialAndInvoiceFlow(t *testing.T) {
	b, _ := fixture(t)
	nk := must[NewKey](t)(b.CreateKey())
	if len(nk.KeyID) != 28 || len(nk.APIKeyPlaintextOnce) != 43 {
		t.Fatal(nk)
	}
	if id, ok, _ := b.KeyForDigest(h(nk.APIKeyPlaintextOnce)); !ok || id != nk.KeyID {
		t.Fatal("new key lookup")
	}
	k := must[*Key](t)(b.Key(nk.KeyID))
	if k.CreditMsat != 0 || k.RateLimitPerMinute != 30 || k.MaxConcurrent != 4 || k.AllowThinking {
		t.Fatal(k)
	}
	b.GrantTrial(nk.KeyID, 20000)
	if bal(t, b, nk.KeyID) != 20000 {
		t.Fatal("trial")
	}
	d := must[pyjson.Object](t)(b.Summary(0, nil, "fake"))
	if v, _ := d.Get("earned_msat"); v != int64(0) {
		t.Fatal(v)
	}
	if v, _ := d.Get("trial_users"); v != int64(1) {
		t.Fatal(pyjson.MustMarshal(d))
	}
	b.AddInvoice(nk.KeyID, 10000, Invoice{"i1", "lnbc1", "ph1", float64(1 << 40)}, "fake")
	b.AddInvoice(nk.KeyID, 10000, Invoice{"i2", "lnbc2", "ph2", 0}, "fake")
	if p := must[[]Row](t)(b.PendingInvoices()); len(p) != 2 {
		t.Fatal(p)
	}
	for range 2 {
		b.InvoiceState("i1", "paid")
		b.InvoiceState("i2", "paid")
	}
	if s := str(must[Row](t)(b.GetInvoice("i1"))["state"]); s != "paid" {
		t.Fatal(s)
	}
	if s := str(must[Row](t)(b.GetInvoice("i2"))["state"]); s != "expired" {
		t.Fatal(s)
	}
	if must[Row](t)(b.GetInvoice("nope")) != nil || bal(t, b, nk.KeyID) != 30000 {
		t.Fatal("balance after invoices")
	}
	b.Credit("test", 7)
	if bal(t, b, "test") != 10007 {
		t.Fatal("credit")
	}
}

func TestStartupReleasesAndLegacyLedger(t *testing.T) {
	b, d := fixture(t)
	b.Reserve("left", "test", 8, 20)
	b.Close()
	b2 := must[*Billing](t)(Open(d))
	defer b2.Close()
	if bal(t, b2, "test") != 10000 {
		t.Fatal("leftover reservation not released")
	}
	tail := must[[]Row](t)(b2.Tail())
	last := tail[len(tail)-2:]
	if str(last[0]["event"]) != "release" || i64(last[0]["amount_msat"]) != 0 || i64(last[1]["amount_msat"]) != 22 {
		t.Fatal(last)
	}
	// legacy ledger.jsonl: one open reservation is refunded on migration
	d2 := t.TempDir()
	os.WriteFile(filepath.Join(d2, "prices.json"), []byte(`{}`), 0o600)
	os.WriteFile(filepath.Join(d2, "api_keys.json"), []byte(`{"z": {"hashed_secret": "a", "credit_msat": 100, "rate_limit_per_minute": 1, "max_concurrent": 1, "allow_thinking": true}, "a": {"hashed_secret": "b", "credit_msat": 0, "rate_limit_per_minute": 1, "max_concurrent": 1}}`), 0o600)
	os.WriteFile(filepath.Join(d2, "ledger.jsonl"), []byte("{\"event\":\"reserve\",\"request_id\":\"x\",\"key_id\":\"z\",\"amount_msat\":40}\n{\"event\":\"reserve\",\"request_id\":\"y\",\"key_id\":\"z\",\"amount_msat\":9}\n{\"event\":\"settle\",\"request_id\":\"y\",\"key_id\":\"z\",\"amount_msat\":9}\n"), 0o600)
	b3 := must[*Billing](t)(Open(d2))
	defer b3.Close()
	if bal(t, b3, "z") != 140 || !must[*Key](t)(b3.Key("z")).AllowThinking {
		t.Fatal(bal(t, b3, "z"))
	}
	if tl := must[[]Row](t)(b3.Tail()); len(tl) != 1 || str(tl[0]["event"]) != "legacy_release" || str(tl[0]["request_id"]) != "x" {
		t.Fatal(tl)
	}
}
