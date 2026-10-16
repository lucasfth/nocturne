package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "bad")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte("ok"))
	})
}
func request(h http.Handler, method, path, peer, forward, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = peer
	if forward != "" {
		r.Header.Set("X-Forwarded-For", forward)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func detail(t *testing.T, w *httptest.ResponseRecorder, status int, text string) {
	t.Helper()
	if w.Code != status || w.Body.String() != `{"detail":"`+text+`"}` {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "" {
		t.Fatal("Python edge does not set Retry-After")
	}
}
func TestPublicPerimeter(t *testing.T) {
	p := NewPublic(okHandler(), Config{PublicRate: 100})
	for _, path := range []string{"/worker/register", "/admin/credit", "/admin/invoices/x/mark-paid", "/docs", "/openapi.json", "/v1/credits/webhook/fake"} {
		detail(t, request(p, "POST", path, "127.0.0.1:10", "", ""), 404, "not found")
	}
	detail(t, request(p, "GET", "/admin/workers", "127.0.0.1:10", "", ""), 404, "not found")
	for _, path := range []string{"/", "/healthz"} {
		w := request(p, "GET", path, "127.0.0.1:10", "", "")
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		for key, value := range map[string]string{"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "X-Frame-Options": "DENY", "Strict-Transport-Security": "max-age=31536000", "Referrer-Policy": "no-referrer", "Server": "", "Access-Control-Allow-Origin": ""} {
			if w.Header().Get(key) != value {
				t.Fatalf("%s: %v", key, w.Header())
			}
		}
		csp := "default-src 'none'; frame-ancestors 'none'"
		if path == "/" {
			csp = LandingCSP
		}
		if w.Header().Get("Content-Security-Policy") != csp {
			t.Fatal(w.Header())
		}
	}
	for _, n := range []int64{256*1024 + 1, -1} {
		r := httptest.NewRequest("POST", "/v1/keys", strings.NewReader(strings.Repeat("x", 256*1024+1)))
		r.ContentLength = n
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		detail(t, w, 413, "body too large")
	}
}
func TestProxyTrustAndRateExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	p := NewPublic(okHandler(), Config{PublicRate: 2, Now: func() time.Time { return now }})
	for _, peer := range []string{"127.0.0.1:10", "[::1]:10", "8.8.8.8:10"} {
		if request(p, "GET", "/healthz", peer, "1.2.3.4", "").Code != 200 || request(p, "GET", "/healthz", peer, "1.2.3.4", "").Code != 200 {
			t.Fatal(peer)
		}
		detail(t, request(p, "GET", "/healthz", peer, "1.2.3.4", ""), 429, "IP rate limit")
		if peer != "8.8.8.8:10" {
			if request(p, "GET", "/healthz", peer, "1.2.3.5", "").Code != 200 {
				t.Fatal(peer)
			}
		} else {
			detail(t, request(p, "GET", "/healthz", peer, "2.3.4.8", ""), 429, "IP rate limit")
		}
		now = now.Add(time.Minute)
	}
	if request(p, "GET", "/healthz", "8.8.8.8:10", "", "").Code != 200 {
		t.Fatal("did not expire")
	}
	for _, tt := range []struct{ peer, forward, want string }{{"127.0.0.1:1", "bad,1.2.3.4", "127.0.0.1"}, {"[::1]:1", "2001:db8::1, 1.2.3.4", "2001:db8::1"}, {"8.8.8.8:1", "1.2.3.4", "8.8.8.8"}, {"", "", "unknown"}} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tt.peer
		r.Header.Set("X-Forwarded-For", tt.forward)
		if ClientIP(r) != tt.want {
			t.Fatal(tt)
		}
	}
}
func TestBodyReplay(t *testing.T) {
	p := NewPublic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { b, _ := io.ReadAll(r.Body); w.Write(b) }), Config{})
	if w := request(p, "POST", "/v1/keys", "127.0.0.1:1", "", "{not parsed}"); w.Body.String() != "{not parsed}" {
		t.Fatal(w.Body)
	}
}

func TestEmptyResponseStillSecured(t *testing.T) {
	p := NewPublic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), Config{})
	w := request(p, "GET", "/healthz", "a", "", "")
	if w.Code != 200 || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal(w.Header())
	}
}
