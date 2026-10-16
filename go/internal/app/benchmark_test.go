package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/benchmark"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func benchmarkFixture(t *testing.T) (*App, *WorkerCoordinator, AdminConfig) {
	a, c, cfg := adminFixture(t)
	var e error
	c.Benchmarks, e = benchmark.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Benchmarks.Close() })
	c.BenchmarkTargets = map[string]BenchmarkTarget{"w": {ModelSHA256: strings.Repeat("a", 64), BuildDigest: strings.Repeat("b", 64)}}
	startWorker(t, c)
	return a, c, cfg
}
func TestBenchmarkDisposableHTTP(t *testing.T) {
	a, c, cfg := benchmarkFixture(t)
	// Real disposable HTTP listeners, actual route/auth/store/scheduler integration.
	admin := httptest.NewServer(a.Admin(cfg))
	defer admin.Close()
	worker := httptest.NewServer(c.Worker())
	defer worker.Close()
	call := func(base, path, token string, body any, status int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != status {
			t.Fatalf("%s: %d %s", path, res.StatusCode, data)
		}
		var v map[string]any
		json.Unmarshal(data, &v)
		return v
	}
	path := "/admin/machines/w/benchmarks"
	call(admin.URL, path, "wrong", map[string]any{}, 401)
	j := call(admin.URL, path, "admin", map[string]any{}, 202)
	id := j["id"].(string)
	if strings.Contains(fmt.Sprint(j), "lease") {
		t.Fatal("lease leaked")
	}
	if c.Workers()["w"].Policy.Ready {
		t.Fatal("not fenced")
	}
	if w := wr(c, "heartbeat", beatBody); parsed(t, w.Body.String())["serve"] != false {
		t.Fatal(w.Body)
	}
	lease := call(worker.URL, "/worker/benchmark/lease", "worker", map[string]any{"worker_id": "w"}, 200)
	token := lease["lease"].(string)
	update := map[string]any{"worker_id": "w", "job_id": id, "lease": token, "update": map[string]any{"sequence": 1, "state": "running", "progress": "measuring"}}
	call(worker.URL, "/worker/benchmark/update", "worker", update, 200)
	call(worker.URL, "/worker/benchmark/update", "worker", update, 409)
	call(admin.URL, path+"/"+id+"/cancel", "admin", map[string]any{}, 200)
	if fenced, e := c.Benchmarks.Fenced("w"); !fenced || e != nil {
		t.Fatal(fenced, e)
	}
	call(worker.URL, "/worker/benchmark/update", "worker", map[string]any{"worker_id": "w", "job_id": id, "lease": token, "update": map[string]any{"sequence": 2, "state": "cancelled", "progress": "done", "restored": true}}, 200)
	if fenced, e := c.Benchmarks.Fenced("w"); fenced || e != nil {
		t.Fatal(fenced, e)
	}
	call(admin.URL, path+"/"+id+"/apply", "admin", map[string]any{}, 409)
}
func TestBenchmarkStrictAuthAndRotation(t *testing.T) {
	a, c, cfg := benchmarkFixture(t)
	path := "/admin/machines/w/benchmarks"
	for _, body := range []string{`{"command":"id"}`, `{"advanced":{"context":8192,"context":32768}}`, `{"advanced":{"Context":8192}}`, `{"advanced":{"parallel":3}}`, `{"advanced":{"url":"https://example.invalid"}}`, `{} {}`, `{"advanced":[]}`} {
		if w := ar(a, cfg, "POST", path, body, "admin", ""); w.Code != 400 {
			t.Fatal(body, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Origin", "https://evil.invalid")
	w := httptest.NewRecorder()
	a.Admin(cfg).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	started := ar(a, cfg, "POST", path, `{}`, "admin", "")
	j := parsed(t, started.Body.String())
	id := j["id"].(string)
	c.Machines.Add(map[string]any{"worker_id": "other", "secret_sha256": digest("other")})
	if w = ar(a, cfg, "GET", "/admin/machines/other/benchmarks/"+id, "", "admin", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	c.UpdateMachine("w", map[string]any{"secret_sha256": digest("rotated")})
	if w = wr(c, "benchmark/lease", `{"worker_id":"w"}`); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if fenced, e := c.Benchmarks.Fenced("w"); !fenced || e != nil {
		t.Fatal(fenced, e)
	}
	delete(c.BenchmarkTargets, "w")
	ar(a, cfg, "POST", path+"/"+id+"/cancel", `{}`, "admin", "")
	if w = ar(a, cfg, "POST", path, `{}`, "admin", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
}
func TestBenchmarkDrainAndUnsafe(t *testing.T) {
	a, c, cfg := benchmarkFixture(t)
	submitWorker(t, c, "active", true, false)
	getLease(t, c)
	w := ar(a, cfg, "POST", "/admin/machines/w/benchmarks", `{}`, "admin", "")
	if w.Code != 202 {
		t.Fatal(w.Body)
	}
	if w = wr(c, "benchmark/lease", `{"worker_id":"w"}`); w.Code != 409 {
		t.Fatal(w.Body)
	}
	c.Cancel("active")
	wr(c, "heartbeat", strings.Replace(beatBody, `"on_ac":true`, `"on_ac":false`, 1))
	if w = wr(c, "benchmark/lease", `{"worker_id":"w"}`); w.Code != 409 {
		t.Fatal(w.Body)
	}
}

type blockedBenchmarkReader struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockedBenchmarkReader) Read(p []byte) (int, error) {
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	<-b.release
	return 0, io.EOF
}
func (b *blockedBenchmarkReader) Close() error { return nil }

type blockedBenchmarkWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (b *blockedBenchmarkWriter) Header() http.Header { return b.header }
func (b *blockedBenchmarkWriter) WriteHeader(int)     {}
func (b *blockedBenchmarkWriter) Write(p []byte) (int, error) {
	close(b.entered)
	<-b.release
	return len(p), nil
}
func TestBenchmarkNetworkNeverHoldsScheduler(t *testing.T) {
	a, c, cfg := benchmarkFixture(t)
	check := func() {
		t.Helper()
		done := make(chan struct{})
		go func() { c.Workers(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("network held scheduler lock")
		}
	}
	body := &blockedBenchmarkReader{make(chan struct{}), make(chan struct{})}
	r := httptest.NewRequest("POST", "/admin/machines/w/benchmarks", body)
	r.Header.Set("Authorization", "Bearer admin")
	done := make(chan struct{})
	go func() { a.Admin(cfg).ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	<-body.entered
	check()
	close(body.release)
	<-done
	writer := &blockedBenchmarkWriter{make(http.Header), make(chan struct{}), make(chan struct{})}
	r = httptest.NewRequest("GET", "/admin/machines/w/benchmarks", nil)
	r.Header.Set("Authorization", "Bearer admin")
	done = make(chan struct{})
	go func() { a.Admin(cfg).ServeHTTP(writer, r); close(done) }()
	<-writer.entered
	check()
	close(writer.release)
	<-done
}
func TestBenchmarkUnicodeAndBrowserGuard(t *testing.T) {
	a, _, cfg := benchmarkFixture(t)
	if w := ar(a, cfg, "POST", "/admin/machines/w/benchmarks", `{"advanced":{"budget_seconds":30,"budget_\u017feconds":600}}`, "admin", ""); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	for _, h := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Origin": "http://user@example.com"}, {"Origin": "http://example.com?x"}, {"Origin": "http://example.com#x"}} {
		r := httptest.NewRequest("POST", "http://example.com/admin/machines/w/benchmarks", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer admin")
		for k, v := range h {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		a.Admin(cfg).ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(h, w.Code)
		}
	}
}
