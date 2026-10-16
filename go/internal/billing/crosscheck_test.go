package billing

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

const prices = `{"input_msat_per_1k_tokens": 333, "output_msat_per_1k_tokens": 777, "min_charge_msat_per_request": 7}`
const legacyKeys = `{"k1": {"hashed_secret": "h1", "credit_msat": 50000, "rate_limit_per_minute": 10, "max_concurrent": 2, "allow_thinking": true}, "k2": {"hashed_secret": "h2", "credit_msat": 9000, "rate_limit_per_minute": 10, "max_concurrent": 2}, "k3": {"hashed_secret": "h3", "credit_msat": 0, "rate_limit_per_minute": 5, "max_concurrent": 1}}`

// pyScript drives the real gateway.billing; it leaves r4 reserved, reopens
// (startup release), then prints summary, keys and the ledger as JSON lines.
const pyScript = `import json, sys
from gateway.billing import Billing
d = sys.argv[1]
b = Billing(d)
b.reserve('r1','k1',100,50); b.finish('r1',30,20)
b.reserve('r2','k2',40,10); b.finish('r2',cancel=True)
b.reserve('r3','k1',8,20); b.finish('r3',2,3); b.finish('r3',2,3)
b.credit('k3',5000); b.grant_trial('k1',1000)
b.add_invoice('k1',20000,dict(invoice_id='i1',bolt11='b1',payment_hash='p1',expires_at=4e9),'phoenixd'); b.invoice_state('i1','paid')
b.add_invoice('k2',3000,dict(invoice_id='i2',bolt11='b2',payment_hash='p2',expires_at=4e9),'fake'); b.invoice_state('i2','paid')
b.add_invoice('k3',777,dict(invoice_id='i3',bolt11='b3',payment_hash='p3',expires_at=4e9),'phoenixd')
b.reserve('r4','k3',10,10)
b.log('custom','x','k1',0,a=1.5,b='\u00e6\U0001F600',c=None,d=[1,2.0,True])
b.db.close()
b = Billing(d)
print(json.dumps(b.summary(0,['k2'],'phoenixd')))
print(json.dumps(b.keys))
print(json.dumps([[r['event'],r['request_id'],r['key_id'],r['amount_msat'],r['meta_json']] for r in b.db.execute('SELECT * FROM ledger ORDER BY id')]))
`

// goOps replays pyScript against the Go port.
func goOps(t *testing.T, d string) {
	b := must[*Billing](t)(Open(d))
	b.Reserve("r1", "k1", 100, 50)
	b.Finish("r1", 30, 20, false)
	b.Reserve("r2", "k2", 40, 10)
	b.Finish("r2", 0, 0, true)
	b.Reserve("r3", "k1", 8, 20)
	b.Finish("r3", 2, 3, false)
	b.Finish("r3", 2, 3, false)
	b.Credit("k3", 5000)
	b.GrantTrial("k1", 1000)
	b.AddInvoice("k1", 20000, Invoice{"i1", "b1", "p1", 4e9}, "phoenixd")
	b.InvoiceState("i1", "paid")
	b.AddInvoice("k2", 3000, Invoice{"i2", "b2", "p2", 4e9}, "fake")
	b.InvoiceState("i2", "paid")
	b.AddInvoice("k3", 777, Invoice{"i3", "b3", "p3", 4e9}, "phoenixd")
	b.Reserve("r4", "k3", 10, 10)
	b.Log("custom", "x", "k1", 0, pyjson.Object{{Key: "a", Val: 1.5}, {Key: "b", Val: "æ😀"}, {Key: "c", Val: nil}, {Key: "d", Val: []any{1, 2.0, true}}})
	b.Close()
}

func dir(t *testing.T) string {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "prices.json"), []byte(prices), 0o600)
	os.WriteFile(filepath.Join(d, "api_keys.json"), []byte(legacyKeys), 0o600)
	return d
}

func goView(t *testing.T, b *Billing) (string, string, string) {
	sum := pyjson.MustMarshal(must[pyjson.Object](t)(b.Summary(0, []string{"k2"}, "phoenixd")))
	keys := must[map[string]Key](t)(b.Keys())
	ko := pyjson.Object{}
	for _, id := range []string{"k1", "k2", "k3"} {
		k := keys[id]
		ko = append(ko, pyjson.Pair{Key: id, Val: pyjson.Object{{Key: "hashed_secret", Val: k.HashedSecret}, {Key: "credit_msat", Val: k.CreditMsat},
			{Key: "rate_limit_per_minute", Val: k.RateLimitPerMinute}, {Key: "max_concurrent", Val: k.MaxConcurrent}, {Key: "allow_thinking", Val: k.AllowThinking}}})
	}
	b.mu.Lock()
	rs, _ := b.rows("SELECT * FROM ledger ORDER BY id")
	b.mu.Unlock()
	var led []any
	for _, r := range rs {
		led = append(led, []any{r["event"], r["request_id"], r["key_id"], r["amount_msat"], r["meta_json"]})
	}
	return sum, pyjson.MustMarshal(ko), pyjson.MustMarshal(led)
}

var tsFields = []string{"created", "last_used"}

func TestCrossCheckPython(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv not available")
	}
	pd := dir(t)
	cmd := exec.Command(uv, "run", "--frozen", "python", "-c", pyScript, pd)
	cmd.Dir = filepath.Join("..", "..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("python reference unavailable: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("python output: %s", out)
	}
	t.Logf("python summary: %s", lines[0])
	// 1. Go opens the Python-written ledger: byte-identical summary, keys, ledger.
	b := must[*Billing](t)(Open(pd))
	s, k, l := goView(t, b)
	b.Close()
	for i, got := range []string{s, k, l} {
		if got != lines[i] {
			t.Errorf("python ledger view %d differs:\n go %s\n py %s", i, got, lines[i])
		}
	}
	// 2. Go replays the same operations: identical rows except timestamps.
	gd := dir(t)
	goOps(t, gd)
	b = must[*Billing](t)(Open(gd))
	s, k, l = goView(t, b)
	b.Close()
	if k != lines[1] || l != lines[2] {
		t.Errorf("go replay differs:\n go %s\n py %s\n go %s\n py %s", k, lines[1], l, lines[2])
	}
	if norm(t, s) != norm(t, lines[0]) {
		t.Errorf("go replay summary differs:\n go %s\n py %s", s, lines[0])
	}
}

// norm drops wall-clock fields (created, last_used) from a summary JSON.
func norm(t *testing.T, s string) string {
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	for _, k := range v["keys"].([]any) {
		for _, f := range tsFields {
			delete(k.(map[string]any), f)
		}
	}
	o, _ := json.Marshal(v)
	return string(o)
}
