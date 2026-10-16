package edge

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestServerFoundation(t *testing.T) {
	if _, err := NewServers(Handlers{}, Config{}, nil); err == nil {
		t.Fatal("accepted missing routes")
	}
	h := okHandler()
	s, err := NewServers(Handlers{h, h, h}, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for server, addr := range map[*http.Server]string{s.Public: "127.0.0.1:8700", s.Worker: "127.0.0.1:8701", s.Admin: "127.0.0.1:8702"} {
		if server.Addr != addr || server.ReadHeaderTimeout != 10*time.Second || server.IdleTimeout != 5*time.Second || server.ReadTimeout != 0 || server.WriteTimeout != 0 {
			t.Fatalf("%+v", server)
		}
	}
}
func TestConcurrency(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	h := ConcurrencyLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(204) }), 1)
	first := httptest.NewRecorder()
	wg.Go(func() { h.ServeHTTP(first, httptest.NewRequest("GET", "/healthz", nil)) })
	<-entered
	w := request(h, "GET", "/healthz", "127.0.0.1:1", "", "")
	if w.Code != 503 || w.Body.String() != "Service Unavailable" || w.Header().Get("Retry-After") != "" || w.Header().Get("Connection") != "close" {
		t.Fatalf("%+v", w)
	}
	close(release)
	wg.Wait()
	if first.Code != 204 {
		t.Fatal(first.Code)
	}
}
func TestTailnetGuard(t *testing.T) {
	h := TailnetGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/summary" && !AuthenticateDigest(r, Digest("admin-test")) {
			reject(w, 401, "invalid admin credentials")
			return
		}
		w.WriteHeader(200)
	}), []string{"lucasfth@github"})
	for _, tt := range []struct {
		path, login string
		status      int
	}{{"/admin/summary", "someone@else", 403}, {"/admin/summary", "lucasfth@github", 401}, {"/admin", "lucasfth@github", 200}, {"/admin/lightning/webhook", "lucasfth@github", 404}, {"/admin/lightning/webhook", "", 200}} {
		r := httptest.NewRequest("POST", tt.path, nil)
		if tt.login != "" {
			r.Header.Set("Tailscale-User-Login", tt.login)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Fatalf("%+v => %d", tt, w.Code)
		}
	}
}
func TestAuthHelpers(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	for _, tt := range []struct {
		header, want string
		ok           bool
	}{{"Bearer admin-test", Digest("admin-test"), true}, {"bearer admin-test", Digest("admin-test"), false}, {"Bearer admin-test", "", false}, {"Bearer bad", Digest("admin-test"), false}} {
		r.Header.Set("Authorization", tt.header)
		if AuthenticateDigest(r, tt.want) != tt.ok {
			t.Fatal(tt.header)
		}
	}
}
func TestRateCapacityAndSharedChatCounter(t *testing.T) {
	now := time.Unix(100, 0)
	p := NewPublic(okHandler(), Config{PublicRate: 1, ChatRate: 3, Now: func() time.Time { return now }})
	if request(p, "POST", "/v1/chat/completions", "a", "", "").Code != 200 {
		t.Fatal("chat")
	}
	detail(t, request(p, "GET", "/healthz", "a", "", ""), 429, "IP rate limit")
	if request(p, "POST", "/v1/chat/completions", "a", "", "").Code != 200 {
		t.Fatal("chat shares counter but has higher ceiling")
	}
	p.rates = map[string][]time.Time{}
	for i := 0; i < 4097; i++ {
		p.rates[time.Unix(int64(i), 0).String()] = []time.Time{now}
	}
	detail(t, request(p, "GET", "/healthz", "new", "", ""), 429, "rate limiter capacity")
	now = now.Add(time.Minute)
	if request(p, "GET", "/healthz", "new", "", "").Code != 200 {
		t.Fatal("stale entries not pruned")
	}
}
