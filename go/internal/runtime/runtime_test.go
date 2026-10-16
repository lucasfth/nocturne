package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"github.com/lucasfth/nocturne/go/internal/energy"
	"github.com/lucasfth/nocturne/go/internal/lightning"
)

func put(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, name)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
}
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	put(t, root, "config.json", `{"model":"test","max_tokens":100,"max_input_chars":1000,"max_context":2000,"request_timeout_s":5,"queue_depth":8,"lightning":{"backend":"fake"},"energy":{"price_area":"DK2"},"admin_tailnet_logins":["allowed"]}`)
	h := sha256.Sum256([]byte("admin"))
	put(t, root, "data/admin-secret.sha256", hex.EncodeToString(h[:]))
	put(t, root, "data/prices.json", `{"input_dkk_per_million_tokens":1,"output_dkk_per_million_tokens":2,"min_charge_dkk_per_request":0.01}`)
	return root
}
func offline() Options {
	return Options{Energy: func(context.Context, string) ([]energy.Row, error) { return nil, errors.New("offline") }, FX: func(context.Context) (float64, error) { return 200000, nil }}
}
func TestConfigDefaultsAndRejection(t *testing.T) {
	root := fixture(t)
	c, e := Load(root)
	if e != nil {
		t.Fatal(e)
	}
	if c.DKK != 100000 || c.PerKey != 4 || c.BulkQueue != 16 || c.BodyIdle != 5 || c.Lightning.Expiry != 3600 || c.ServiceName != "inference" {
		t.Fatal(c)
	}
	original, _ := os.ReadFile(filepath.Join(root, "config.json"))
	var base map[string]any
	json.Unmarshal(original, &base)
	for _, tc := range []struct {
		k string
		v any
	}{{"model", nil}, {"model", 42}, {"request_timeout_s", 0}, {"request_timeout_s", 1e100}, {"max_tokens", true}, {"per_key_max_concurrent", 0}, {"lightning", map[string]any{}}, {"lightning", map[string]any{"backend": "typo-password-do-not-log"}}, {"schedule", map[string]any{"tz": "INVALID", "start": "17:00", "end": "08:00"}}, {"schedule", map[string]any{"tz": "UTC", "start": "99:99", "end": "08:00"}}, {"energy", map[string]any{"refresh_min": -1}}} {
		t.Run(tc.k+"-"+stringMust(tc.v), func(t *testing.T) {
			b := map[string]any{}
			for k, v := range base {
				b[k] = v
			}
			b[tc.k] = tc.v
			raw, _ := json.Marshal(b)
			put(t, root, "config.json", string(raw))
			_, e := Load(root)
			if e == nil || e.Error() != "invalid config.json" {
				t.Fatal(e)
			}
		})
	}
	for _, s := range []string{"null", "[]", string(original) + " {}", `{"model":"secret","model":false}`, strings.Replace(string(original), `"backend":"fake"`, `"backend":"fake","backend":"fake"`, 1)} {
		put(t, root, "config.json", s)
		if _, e := Load(root); e == nil {
			t.Fatal(s)
		}
	}
}
func stringMust(v any) string { b, _ := json.Marshal(v); return string(b) }
func TestPrivateAdminAndStartupFailures(t *testing.T) {
	root := fixture(t)
	p := filepath.Join(root, "data/admin-secret.sha256")
	os.Chmod(p, 0644)
	if r, e := New(root, offline()); e == nil {
		r.Close()
		t.Fatal("public digest accepted")
	}
	os.Chmod(p, 0600)
	put(t, root, "data/admin-secret.sha256", "not-a-digest")
	if r, e := New(root, offline()); e == nil {
		r.Close()
		t.Fatal("invalid digest accepted")
	}
}
func TestSharedWiringRecoveryAndAssets(t *testing.T) {
	root := fixture(t)
	b, e := billing.Open(filepath.Join(root, "data"))
	if e != nil {
		t.Fatal(e)
	}
	k, e := b.CreateKey()
	if e != nil {
		t.Fatal(e)
	}
	b.Credit(k.KeyID, 10000)
	ok, e := b.Reserve("orphan", k.KeyID, 8, 30)
	if !ok || e != nil {
		t.Fatal(ok, e)
	}
	b.Close()
	put(t, root, "data/fx.json", `{"dkk_per_btc":150000,"updated":0}`)
	r, e := New(root, offline())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	key, e := r.Bill.Key(k.KeyID)
	if e != nil || key.CreditMsat != 10000 {
		t.Fatal(key, e)
	}
	tail, _ := r.Bill.Tail()
	found := false
	for _, v := range tail {
		if v["request_id"] == "orphan" && v["event"] == "release" {
			found = true
		}
	}
	if !found {
		t.Fatal(tail)
	}
	if r.App.Bill != r.Bill || r.App.Energy != r.Energy || r.App.Coordinator != r.Workers || r.Workers.Energy != r.Energy || r.App.Rate() != 150000 {
		t.Fatal("unshared components")
	}
	in, _, _ := r.Bill.Prices()
	if in != 667 {
		t.Fatal(in)
	}
	for _, tc := range []struct {
		handler            http.Handler
		path, token, login string
		want               int
	}{{r.Servers.Admin.Handler, "/admin", "", "", 503}, {r.Servers.Admin.Handler, "/admin/machines", "admin", "", 200}, {r.Servers.Admin.Handler, "/admin/machines", "bad", "", 401}, {r.Servers.Admin.Handler, "/admin/machines", "admin", "denied", 403}, {r.Servers.Public.Handler, "/admin/machines", "admin", "", 404}, {r.Servers.Public.Handler, "/", "", "", 503}, {r.Servers.Worker.Handler, "/healthz", "", "", 404}} {
		q := httptest.NewRequest("GET", tc.path, nil)
		q.Header.Set("Authorization", "Bearer "+tc.token)
		if tc.login != "" {
			q.Header.Set("Tailscale-User-Login", tc.login)
		}
		w := httptest.NewRecorder()
		tc.handler.ServeHTTP(w, q)
		if w.Code != tc.want {
			t.Fatalf("%s got %d: %s", tc.path, w.Code, w.Body)
		}
	}
	put(t, root, "web/dist/index.html", "<h1>built</h1>")
	w := httptest.NewRecorder()
	r.Servers.Admin.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/admin", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "built") {
		t.Fatal(w.Code, w.Body)
	}
}
func TestServeLifecycleAndMaintenanceCancellation(t *testing.T) {
	root := fixture(t)
	var fxCalls, energyCalls atomic.Int32
	started := make(chan struct{})
	o := offline()
	o.FX = func(ctx context.Context) (float64, error) { fxCalls.Add(1); return 200000, nil }
	o.Energy = func(ctx context.Context, _ string) ([]energy.Row, error) {
		energyCalls.Add(1)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var mu sync.Mutex
	var ls []net.Listener
	o.Listen = func(network, address string) (net.Listener, error) {
		if !strings.HasPrefix(address, "127.0.0.1:") {
			t.Errorf("unsafe address %s", address)
		}
		l, e := net.Listen(network, "127.0.0.1:0")
		mu.Lock()
		ls = append(ls, l)
		mu.Unlock()
		return l, e
	}
	o.ShutdownTimeout = 100 * time.Millisecond
	r, e := New(root, o)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("maintenance not started")
	}
	mu.Lock()
	l := ls[0]
	mu.Unlock()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, e := client.Get("http://" + l.Addr().String() + "/v1/pricing")
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung")
	}
	if energyCalls.Load() != 1 {
		t.Fatal(energyCalls.Load())
	}
	for _, l := range ls {
		if _, e := net.DialTimeout("tcp", l.Addr().String(), 100*time.Millisecond); e == nil {
			t.Fatal("listener leaked")
		}
	}
	client.CloseIdleConnections()
}
func TestServeRepricesAndPartialBindCleanup(t *testing.T) {
	root := fixture(t)
	o := offline()
	var first net.Listener
	calls := 0
	o.Listen = func(n, a string) (net.Listener, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("secret-listener-error")
		}
		var e error
		first, e = net.Listen(n, "127.0.0.1:0")
		return first, e
	}
	r, e := New(root, o)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Serve(context.Background()); e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal(e)
	}
	if c, e := net.DialTimeout("tcp", first.Addr().String(), 100*time.Millisecond); e == nil {
		c.Close()
		t.Fatal("partial listener leaked")
	}
	root = fixture(t)
	o = offline()
	o.Listen = func(n, a string) (net.Listener, error) { return net.Listen(n, "127.0.0.1:0") }
	r, e = New(root, o)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for r.FX.Rate() != 200000 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.FX.Rate() != 200000 {
		t.Fatal("FX not refreshed")
	}
	cancel()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	in, _, _ := r.Bill.Prices()
	if in != 500 {
		t.Fatal(in)
	}
}

type paymentMock struct {
	calls   atomic.Int32
	entered chan struct{}
	block   bool
}

func (*paymentMock) Name() string { return "mock" }
func (*paymentMock) CreateInvoice(context.Context, int64, string) (billing.Invoice, error) {
	return billing.Invoice{}, errors.New("unused")
}
func (p *paymentMock) CheckInvoice(ctx context.Context, id string) (string, error) {
	p.calls.Add(1)
	if p.entered != nil {
		close(p.entered)
	}
	if p.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "paid", nil
}
func TestInvoiceMaintenanceUsesSharedBillingAndCancellation(t *testing.T) {
	for _, block := range []bool{false, true} {
		t.Run(stringMust(block), func(t *testing.T) {
			root := fixture(t)
			p := &paymentMock{entered: make(chan struct{}), block: block}
			o := offline()
			o.Payment = func(_ lightning.Config, b *billing.Billing) (app.Lightning, error) {
				if b == nil {
					t.Fatal("missing billing")
				}
				return p, nil
			}
			o.Listen = func(n, a string) (net.Listener, error) { return net.Listen(n, "127.0.0.1:0") }
			r, e := New(root, o)
			if e != nil {
				t.Fatal(e)
			}
			k, e := r.Bill.CreateKey()
			if e != nil {
				t.Fatal(e)
			}
			if e = r.Bill.AddInvoice(k.KeyID, 1000, billing.Invoice{InvoiceID: "pending", PaymentHash: "hash", Bolt11: "mock-not-payable", ExpiresAt: float64(time.Now().Unix() + 3600)}, "mock"); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Serve(ctx) }()
			select {
			case <-p.entered:
			case <-time.After(3 * time.Second):
				cancel()
				t.Fatal("invoice poll missing")
			}
			if !block {
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					row, e := r.Bill.GetInvoice("pending")
					if e != nil {
						t.Fatal(e)
					}
					if row["state"] == "paid" {
						break
					}
					time.Sleep(time.Millisecond)
				}
				row, _ := r.Bill.GetInvoice("pending")
				if row["state"] != "paid" {
					t.Fatal(row)
				}
				key, _ := r.Bill.Key(k.KeyID)
				if key.CreditMsat != 1000 {
					t.Fatal(key)
				}
			}
			cancel()
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("invoice cancellation hung")
			}
			if p.calls.Load() != 1 {
				t.Fatal(p.calls.Load())
			}
		})
	}
}
func TestConcurrentLivePricing(t *testing.T) {
	root := fixture(t)
	r, e := New(root, offline())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Bill.Reprice(float64(100000 + j))
				r.Energy.SetLiveRate(float64(100000 + j))
				r.Bill.Quote(20, 30)
				w := httptest.NewRecorder()
				r.App.ServeHTTP(w, httptest.NewRequest("GET", "/v1/pricing", nil))
				if w.Code != 200 {
					t.Error(w.Code)
				}
				r.Energy.Admit(nil, 10, 5, 10, false)
			}
		}()
	}
	wg.Wait()
}

func TestShutdownCancelsActiveChatAndRefundsBeforeClose(t *testing.T) {
	root := fixture(t)
	o := offline()
	o.Energy = func(context.Context, string) ([]energy.Row, error) { return nil, errors.New("offline") }
	ready := make(chan net.Listener, 3)
	o.Listen = func(n, a string) (net.Listener, error) {
		l, e := net.Listen(n, "127.0.0.1:0")
		if e == nil {
			ready <- l
		}
		return l, e
	}
	r, e := New(root, o)
	if e != nil {
		t.Fatal(e)
	}
	k, e := r.Bill.CreateKey()
	if e != nil {
		t.Fatal(e)
	}
	r.Bill.Credit(k.KeyID, 100000)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx) }()
	l := <-ready
	transport := &http.Transport{Proxy: nil}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	chatDone := make(chan struct{})
	go func() {
		defer close(chatDone)
		q, _ := http.NewRequest("POST", "http://"+l.Addr().String()+"/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hello"}],"max_tokens":20}`))
		q.Header.Set("Authorization", "Bearer "+k.APIKeyPlaintextOnce)
		resp, e := client.Do(q)
		if e == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for r.Workers.Pending(false) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.Workers.Pending(false) != 1 {
		cancel()
		t.Fatal("chat not queued")
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active request prevented shutdown")
	}
	<-chatDone
	b, e := billing.Open(filepath.Join(root, "data"))
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	key, e := b.Key(k.KeyID)
	if e != nil || key.CreditMsat != 100000 {
		t.Fatal(key, e)
	}
	tail, _ := b.Tail()
	found := false
	for _, v := range tail {
		if v["event"] == "cancel" {
			found = true
		}
	}
	if !found {
		t.Fatal("shutdown did not settle cancellation before DB closed", tail)
	}
}
