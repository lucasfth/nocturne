package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/billing"
	"github.com/lucasfth/nocturne/go/internal/fx"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func adminFixture(t *testing.T) (*App, *WorkerCoordinator, AdminConfig) {
	a, c, _ := workerFixture(t)
	cfg := AdminConfig{SecretSHA256: digest("admin"), Coordinator: c, Fx: fx.New(500000, t.TempDir(), func(context.Context) (float64, error) { return 500000, nil }, nil), TailnetLogins: []string{"allowed"}}
	return a, c, cfg
}
func ar(a *App, c AdminConfig, method, p, b, token, login string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, strings.NewReader(b))
	r.RemoteAddr = "127.0.0.1:123"
	r.Header.Set("Authorization", "Bearer "+token)
	if login != "" {
		r.Header.Set("Tailscale-User-Login", login)
	}
	w := httptest.NewRecorder()
	a.Admin(c).ServeHTTP(w, r)
	return w
}
func TestAdminAuthRoutingAndTailnet(t *testing.T) {
	a, _, c := adminFixture(t)
	for _, x := range []struct {
		method, p, token, login string
		want                    int
	}{
		{"GET", "/admin/summary", "", "", 401}, {"GET", "/admin/summary", "admin", "denied", 403}, {"GET", "/admin/summary", "", "allowed", 401}, {"GET", "/admin", "", "allowed", 503}, {"POST", "/admin/lightning/webhook", "", "allowed", 404}, {"GET", "/admin/workers", "Admin", "", 401}, {"POST", "/admin/workers", "admin", "", 405}, {"HEAD", "/admin", "", "", 405}, {"GET", "/admin/no-such-route", "admin", "", 404}, {"GET", "/admin/summary", "admin", "", 200},
	} {
		w := ar(a, c, x.method, x.p, "", x.token, x.login)
		if w.Code != x.want {
			t.Fatalf("%+v: %d %s", x, w.Code, w.Body)
		}
	}
	if w := request(a, "GET", "/admin/summary", "", "admin"); w.Code != 404 {
		t.Fatal(w.Body)
	}
}
func TestAdminRegistryAndRealInvalidation(t *testing.T) {
	for _, mutation := range []string{"disable", "rotate", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			a, c, cfg := adminFixture(t)
			startWorker(t, c)
			ch := submitWorker(t, c, "admin-invalidation", true, false)
			j := getLease(t, c)
			var w *httptest.ResponseRecorder
			switch mutation {
			case "disable":
				w = ar(a, cfg, "PATCH", "/admin/machines/w", `{"enabled":false}`, "admin", "")
			case "rotate":
				w = ar(a, cfg, "PATCH", "/admin/machines/w", fmt.Sprintf(`{"secret_sha256":%q}`, digest("new")), "admin", "")
			case "delete":
				w = ar(a, cfg, "DELETE", "/admin/machines/w", "", "admin", "")
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body)
			}
			if len(c.Workers()) != 0 {
				t.Fatal("worker not dropped")
			}
			select {
			case e := <-ch:
				if e.Error != "worker_interrupted" {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("stream not invalidated")
			}
			if w = wr(c, "register", `{"worker_id":"w"}`); w.Code != 401 {
				t.Fatal(w.Code, w.Body)
			}
			if w = wr(c, "result", resultBody(j, `"chunk":"stale","final":true`)); w.Code != 401 {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
	a, _, c := adminFixture(t)
	for _, b := range []string{`[]`, `{`, `{"worker_id":"Bad!"}`, `{"worker_id":"m2","secret_sha256":"zz"}`, fmt.Sprintf(`{"worker_id":"m2","secret_sha256":%q,"meta":{"host":"a;rm -rf /"}}`, digest("s2")), fmt.Sprintf(`{"worker_id":"m2","secret_sha256":%q}`, digest("worker"))} {
		if w := ar(a, c, "POST", "/admin/machines", b, "admin", ""); w.Code != 400 {
			t.Fatal(b, w.Code, w.Body)
		}
	}
	b := fmt.Sprintf(`{"worker_id":"m2","secret_sha256":%q,"meta":{"owner":"Anna","host":"air.local"}}`, digest("s2"))
	w := ar(a, c, "POST", "/admin/machines", b, "admin", "")
	if w.Code != 201 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Code, w.Body)
	}
	if w = ar(a, c, "POST", "/admin/machines", b, "admin", ""); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	w = ar(a, c, "PATCH", "/admin/machines/m2", `{"meta":{"owner":null,"agent":"Max"}}`, "admin", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "Anna") {
		t.Fatal(w.Code, w.Body)
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		if w = ar(a, c, method, "/admin/machines/missing", `{"enabled":false}`, "admin", ""); w.Code != 404 {
			t.Fatal(w.Code, w.Body)
		}
	}
}
func TestAdminSummaryTelemetryCreditEnergy(t *testing.T) {
	a, c, cfg := adminFixture(t)
	w := ar(a, cfg, "GET", "/admin/summary", "", "admin", "")
	d := parsed(t, w.Body.String())
	for _, k := range []string{"wallet", "energy", "workers", "days", "keys", "invoices", "customer_credit_liability_msat", "earned_msat", "test_usage_msat", "customers", "fx", "pricing", "queue"} {
		if _, ok := d[k]; !ok {
			t.Fatal(k, d)
		}
	}
	row := d["workers"].([]any)[0].(map[string]any)
	if row["online"] != false || row["battery_pct"] != nil {
		t.Fatal(row)
	}
	startWorker(t, c)
	b := strings.TrimSuffix(beatBody, "}") + `,"metrics":{"cpu_pct":12.5,"gpu_pct":null},"tokens_per_sec":30}`
	if w = wr(c, "heartbeat", b); w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = ar(a, cfg, "GET", "/admin/summary", "", "admin", "")
	d = parsed(t, w.Body.String())
	row = d["workers"].([]any)[0].(map[string]any)
	if row["online"] != true || row["metrics"].(map[string]any)["cpu_pct"] != 12.5 {
		t.Fatal(row)
	}
	w = ar(a, cfg, "GET", "/admin/workers", "", "admin", "")
	if !strings.Contains(w.Body.String(), `"active_requests": []`) || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Body)
	}
	w = ar(a, cfg, "GET", "/admin/energy", "", "admin", "")
	if d = parsed(t, w.Body.String()); d["rows"] == nil {
		t.Fatal(d)
	}
	keys, _ := a.Bill.Keys()
	var id string
	for k := range keys {
		id = k
	}
	for _, amount := range []string{"true", "1.0", "0", "-1"} {
		if w = ar(a, cfg, "POST", "/admin/credit", fmt.Sprintf(`{"key_id":%q,"amount_msat":%s}`, id, amount), "admin", ""); w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	w = ar(a, cfg, "POST", "/admin/credit", fmt.Sprintf(`{"key_id":%q,"amount_msat":10}`, id), "admin", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w = ar(a, cfg, "GET", "/admin/ledger/tail", "", "admin", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "admin_credit") {
		t.Fatal(w.Code, w.Body)
	}
}
func TestAdminAssets(t *testing.T) {
	a, _, c := adminFixture(t)
	c.Dist = fstest.MapFS{"index.html": {Data: []byte("<html>SPA</html>")}, "assets/app.js": {Data: []byte("app()")}, "assets/app.css": {Data: []byte("body{}")}, "assets/a.svg": {Data: []byte("svg")}, "assets/f.woff2": {Data: []byte("font")}, "assets/a.png": {Data: []byte("png")}, "assets/a.bin": {Data: []byte("bin")}}
	w := ar(a, c, "GET", "/admin", "", "", "")
	if w.Code != 200 || w.Header().Get("Content-Security-Policy") != adminCSP || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
	for file, media := range map[string]string{"app.js": "text/javascript", "app.css": "text/css", "a.svg": "image/svg+xml", "f.woff2": "font/woff2", "a.png": "image/png", "a.bin": "application/octet-stream"} {
		w = ar(a, c, "GET", "/admin/assets/"+file, "", "", "")
		if w.Code != 200 || w.Header().Get("Content-Type") != media || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Fatal(file, w.Code, w.Header())
		}
	}
	for _, name := range []string{".hidden", "..", "no-file", "a%2fb", "a%5cb", strings.Repeat("x", 129)} {
		if w = ar(a, c, "GET", "/admin/assets/"+name, "", "", ""); w.Code != 404 {
			t.Fatal(name, w.Code)
		}
	}
}

type adminLightning struct {
	*fakeLightning
	err      error
	credited bool
	bill     *App
}

func (l *adminLightning) Wallet(context.Context) (any, error) {
	return object("balance_sat", int64(42)), l.err
}
func (l *adminLightning) Webhook(_ context.Context, _ []byte, _ string) (bool, error) {
	return l.credited, l.err
}
func (l *adminLightning) MarkPaid(_ context.Context, id string) error {
	return l.bill.Bill.InvoiceState(id, "paid")
}
func TestAdminWebhookWalletAndMarkPaid(t *testing.T) {
	a, _, c := adminFixture(t)
	l := &adminLightning{fakeLightning: &fakeLightning{name: "phoenixd"}, credited: true, bill: a}
	a.Lightning = l
	for _, x := range []struct {
		err  error
		want int
	}{{nil, 200}, {ErrWebhookSignature, 401}, {ErrWebhookEvent, 400}, {errors.New("private backend failure"), 502}} {
		l.err = x.err
		w := ar(a, c, "POST", "/admin/lightning/webhook", `{}`, "", "")
		if w.Code != x.want || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Code, w.Body)
		}
	}
	l.err = nil
	if w := ar(a, c, "POST", "/admin/lightning/webhook", strings.Repeat("x", 16385), "", ""); w.Code != 413 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", "/admin/lightning/webhook", strings.NewReader("{}"))
	r.RemoteAddr = "192.0.2.1:4"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	w := httptest.NewRecorder()
	a.Admin(c).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	l.err = errors.New("private wallet failure")
	w = ar(a, c, "GET", "/admin/summary", "", "admin", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "wallet unavailable") || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Code, w.Body)
	}
	l.err = nil
	l.name = "fake"
	key, _ := a.Bill.CreateKey()
	inv, _ := l.CreateInvoice(context.Background(), 1000, "memo")
	if err := a.Bill.AddInvoice(key.KeyID, 1000, inv, "fake"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		w = ar(a, c, "POST", "/admin/invoices/invoice/mark-paid", "", "admin", "")
		if w.Code != 200 || parsed(t, w.Body.String())["state"] != "paid" {
			t.Fatal(w.Code, w.Body)
		}
	}
	k, _ := a.Bill.Key(key.KeyID)
	if k.CreditMsat != 1000 {
		t.Fatal(k)
	}
}

func TestAdminRotationWakesPendingLease(t *testing.T) {
	a, c, cfg := adminFixture(t)
	startWorker(t, c)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- wr(c, "lease", `{"worker_id":"w"}`) }()
	time.Sleep(20 * time.Millisecond)
	w := ar(a, cfg, "PATCH", "/admin/machines/w", fmt.Sprintf(`{"secret_sha256":%q}`, digest("new")), "admin", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	select {
	case result := <-done:
		if result.Code != 401 {
			t.Fatal(result.Code, result.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("pending poll not invalidated")
	}
}
func TestAdminRetainsOptionalTelemetry(t *testing.T) {
	a, c, cfg := adminFixture(t)
	startWorker(t, c)
	b := strings.TrimSuffix(beatBody, "}") + `,"metrics":{"cpu_pct":12.5},"tokens_per_sec":30}`
	if w := wr(c, "heartbeat", b); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := wr(c, "heartbeat", beatBody); w.Code != 200 {
		t.Fatal(w.Body)
	}
	w := ar(a, cfg, "GET", "/admin/summary", "", "admin", "")
	row := parsed(t, w.Body.String())["workers"].([]any)[0].(map[string]any)
	if row["metrics"].(map[string]any)["cpu_pct"] != 12.5 || row["tokens_per_sec"] != float64(30) {
		t.Fatal(row)
	}
}

// Regression from actual Python/Go HTTP shadow: an empty ledger is an array.
func TestAdminEmptyLedgerArray(t *testing.T) {
	a, _, cfg := adminFixture(t)
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "prices.json"), []byte(`{"price_per_token_msat":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	empty, err := billing.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	a.Bill = empty
	w := ar(a, cfg, "GET", "/admin/ledger/tail", "", "admin", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty ledger: %d %s", w.Code, w.Body)
	}
}
