package app

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lucasfth/nocturne/go/internal/edge"
)

func TestIngressDirectLimits(t *testing.T) {
	a, c, cfg := adminFixture(t)
	for _, tc := range []struct {
		path  string
		limit int
		admin bool
	}{
		{"/worker/register", 4096, false}, {"/worker/lease", 4096, false}, {"/worker/fail", 4096, false},
		{"/worker/heartbeat", 65536, false}, {"/worker/result", 262144, false},
		{"/admin/config", 4096, true}, {"/admin/credit", 65536, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			h := c.Worker()
			if tc.admin {
				h = a.Admin(cfg)
			}
			for _, unknown := range []bool{false, true} {
				r := httptest.NewRequest("POST", tc.path, strings.NewReader(strings.Repeat(" ", tc.limit+1)))
				token := "worker"
				if tc.admin {
					token = "admin"
				}
				r.Header.Set("Authorization", "Bearer "+token)
				if unknown {
					r.ContentLength = -1
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 413 {
					t.Fatalf("unknown=%v: %d", unknown, w.Code)
				}
			}
		})
	}
	for _, token := range []string{"", strings.Repeat("x", 513), "bad\ttoken", "bad\x7ftoken"} {
		r := httptest.NewRequest("POST", "/worker/register", strings.NewReader("not JSON"))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		c.Worker().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	for _, id := range []string{strings.Repeat("a", 33), "UPPER", "bad/id", ""} {
		r := httptest.NewRequest("POST", "/worker/register", strings.NewReader(fmt.Sprintf(`{"worker_id":%q}`, id)))
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		c.Worker().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
}

func TestIngressAdminBrowserMutations(t *testing.T) {
	a, _, cfg := adminFixture(t)
	for _, path := range []string{"/admin/credit", "/admin/machines", "/admin/machines/w", "/admin/config", "/admin/invoices/x/mark-paid"} {
		for _, headers := range []map[string]string{
			{"Origin": "https://evil.example", "Content-Type": "application/json"},
			{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"},
			{"Content-Type": "text/plain"},
		} {
			r := httptest.NewRequest("POST", path, strings.NewReader("{}"))
			r.Header.Set("Authorization", "Bearer admin")
			for k, v := range headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			a.Admin(cfg).ServeHTTP(w, r)
			if w.Code != 403 && w.Code != 415 {
				t.Fatalf("%s: %d", path, w.Code)
			}
		}
	}
	r := httptest.NewRequest("POST", "/admin/lightning/webhook", strings.NewReader("{}"))
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	a.Admin(cfg).ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("webhook exemption: %d", w.Code)
	}
}

// Real TCP and chunk framing exercise net/http's native deadline and body-close
// behavior, not a cooperative fake reader. Public stays on a separate listener.
func TestIngressNativeChunked(t *testing.T) {
	a, c, cfg := adminFixture(t)
	servers, err := edge.NewServers(edge.Handlers{Public: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), Worker: c.Worker(), Admin: a.Admin(cfg)}, edge.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	public := httptest.NewServer(servers.Public.Handler)
	defer public.Close()
	for _, tc := range []struct {
		name, path string
		h          http.Handler
		limit      int
	}{
		{"worker", "/worker/register", servers.Worker.Handler, 4096},
		{"admin", "/admin/credit", servers.Admin.Handler, 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.h)
			defer s.Close()
			for _, stalled := range []bool{false, true} {
				conn, err := net.Dial("tcp", s.Listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(9 * time.Second))
				token := "worker"
				if tc.name == "admin" {
					token = "admin"
				}
				fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nTransfer-Encoding: chunked\r\nContent-Type: application/json\r\n\r\n", tc.path, token)
				if stalled {
					fmt.Fprint(conn, "100\r\nx")
				} else {
					payload := strings.Repeat("x", tc.limit+1)
					fmt.Fprintf(conn, "%x\r\n%s\r\n0\r\n\r\n", len(payload), payload)
				}
				client := &http.Client{Timeout: time.Second}
				resp, err := client.Get(public.URL + "/healthz")
				if err != nil {
					t.Fatal("public blocked:", err)
				}
				if resp.StatusCode != 204 {
					t.Fatalf("public: %d", resp.StatusCode)
				}
				resp.Body.Close()
				resp, err = http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					conn.Close()
					t.Fatal(err)
				}
				want := 413
				if stalled {
					want = 408
				}
				if resp.StatusCode != want {
					t.Errorf("stalled=%v: %d", stalled, resp.StatusCode)
				}
				resp.Body.Close()
				conn.Close()
			}
		})
	}
}

// A syntactically valid but unknown credential must never reach the body reader.
type unreadAuthBody struct{ reads int }

func (b *unreadAuthBody) Read([]byte) (int, error) { b.reads++; return 0, fmt.Errorf("must not read") }
func (b *unreadAuthBody) Close() error             { return nil }
func TestIngressAuthenticatesBeforeBody(t *testing.T) {
	a, c, cfg := adminFixture(t)
	for _, path := range []string{"/admin/credit", "/admin/config", "/worker/register", "/worker/lease", "/worker/benchmark/lease"} {
		for _, token := range []string{"", "unknown-valid-bearer"} {
			b := &unreadAuthBody{}
			r := httptest.NewRequest("POST", path, nil)
			r.Body = b
			r.ContentLength = -1
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			h := c.Worker()
			if strings.HasPrefix(path, "/admin/") {
				h = a.Admin(cfg)
			}
			h.ServeHTTP(w, r)
			if w.Code != 401 || b.reads != 0 {
				t.Fatalf("%s status=%d reads=%d", path, w.Code, b.reads)
			}
		}
	}
	// Admission cannot bind a credential to a different worker ID.
	if w := wr(c, "register", `{"worker_id":"other"}`); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, mutation := range []map[string]any{{"secret_sha256": digest("rotated")}, {"enabled": false}} {
		if _, err := c.Machines.Update("w", mutation); err != nil {
			t.Fatal(err)
		}
		b := &unreadAuthBody{}
		r := httptest.NewRequest("POST", "/worker/register", nil)
		r.Body = b
		r.Header.Set("Authorization", "Bearer worker")
		w := httptest.NewRecorder()
		c.Worker().ServeHTTP(w, r)
		if w.Code != 401 || b.reads != 0 {
			t.Fatal("stale credential admitted")
		}
	}
}
