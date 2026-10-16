package lightning

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lucasfth/nocturne/go/internal/app"
	"github.com/lucasfth/nocturne/go/internal/billing"
	"github.com/lucasfth/nocturne/go/internal/edge"
)

const testHash = "abababababababababababababababababababababababababababababababab"

func writeSecret(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}
func ledger(t *testing.T) (*billing.Billing, string) {
	t.Helper()
	d := t.TempDir()
	if e := os.WriteFile(filepath.Join(d, "prices.json"), []byte(`{"price_per_token_msat":1}`), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := billing.Open(d)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { b.Close() })
	k, e := b.CreateKey()
	if e != nil {
		t.Fatal(e)
	}
	return b, k.KeyID
}
func phoenix(t *testing.T, handler http.HandlerFunc) (*Phoenix, *billing.Billing, string) {
	t.Helper()
	b, k := ledger(t)
	p, e := New(Config{Backend: "phoenixd", ConfFile: writeSecret(t, "fixture.conf", "http-password=fixture-full\nhttp-password-limited-access=fixture-read\nwebhook-secret=fixture-signing\n")}, b)
	if e != nil {
		t.Fatal(e)
	}
	ph := p.(*Phoenix)
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	ph.base = s.URL
	return ph, b, k
}
func add(t *testing.T, b *billing.Billing, k string, expiry float64) {
	t.Helper()
	if e := b.AddInvoice(k, 10000, billing.Invoice{InvoiceID: "invoice-test", PaymentHash: testHash, Bolt11: "lnbc-test", ExpiresAt: expiry}, "phoenixd"); e != nil {
		t.Fatal(e)
	}
}
func event() []byte {
	return []byte(`{"type":"payment_received","externalId":"invoice-test","paymentHash":"` + testHash + `","amountSat":999999999}`)
}
func sign(b []byte) string {
	m := hmac.New(sha256.New, []byte("fixture-signing"))
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
func paid(w http.ResponseWriter) {
	w.Write([]byte(`{"paymentHash":"` + testHash + `","externalId":"invoice-test","requestedSat":10,"isPaid":true,"isExpired":false}`))
}
func TestPhoenixCreate(t *testing.T) {
	var external string
	p, _, _ := phoenix(t, func(w http.ResponseWriter, r *http.Request) {
		u, pass, ok := r.BasicAuth()
		if !ok || u != "" || pass != "fixture-read" {
			t.Error("limited credential not preferred")
		}
		r.ParseForm()
		external = r.Form.Get("externalId")
		if r.URL.Path != "/createinvoice" || r.Form.Get("amountSat") != "10" || r.Form.Get("expirySeconds") != "3600" || len([]rune(r.Form.Get("description"))) != 128 {
			t.Error(r.URL.Path, r.Form)
		}
		w.Write([]byte(`{"paymentHash":"` + testHash + `","serialized":"lnbc10-test"}`))
	})
	inv, e := p.CreateInvoice(context.Background(), 10000, strings.Repeat("ø", 140))
	if e != nil || inv.InvoiceID != external || inv.PaymentHash != testHash || inv.Bolt11 != "lnbc10-test" || inv.ExpiresAt < now()+3500 {
		t.Fatal(inv, e)
	}
}
func TestPhoenixBoundsAndResponses(t *testing.T) {
	var calls atomic.Int32
	p, _, _ := phoenix(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"paymentHash":"BAD","serialized":"lntb-test"}`))
	})
	for _, amount := range []int64{0, 999, 1001, -1000, 100001000} {
		if _, e := p.CreateInvoice(context.Background(), amount, ""); e == nil {
			t.Fatal(amount)
		}
	}
	if _, e := p.CreateInvoiceWithExpiry(context.Background(), 1000, "", 0); e == nil {
		t.Fatal("expiry")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input contacted backend")
	}
	if _, e := p.CreateInvoice(context.Background(), 1000, ""); e == nil {
		t.Fatal("invalid mainnet response")
	}
}
func TestPhoenixStatesAndMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
		bad              bool
	}{{"pending", "", "pending", 204, false}, {"paid", `{"paymentHash":"` + testHash + `","externalId":"invoice-test","requestedSat":10,"isPaid":true}`, "paid", 200, false}, {"expired", `{"paymentHash":"` + testHash + `","externalId":"invoice-test","requestedSat":10,"isExpired":true}`, "expired", 200, false}, {"amount", `{"paymentHash":"` + testHash + `","externalId":"invoice-test","requestedSat":1,"isPaid":true}`, "", 200, true}, {"hash", `{"paymentHash":"bad"}`, "", 200, true}, {"external", `{"paymentHash":"` + testHash + `","externalId":"other","requestedSat":10}`, "", 200, true}} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, k := phoenix(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/payments/incoming/"+testHash {
					t.Error(r.URL.Path)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			add(t, b, k, now()+3600)
			state, e := p.CheckInvoice(context.Background(), "invoice-test")
			if (e != nil) != tc.bad || state != tc.want {
				t.Fatal(state, e)
			}
			row, _ := b.GetInvoice("invoice-test")
			if row["state"] != "pending" {
				t.Fatal(row)
			}
		})
	}
}
func TestPhoenixWebhookRaceAndAlteredAmount(t *testing.T) {
	var calls atomic.Int32
	p, b, k := phoenix(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); paid(w) })
	add(t, b, k, now()+3600)
	a := app.New(app.Config{}, b, nil, nil, p, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, e := p.Webhook(context.Background(), event(), sign(event()))
				if e != nil {
					t.Error(e)
				}
			} else {
				if _, e := a.PollInvoice(context.Background(), "invoice-test"); e != nil {
					t.Error(e)
				}
			}
		}(i)
	}
	wg.Wait()
	key, _ := b.Key(k)
	if key.CreditMsat != 10000 {
		t.Fatal(key)
	}
	if ok, e := p.Webhook(context.Background(), event(), sign(event())); ok || e != nil {
		t.Fatal(ok, e)
	}
	if calls.Load() == 0 {
		t.Fatal("must recheck")
	}
}
func TestPhoenixWebhookExpired(t *testing.T) {
	p, b, k := phoenix(t, func(w http.ResponseWriter, r *http.Request) { paid(w) })
	add(t, b, k, 0)
	ok, e := p.Webhook(context.Background(), event(), sign(event()))
	key, _ := b.Key(k)
	if ok || e != nil || key.CreditMsat != 0 {
		t.Fatal(ok, e, key)
	}
}
func TestPhoenixWebhookBoundaries(t *testing.T) {
	p, b, k := phoenix(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected lookup") })
	add(t, b, k, now()+3600)
	a := app.New(app.Config{}, b, nil, nil, p, nil)
	for _, tc := range []struct {
		remote, body, sig string
		status            int
	}{{"127.0.0.1:1", string(event()), "", 401}, {"8.8.8.8:1", string(event()), sign(event()), 403}, {"[::1]:1", "[]", sign([]byte("[]")), 400}, {"127.0.0.1:1", strings.Repeat("x", 16385), "", 413}} {
		r := httptest.NewRequest("POST", "/admin/lightning/webhook", strings.NewReader(tc.body))
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Phoenix-Signature", tc.sig)
		w := httptest.NewRecorder()
		a.Admin(app.AdminConfig{}).ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("POST", "/admin/lightning/webhook", strings.NewReader(string(event())))
	w := httptest.NewRecorder()
	a.Public(edge.Config{}).ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`{`, `null`, `[]`, `{"type":42}`, `{"type":"payment_received","externalId":[]}`} {
		_, e := p.Webhook(context.Background(), []byte(body), sign([]byte(body)))
		if !errors.Is(e, app.ErrWebhookEvent) {
			t.Fatal(body, e)
		}
	}
	for _, body := range []string{`{}`, `{"type":"other"}`, `{"type":"payment_received","externalId":"missing"}`} {
		ok, e := p.Webhook(context.Background(), []byte(body), sign([]byte(body)))
		if ok || e != nil {
			t.Fatal(body, ok, e)
		}
	}
}
func TestPhoenixPollerAndWallet(t *testing.T) {
	p, b, k := phoenix(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getbalance":
			w.Write([]byte(`{"balanceSat":123,"feeCreditSat":4}`))
		case "/getinfo":
			w.Write([]byte(`{"nodeId":"node","channels":[{"inboundLiquiditySat":20},{"inboundLiquiditySat":30}]}`))
		default:
			paid(w)
		}
	})
	add(t, b, k, now()+3600)
	a := app.New(app.Config{}, b, nil, nil, p, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.RunInvoiceMaintenance(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(time.Second)
	for {
		r, _ := b.GetInvoice("invoice-test")
		if r["state"] == "paid" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poller did not pay")
		}
		time.Sleep(time.Millisecond)
	}
	v, e := p.Wallet(context.Background())
	raw, _ := json.Marshal(v)
	if e != nil || string(raw) != `{"balance_sat":123,"channels":2,"fee_credit_sat":4,"inbound_liquidity_sat":50,"node_id":"node"}` {
		t.Fatal(string(raw), e)
	}
}
func TestPhoenixLoopbackAndConfig(t *testing.T) {
	b, _ := ledger(t)
	for _, base := range []string{"http://localhost:9740", "http://127.0.0.1:9741", "https://127.0.0.1:9740", "http://127.0.0.1:9740@evil"} {
		if _, e := New(Config{Backend: "phoenixd", BaseURL: base}, b); e == nil {
			t.Fatal(base)
		}
	}
	p := writeSecret(t, "fixture.conf", "http-password=fixture-fallback\nwebhook-secret=fixture-signing\n")
	v, e := New(Config{Backend: "phoenixd", ConfFile: p}, b)
	if e != nil {
		t.Fatal(e)
	}
	req := &http.Request{Header: v.(*Phoenix).headers}
	_, pass, _ := req.BasicAuth()
	if pass != "fixture-fallback" {
		t.Fatal("fallback")
	}
	os.Chmod(p, 0644)
	if _, e = New(Config{Backend: "phoenixd", ConfFile: p}, b); e == nil {
		t.Fatal("permissions")
	}
}

func TestPhoenixWebhookMismatchAndPending(t *testing.T) {
	for _, tc := range []struct {
		body string
		bad  bool
	}{{`{"paymentHash":"` + testHash + `","externalId":"invoice-test","requestedSat":1,"isPaid":true}`, true}, {`{"paymentHash":"` + testHash + `","externalId":"invoice-test","requestedSat":10,"isPaid":false}`, false}} {
		p, b, k := phoenix(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) })
		add(t, b, k, now()+3600)
		ok, e := p.Webhook(context.Background(), event(), sign(event()))
		if ok || errors.Is(e, app.ErrWebhookEvent) != tc.bad {
			t.Fatal(ok, e)
		}
		key, _ := b.Key(k)
		if key.CreditMsat != 0 {
			t.Fatal(key)
		}
	}
	p, _, _ := phoenix(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not query invalid hash") })
	if _, e := p.checkPaid(context.Background(), "bad", nil); e == nil {
		t.Fatal("hash")
	}
	if state, e := p.CheckInvoice(context.Background(), "missing"); state != "pending" || e != nil {
		t.Fatal(state, e)
	}
}
func TestPhoenixLimitedOnlyAndEmptyWallet(t *testing.T) {
	b, _ := ledger(t)
	v, e := New(Config{Backend: "phoenixd", ConfFile: writeSecret(t, "fixture.conf", "http-password-limited-access=fixture-read\nwebhook-secret=fixture-signing\n")}, b)
	if e != nil {
		t.Fatal(e)
	}
	p := v.(*Phoenix)
	if p.http.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("Phoenix proxy enabled")
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer s.Close()
	p.base = s.URL
	v2, e := p.Wallet(context.Background())
	raw, _ := json.Marshal(v2)
	if e != nil || string(raw) != `{"balance_sat":0,"channels":0,"fee_credit_sat":0,"inbound_liquidity_sat":0,"node_id":null}` {
		t.Fatal(string(raw), e)
	}
}
