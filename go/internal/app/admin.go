package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/edge"
	"github.com/lucasfth/nocturne/go/internal/fx"
	"github.com/lucasfth/nocturne/go/internal/machines"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// AdminConfig is immutable after construction; Dist is rooted at web/dist.
// Coordinator must be the instance used for worker ingress and public scheduling.
type AdminConfig struct {
	SecretSHA256  string
	TailnetLogins []string
	Coordinator   *WorkerCoordinator
	Fx            *fx.Fx
	Dist          fs.FS
	InternalKeys  []string
}

// Real adapters optionally expose these capabilities. Wallet must be sanitized.
type WalletBackend interface {
	Wallet(context.Context) (any, error)
}
type WebhookBackend interface {
	Webhook(context.Context, []byte, string) (bool, error)
}
type MarkPaidBackend interface {
	MarkPaid(context.Context, string) error
}

var ErrWebhookSignature = errors.New("invalid webhook signature")
var ErrWebhookEvent = errors.New("invalid webhook event")

func (a *App) Admin(c AdminConfig) http.Handler {
	return edge.TailnetGuard(edge.AdminIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.serveAdmin(w, r, c) }), settingsBrowserGuard, func(r *http.Request) int {
		if r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/assets/") || r.URL.Path == "/admin/lightning/webhook" {
			return 0
		}
		if !edge.AuthenticateDigest(r, c.SecretSHA256) {
			return http.StatusUnauthorized
		}
		return 0
	}), c.TailnetLogins)
}
func adminRoute(p string) (string, string) {
	if m, _, _ := benchmarkRoute(p); m != "" {
		return m, ""
	}
	switch p {
	case "/admin", "/admin/energy", "/admin/workers", "/admin/summary", "/admin/ledger/tail":
		return "GET", ""
	case "/admin/config":
		return "GET, PATCH", ""
	case "/admin/machines":
		return "GET, POST", ""
	case "/admin/credit", "/admin/lightning/webhook":
		return "POST", ""
	}
	for _, prefix := range []string{"/admin/assets/", "/admin/machines/"} {
		if s, ok := strings.CutPrefix(p, prefix); ok && s != "" && !strings.Contains(s, "/") {
			if prefix == "/admin/assets/" {
				return "GET", s
			}
			return "PATCH, DELETE", s
		}
	}
	if s, ok := strings.CutPrefix(p, "/admin/invoices/"); ok && strings.HasSuffix(s, "/mark-paid") {
		id := strings.TrimSuffix(s, "/mark-paid")
		if id != "" && !strings.Contains(id, "/") {
			return "POST", id
		}
	}
	return "", ""
}
func (a *App) serveAdmin(w http.ResponseWriter, r *http.Request, c AdminConfig) {
	methods, id := adminRoute(r.URL.Path)
	if methods == "" {
		reject(w, 404, "Not Found")
		return
	}
	found := false
	for _, m := range strings.Split(methods, ", ") {
		if m == r.Method {
			found = true
		}
	}
	if !found {
		w.Header().Set("Allow", methods)
		reject(w, 405, "Method Not Allowed")
		return
	}
	p := r.URL.Path
	if p == "/admin" || strings.HasPrefix(p, "/admin/assets/") {
		a.adminStatic(w, r, c.Dist, id)
		return
	}
	if p == "/admin/lightning/webhook" {
		a.adminWebhook(w, r)
		return
	}
	if !edge.AuthenticateDigest(r, c.SecretSHA256) {
		reject(w, 401, "invalid admin credentials")
		return
	}
	if p == "/admin/config" {
		a.adminSettings(w, r)
		return
	}
	if c.Coordinator == nil {
		reject(w, 503, "admin coordinator unavailable")
		return
	}
	if methods, _, _ := benchmarkRoute(p); methods != "" {
		c.Coordinator.serveBenchmarkAdmin(w, r)
		return
	}
	switch p {
	case "/admin/machines":
		if r.Method == "GET" {
			adminReply(w, 200, c.Coordinator.Machines.List())
			return
		}
		b := adminBody(w, r)
		if b == nil {
			return
		}
		m, err := c.Coordinator.Machines.Add(b)
		if adminMachineError(w, err) {
			return
		}
		adminReply(w, 201, m)
	case "/admin/workers":
		adminReply(w, 200, c.Coordinator.AdminWorkers())
	case "/admin/summary":
		a.adminSummary(w, r, c)
	case "/admin/energy":
		a.Energy.Refresh(r.Context())
		s, _ := a.Energy.State(a.Coordinator.Workers())
		o := s.Object()
		rows := []any{}
		for _, row := range a.Energy.Rows() {
			rows = append(rows, object("time", row.Time, "spot_dkk_mwh", row.Spot, "dataset", row.Dataset))
		}
		o.Set("rows", rows)
		o.Set("last_success", a.Energy.LastSuccess())
		adminReply(w, 200, o)
	case "/admin/ledger/tail":
		v, err := a.Bill.Tail()
		if adminInternal(w, err) {
			return
		}
		adminReply(w, 200, v)
	case "/admin/credit":
		b := adminBody(w, r)
		if b == nil {
			return
		}
		k, _ := b["key_id"].(string)
		n, ok := integer(b["amount_msat"])
		key, err := a.Bill.Key(k)
		if adminInternal(w, err) {
			return
		}
		if key == nil || !ok || n <= 0 {
			reject(w, 400, "invalid credit")
			return
		}
		if adminInternal(w, a.Bill.Credit(k, n)) {
			return
		}
		key, err = a.Bill.Key(k)
		if adminInternal(w, err) {
			return
		}
		adminReply(w, 200, object("key_id", k, "credit_msat", key.CreditMsat))
	default:
		if strings.HasPrefix(p, "/admin/machines/") {
			if r.Method == "DELETE" {
				err := c.Coordinator.DeleteMachine(id)
				if adminMachineError(w, err) {
					return
				}
				adminReply(w, 200, object("deleted", id))
				return
			}
			b := adminBody(w, r)
			if b == nil {
				return
			}
			m, err := c.Coordinator.UpdateMachine(id, b)
			if adminMachineError(w, err) {
				return
			}
			adminReply(w, 200, m)
			return
		}
		a.adminMarkPaid(w, r, id)
	}
}
func adminInternal(w http.ResponseWriter, err error) bool {
	if err != nil {
		reject(w, 500, "Internal Server Error")
		return true
	}
	return false
}
func adminBody(w http.ResponseWriter, r *http.Request) map[string]any {
	v, err := decode(r)
	if err != nil {
		reject(w, 400, "invalid JSON")
		return nil
	}
	b, ok := v.(map[string]any)
	if !ok {
		reject(w, 400, "invalid body")
		return nil
	}
	return b
}
func adminMachineError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, machines.ErrUnavailable):
		reject(w, 503, "machine registry unavailable")
	case errors.Is(err, machines.ErrExists):
		reject(w, 409, "machine already exists")
	case errors.Is(err, machines.ErrNotFound):
		reject(w, 404, "not found")
	default:
		var pe *fs.PathError
		if errors.As(err, &pe) {
			reject(w, 500, "Internal Server Error")
		} else {
			reject(w, 400, err.Error())
		}
	}
	return true
}

const adminCSP = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

var assetName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func (a *App) adminStatic(w http.ResponseWriter, r *http.Request, dist fs.FS, name string) {
	if r.URL.Path == "/admin" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", adminCSP)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var b []byte
		var err error
		if dist != nil {
			b, err = fs.ReadFile(dist, "index.html")
		} else {
			err = fs.ErrNotExist
		}
		if err != nil {
			w.WriteHeader(503)
			b = []byte("<h1>admin UI not built</h1><p>Run <code>cd web && bun install && bun run build</code>.</p>")
		}
		w.Write(b)
		return
	}
	if !assetName.MatchString(name) || strings.HasPrefix(name, ".") || dist == nil {
		reject(w, 404, "not found")
		return
	}
	f, err := dist.Open("assets/" + name)
	if err != nil {
		reject(w, 404, "not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		reject(w, 404, "not found")
		return
	}
	media := map[string]string{".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".woff2": "font/woff2", ".png": "image/png"}[path.Ext(name)]
	if media == "" {
		media = "application/octet-stream"
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.Copy(w, f)
}
func (a *App) adminWebhook(w http.ResponseWriter, r *http.Request) {
	if a.Lightning == nil || a.Lightning.Name() != "phoenixd" {
		reject(w, 404, "not found")
		return
	}
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if peer != "127.0.0.1" && peer != "::1" {
		reject(w, 403, "loopback required")
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 16385))
	if len(b) > 16384 {
		reject(w, 413, "webhook too large")
		return
	}
	if err != nil {
		reject(w, 400, "invalid webhook event")
		return
	}
	backend, ok := a.Lightning.(WebhookBackend)
	if !ok {
		reject(w, 502, "invoice backend unavailable")
		return
	}
	credited, err := backend.Webhook(r.Context(), b, r.Header.Get("X-Phoenix-Signature"))
	switch {
	case errors.Is(err, ErrWebhookSignature):
		reject(w, 401, "invalid webhook signature")
	case errors.Is(err, ErrWebhookEvent):
		reject(w, 400, "invalid webhook event")
	case err != nil:
		reject(w, 502, "invoice backend unavailable")
	default:
		adminReply(w, 200, object("ok", true, "credited", credited))
	}
}
func (a *App) adminMarkPaid(w http.ResponseWriter, r *http.Request, id string) {
	if a.Lightning == nil || a.Lightning.Name() != "fake" {
		reject(w, 404, "not found")
		return
	}
	row, err := a.Bill.GetInvoice(id)
	if adminInternal(w, err) {
		return
	}
	if row == nil || row["backend"] != "fake" {
		reject(w, 404, "not found")
		return
	}
	backend, ok := a.Lightning.(MarkPaidBackend)
	if !ok {
		reject(w, 502, "invoice backend unavailable")
		return
	}
	if backend.MarkPaid(r.Context(), id) != nil {
		reject(w, 502, "invoice backend unavailable")
		return
	}
	row, err = a.Bill.GetInvoice(id)
	if adminInternal(w, err) {
		return
	}
	adminReply(w, 200, object("invoice_id", id, "state", row["state"]))
}
func (a *App) adminSummary(w http.ResponseWriter, r *http.Request, c AdminConfig) {
	a.Energy.Refresh(r.Context())
	var wallet any
	if backend, ok := a.Lightning.(WalletBackend); ok {
		v, err := backend.Wallet(r.Context())
		if err != nil {
			wallet = object("error", "wallet unavailable")
		} else {
			wallet = v
		}
	}
	if c.Fx == nil || a.Lightning == nil {
		reject(w, 503, "admin backend unavailable")
		return
	}
	internal := c.InternalKeys
	if internal == nil {
		internal = []string{"demo"}
	}
	sum, err := a.Bill.Summary(float64(a.Now().UnixNano())/1e9-30*86400, internal, a.Lightning.Name())
	if adminInternal(w, err) {
		return
	}
	rows, active, pending := c.Coordinator.MachineRows()
	s, _ := a.Energy.State(a.Coordinator.Workers())
	watts := 20.0
	if e := a.Energy.Config.Energy; e != nil && e.WattsPerWorker != nil {
		watts = *e.WattsPerWorker
	}
	o := object("service", a.serviceName(), "dkk_per_btc", a.Rate(), "fx", c.Fx.Status(), "pricing", a.pricing(), "watts_per_worker", watts, "wallet", wallet, "energy", s.Object(), "workers", rows, "queue", object("pending", pending, "active", active))
	o = append(o, sum...)
	adminReply(w, 200, o)
}

// Normalize typed snapshots (registry, ledger, wallet) before pyjson rendering.
// Marshal before headers so invalid adapter data fails without a partial reply.
func adminReply(w http.ResponseWriter, status int, v any) {
	normalized := adminJSON(v)
	text, err := pyjson.Marshal(normalized)
	if err != nil {
		reject(w, 500, "Internal Server Error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, text)
}
func adminJSON(v any) any {
	switch x := v.(type) {
	case pyjson.Object:
		o := pyjson.Object{}
		for _, p := range x {
			o = append(o, pyjson.Pair{Key: p.Key, Val: adminJSON(p.Val)})
		}
		return o
	case []pyjson.Object:
		o := []any{}
		for _, p := range x {
			o = append(o, adminJSON(p))
		}
		return o
	case []any:
		o := []any{}
		for _, p := range x {
			o = append(o, adminJSON(p))
		}
		return o
	case map[string]any:
		o := map[string]any{}
		for k, p := range x {
			o[k] = adminJSON(p)
		}
		return o
	case json.Number:
		if i, e := x.Int64(); e == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case nil, bool, string, int, int64, int32, uint64, float64, float32:
		return v
	}
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.UseNumber()
	var out any
	if d.Decode(&out) != nil {
		return v
	}
	return adminJSON(out)
}
