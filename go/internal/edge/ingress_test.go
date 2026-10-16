package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIngressClearsDeadlineBeforeDownstream(t *testing.T) {
	for _, worker := range []bool{true, false} {
		w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		called := false
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			called = true
			if w.calls == 0 || !w.deadline.IsZero() {
				t.Fatal("body deadline retained into lease/stream")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || len(body) != 4096 {
				t.Fatalf("bounded replay: %d %v", len(body), err)
			}
		})
		h := AdminIngress(next, func(*http.Request) bool { return true }, func(*http.Request) int { return 0 })
		path := "/admin/config"
		if worker {
			h = WorkerIngress(next, func(*http.Request) int { return 0 })
			path = "/worker/register"
		}
		r := httptest.NewRequest("POST", path, strings.NewReader(strings.Repeat("x", 4096)))
		r.Header.Set("Authorization", "Bearer secret")
		h.ServeHTTP(w, r)
		if !called {
			t.Fatal(w.Code)
		}
	}
}

func TestSharedReaderTotalBudget(t *testing.T) {
	now := time.Now()
	b := &advancingBody{now: &now, step: time.Second}
	r := httptest.NewRequest("POST", "/", nil)
	r.Body = b
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	_, status, _ := readBody(w, r, 65536, 15*time.Second, 5*time.Second, func() time.Time { return now })
	if status != 408 || b.calls != 15 || !w.deadline.IsZero() {
		t.Fatalf("status=%d reads=%d deadline=%v", status, b.calls, w.deadline)
	}
}

func TestIngressUnavailableBeforeRead(t *testing.T) {
	b := &advancingBody{now: new(time.Time), step: time.Second}
	r := httptest.NewRequest("POST", "/worker/register", nil)
	r.Body = b
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	h := WorkerIngress(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("poisoned admission reached handler") }), func(*http.Request) int { return 503 })
	h.ServeHTTP(w, r)
	if w.Code != 503 || b.calls != 0 || !w.deadline.IsZero() {
		t.Fatalf("status=%d reads=%d deadline=%v", w.Code, b.calls, w.deadline)
	}
}
