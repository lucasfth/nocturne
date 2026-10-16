package app

import (
	"context"
	"encoding/json"
	"github.com/lucasfth/nocturne/go/internal/billing"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeCoordinator struct {
	events    chan Event
	job       Job
	pending   int
	cancelled int
	submitErr error
}

func (f *fakeCoordinator) Workers() map[string]energy.Worker { return map[string]energy.Worker{} }
func (f *fakeCoordinator) Pending(bool) int                  { return f.pending }
func (f *fakeCoordinator) Submit(_ context.Context, j Job) (<-chan Event, error) {
	f.job = j
	return f.events, f.submitErr
}
func (f *fakeCoordinator) Cancel(string) { f.cancelled++ }

type fakeLightning struct {
	name, state string
	err         error
	calls       int
}

func (f *fakeLightning) Name() string { return f.name }
func (f *fakeLightning) CreateInvoice(_ context.Context, _ int64, memo string) (billing.Invoice, error) {
	f.calls++
	return billing.Invoice{InvoiceID: "invoice", Bolt11: "fake-not-payable-invoice", PaymentHash: "hash", ExpiresAt: float64(time.Now().Unix() + 3600)}, f.err
}
func (f *fakeLightning) CheckInvoice(context.Context, string) (string, error) { return f.state, f.err }
func fixture(t *testing.T) (*App, *fakeCoordinator, *fakeLightning, string) {
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
	nk, e := b.CreateKey()
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Credit(nk.KeyID, 10000); e != nil {
		t.Fatal(e)
	}
	f := &fakeCoordinator{events: make(chan Event, 10)}
	l := &fakeLightning{name: "fake", state: "pending"}
	a := New(Config{Model: "Ornith-1.5-9B", MaxTokens: 100, MaxInputChars: 1000, MaxContext: 1000, QueueDepth: 2}, b, energy.New(&energy.Config{}, nil, nil), f, l, func() float64 { return 500000 })
	return a, f, l, nk.APIKeyPlaintextOnce
}
func request(a *App, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

const chatBody = `{"model":"Ornith-1.5-9B","messages":[{"role":"user","content":"hello"}],"max_tokens":20}`

func parsed(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if e := json.Unmarshal([]byte(s), &v); e != nil {
		t.Fatal(e, s)
	}
	return v
}
func TestAuthAndMethods(t *testing.T) {
	a, _, _, token := fixture(t)
	for _, c := range []struct {
		method, path, token string
		status              int
	}{{"GET", "/v1/models", "bad", 401}, {"GET", "/v1/models", token, 200}, {"POST", "/healthz", token, 404}, {"HEAD", "/healthz", token, 404}, {"GET", "/healthz", "", 200}, {"GET", "/v1/status", "", 200}, {"GET", "/v1/pricing", "", 200}, {"GET", "/v1/credits/balance", token, 200}} {
		w := request(a, c.method, c.path, "", c.token)
		if w.Code != c.status {
			t.Fatalf("%+v: %d %s", c, w.Code, w.Body)
		}
	}
}
func TestChatSuccessBilling(t *testing.T) {
	a, f, _, token := fixture(t)
	n := int64(999)
	f.events <- Event{Text: "hello", Final: true, CompletionTokens: &n}
	w := request(a, "POST", "/v1/chat/completions", chatBody, token)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	v := parsed(t, w.Body.String())
	u := v["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(2) || u["completion_tokens"] != float64(3) {
		t.Fatal(u)
	}
	rows, e := a.Bill.Tail()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, r := range rows {
		if r["event"] == "token_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatal(rows)
	}
	key, _ := a.Bill.Key(f.job.Key)
	if key.CreditMsat != 9995 {
		t.Fatal(key)
	}
	if f.cancelled != 0 {
		t.Fatal(f.cancelled)
	}
	if e := a.Bill.Finish(f.job.ID, 2, 3, false); e != nil {
		t.Fatal(e)
	}
	key, _ = a.Bill.Key(f.job.Key)
	if key.CreditMsat != 9995 {
		t.Fatal(key)
	}
}
