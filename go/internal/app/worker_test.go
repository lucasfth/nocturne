package app

import (
	"context"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"github.com/lucasfth/nocturne/go/internal/machines"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func workerFixture(t *testing.T) (*App, *WorkerCoordinator, string) {
	t.Helper()
	a, _, _, token := fixture(t)
	m, e := machines.Open(t.TempDir(), machines.Legacy{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.Add(map[string]any{"worker_id": "w", "secret_sha256": digest("worker")}); e != nil {
		t.Fatal(e)
	}
	c := NewWorkerCoordinator(WorkerConfig{Model: a.Config.Model, RequestTimeout: 2 * time.Second}, m, a.Energy)
	a.Coordinator = c
	return a, c, token
}
func wr(c *WorkerCoordinator, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/worker/"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer worker")
	w := httptest.NewRecorder()
	c.Worker().ServeHTTP(w, r)
	return w
}

const beatBody = `{"worker_id":"w","model":"Ornith-1.5-9B","ready":true,"on_ac":true,"battery_pct":90,"thermal_state":"nominal","user_active":false,"in_flight":0,"capacity":2}`

func startWorker(t *testing.T, c *WorkerCoordinator) {
	t.Helper()
	if w := wr(c, "register", `{"worker_id":"w"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := wr(c, "heartbeat", beatBody); w.Code != 200 {
		t.Fatal(w.Body)
	}
}
func submitWorker(t *testing.T, c *WorkerCoordinator, id string, stream, bulk bool) <-chan Event {
	t.Helper()
	ch, e := c.Submit(context.Background(), Job{ID: id, Body: map[string]any{"max_tokens": int64(20)}, Stream: stream, Bulk: bulk})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Cancel(id) })
	return ch
}
func getLease(t *testing.T, c *WorkerCoordinator) map[string]any {
	t.Helper()
	w := wr(c, "lease", `{"worker_id":"w"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	return parsed(t, w.Body.String())
}
func resultBody(j map[string]any, extra string) string {
	return fmt.Sprintf(`{"worker_id":"w","request_id":%q,"lease_id":%q,%s}`, j["request_id"], j["lease_id"], extra)
}
func TestRealWorkerPublicIntegration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			a, c, token := workerFixture(t)
			startWorker(t, c)
			body := chatBody
			if stream {
				body = strings.TrimSuffix(body, "}") + `,"stream":true,"stream_options":{"include_usage":true}}`
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- request(a, "POST", "/v1/chat/completions", body, token) }()
			deadline := time.After(2 * time.Second)
			for c.Pending(false) == 0 {
				select {
				case <-deadline:
					t.Fatal("public submit timeout")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			j := getLease(t, c)
			if w := wr(c, "result", resultBody(j, `"chunk":"hello","reasoning":"secret plan","final":true,"usage":{"prompt_tokens":999,"completion_tokens":999}`)); !strings.Contains(w.Body.String(), "true") {
				t.Fatal(w.Body)
			}
			select {
			case w := <-done:
				if w.Code != 200 || !strings.Contains(w.Body.String(), "hello") || strings.Contains(w.Body.String(), "secret plan") || !strings.Contains(w.Body.String(), `"completion_tokens": 8`) {
					t.Fatal(w.Code, w.Body)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("completion timeout")
			}
			if w := wr(c, "result", resultBody(j, `"final":true`)); !strings.Contains(w.Body.String(), "false") {
				t.Fatal(w.Body)
			}
		})
	}
}
func TestRetryResetAndStaleLease(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	ch := submitWorker(t, c, "retry", false, false)
	j := getLease(t, c)
	wr(c, "result", resultBody(j, `"chunk":"discard","reasoning":"old"`))
	wr(c, "fail", resultBody(j, `"error":"failed"`))
	if c.Pending(false) != 1 {
		t.Fatal("not requeued")
	}
	if w := wr(c, "result", resultBody(j, `"chunk":"stale","final":true`)); !strings.Contains(w.Body.String(), "false") {
		t.Fatal(w.Body)
	}
	j2 := getLease(t, c)
	if j2["lease_id"] == j["lease_id"] {
		t.Fatal("reused lease")
	}
	wr(c, "result", resultBody(j2, `"chunk":"new","final":true`))
	e := <-ch
	if e.Text != "new" || e.Reasoning != "" {
		t.Fatal(e)
	}
	ch = submitWorker(t, c, "twice", false, false)
	j = getLease(t, c)
	wr(c, "fail", resultBody(j, `"error":"x"`))
	j = getLease(t, c)
	wr(c, "fail", resultBody(j, `"error":"x"`))
	if e := <-ch; e.Error != "worker_interrupted" {
		t.Fatal(e)
	}
}
func TestMalformedWorkerResultsAtomic(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	ch := submitWorker(t, c, "bad", false, false)
	j := getLease(t, c)
	for _, extra := range []string{`"chunk":1`, `"reasoning":null`, `"chunk":"` + strings.Repeat("😀", 16385) + `"`, `"final":true,"usage":[]`, `"chunk":"bad","final":true,"usage":{"prompt_tokens":true}`, `"final":true,"usage":{"completion_tokens":-1}`, `"final":true,"usage":{"completion_tokens":1.0}`} {
		w := wr(c, "result", resultBody(j, extra))
		if w.Code != 400 {
			t.Fatal(extra[:min(70, len(extra))], w.Code, w.Body)
		}
	}
	wr(c, "result", resultBody(j, `"chunk":"ok","final":true`))
	if e := <-ch; e.Text != "ok" {
		t.Fatal(e)
	}
}
func TestWorkerHeartbeatValidationAndHysteresis(t *testing.T) {
	_, c, _ := workerFixture(t)
	if w := wr(c, "heartbeat", beatBody); w.Code != 409 {
		t.Fatal(w.Code)
	}
	startWorker(t, c)
	for _, extra := range []string{`"metrics":[]`, `"metrics":{"x":"bad"}`, `"metrics":{"x":true}`, `"tokens_per_sec":true`, `"tokens_per_sec":0`, `"unknown":1`} {
		w := wr(c, "heartbeat", strings.TrimSuffix(beatBody, "}")+","+extra+"}")
		if w.Code != 400 {
			t.Fatal(extra, w.Code)
		}
	}
	for _, pct := range []int{25, 26} {
		b := strings.ReplaceAll(beatBody, `"on_ac":true`, `"on_ac":false`)
		b = strings.ReplaceAll(b, `"battery_pct":90`, fmt.Sprintf(`"battery_pct":%d`, pct))
		w := wr(c, "heartbeat", b)
		if parsed(t, w.Body.String())["eligible"] != (pct > 25) {
			t.Fatal(w.Body)
		}
	}
	ch := submitWorker(t, c, "battery", true, false)
	getLease(t, c)
	b := strings.ReplaceAll(beatBody, `"on_ac":true`, `"on_ac":false`)
	b = strings.ReplaceAll(b, `"battery_pct":90`, `"battery_pct":22`)
	if w := wr(c, "heartbeat", b); parsed(t, w.Body.String())["eligible"] != true {
		t.Fatal(w.Body)
	}
	b = strings.ReplaceAll(b, `"battery_pct":22`, `"battery_pct":20`)
	wr(c, "heartbeat", b)
	if e := <-ch; e.Error != "worker_interrupted" {
		t.Fatal(e)
	}
}
func TestBulkPaidPriority(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	submitWorker(t, c, "bulk", false, true)
	submitWorker(t, c, "paid", false, false)
	if j := getLease(t, c); j["request_id"] != "paid" {
		t.Fatal(j)
	}
	if j := getLease(t, c); j["request_id"] != "bulk" {
		t.Fatal(j)
	}
}
func TestConcurrentPollCancellationAndBroadcast(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	var wg sync.WaitGroup
	ready := make(chan struct{}, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			r := httptest.NewRequest("POST", "/worker/lease", strings.NewReader(`{"worker_id":"w"}`)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer worker")
			ready <- struct{}{}
			time.AfterFunc(20*time.Millisecond, cancel)
			c.Worker().ServeHTTP(httptest.NewRecorder(), r)
		}()
	}
	for i := 0; i < 12; i++ {
		<-ready
	}
	wg.Wait()
	done := make(chan struct{})
	go func() { wr(c, "lease", `{"worker_id":"w"}`); close(done) }()
	time.Sleep(10 * time.Millisecond)
	c.DropWorker("w")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll did not wake")
	}
}
func TestPollReauthAndReregister(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(fmt.Sprint(rotate), func(t *testing.T) {
			_, c, _ := workerFixture(t)
			startWorker(t, c)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- wr(c, "lease", `{"worker_id":"w"}`) }()
			time.Sleep(20 * time.Millisecond)
			if rotate {
				if _, e := c.Machines.Update("w", map[string]any{"secret_sha256": digest("new")}); e != nil {
					t.Fatal(e)
				}
				c.DropWorker("w")
			} else {
				wr(c, "register", `{"worker_id":"w"}`)
			}
			select {
			case w := <-done:
				if rotate && w.Code != 401 {
					t.Fatal(w.Code, w.Body)
				}
				if !rotate && !strings.Contains(w.Body.String(), `"job": null`) {
					t.Fatal(w.Body)
				}
			case <-time.After(time.Second):
				t.Fatal("poll stuck")
			}
		})
	}
}
func TestMaintenanceTimeoutCleanupAndStale(t *testing.T) {
	_, c, _ := workerFixture(t)
	now := c.Config.Monotonic()
	c.Config.Monotonic = func() float64 { return now }
	startWorker(t, c)
	ch := submitWorker(t, c, "stale", true, false)
	getLease(t, c)
	now += 46
	c.Maintain()
	if e := <-ch; e.Error != "worker_interrupted" {
		t.Fatal(e)
	}
	ch = submitWorker(t, c, "timeout", false, false)
	now += 3
	c.Maintain()
	if e := <-ch; e.Error != "request_timeout" {
		t.Fatal(e)
	}
	now += 61
	c.Maintain()
	c.mu.Lock()
	n := len(c.jobs)
	q := len(c.queue)
	c.mu.Unlock()
	if n != 0 || q != 0 {
		t.Fatal(n, q)
	}
}
func TestBoundedOrderedOutputAndCancellation(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	ch := submitWorker(t, c, "overflow", true, false)
	j := getLease(t, c)
	for i := 0; i < 64; i++ {
		if w := wr(c, "result", resultBody(j, fmt.Sprintf(`"chunk":%q`, fmt.Sprint(i)))); !strings.Contains(w.Body.String(), "true") {
			t.Fatal(w.Body)
		}
	}
	if w := wr(c, "result", resultBody(j, `"chunk":"overflow"`)); !strings.Contains(w.Body.String(), "false") {
		t.Fatal(w.Body)
	}
	for i := 0; i < 64; i++ {
		if e := <-ch; e.Text != fmt.Sprint(i) {
			t.Fatal(i, e)
		}
	}
	if e := <-ch; e.Error != "output_limit" {
		t.Fatal(e)
	}
	ch = submitWorker(t, c, "cancel", true, false)
	j = getLease(t, c)
	c.Cancel("cancel")
	c.Cancel("cancel")
	if _, open := <-ch; open {
		t.Fatal("not closed")
	}
	if w := wr(c, "result", resultBody(j, `"final":true`)); !strings.Contains(w.Body.String(), "false") {
		t.Fatal(w.Body)
	}
}
func TestWorkerRoutesAuthAndLimits(t *testing.T) {
	_, c, _ := workerFixture(t)
	for _, path := range []string{"register", "heartbeat", "lease", "result", "fail"} {
		r := httptest.NewRequest("POST", "/worker/"+path, strings.NewReader(`{"worker_id":"w"}`))
		w := httptest.NewRecorder()
		c.Worker().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal(path, w.Code)
		}
		r = httptest.NewRequest("GET", "/worker/"+path, nil)
		w = httptest.NewRecorder()
		c.Worker().ServeHTTP(w, r)
		if w.Code != 405 {
			t.Fatal(path, w.Code)
		}
	}
	startWorker(t, c)
	ch := submitWorker(t, c, "limit", false, false)
	j := getLease(t, c)
	w := wr(c, "result", resultBody(j, `"reasoning":"`+strings.Repeat("x", 321)+`"`))
	if !strings.Contains(w.Body.String(), "false") {
		t.Fatal(w.Body)
	}
	if e := <-ch; e.Error != "output_limit" {
		t.Fatal(e)
	}
	// Unicode limits count characters, not UTF-8 bytes.
	ch = submitWorker(t, c, "unicode", false, false)
	j = getLease(t, c)
	wr(c, "result", resultBody(j, `"chunk":"😀😀","final":true`))
	if e := <-ch; e.Text != "😀😀" {
		t.Fatal(e)
	}
}

func TestLeaseTimeoutIsTwentySeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("real protocol timeout")
	}
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	start := time.Now()
	w := wr(c, "lease", `{"worker_id":"w"}`)
	elapsed := time.Since(start)
	if elapsed < 20*time.Second || elapsed > 23*time.Second || w.Code != 200 || !strings.Contains(w.Body.String(), `"job": null`) {
		t.Fatal(elapsed, w.Code, w.Body)
	}
}
func TestBulkNeverTakesOnlySlot(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	wr(c, "heartbeat", strings.ReplaceAll(beatBody, `"capacity":2`, `"capacity":1`))
	submitWorker(t, c, "bulk-only", false, true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- wr(c, "lease", `{"worker_id":"w"}`) }()
	select {
	case w := <-done:
		t.Fatal("bulk leased only slot", w.Body)
	case <-time.After(25 * time.Millisecond):
	}
	wr(c, "heartbeat", beatBody)
	select {
	case w := <-done:
		if !strings.Contains(w.Body.String(), "bulk-only") {
			t.Fatal(w.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not wake poll")
	}
}
func TestConcurrentDuplicateFinalAndContextCancel(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	ch := submitWorker(t, c, "duplicate", true, false)
	j := getLease(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); wr(c, "result", resultBody(j, `"chunk":"once","final":true`)) }()
	}
	wg.Wait()
	count := 0
	for e := range ch {
		count++
		if e.Text != "once" || !e.Final {
			t.Fatal(e)
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, e := c.Submit(ctx, Job{ID: "ctx", Body: map[string]any{"max_tokens": int64(20)}})
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("unexpected event")
		}
	case <-time.After(time.Second):
		t.Fatal("context did not cancel")
	}
}
func TestScheduleGraceInterruptsAndRunStops(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	ch := submitWorker(t, c, "grace", true, false)
	getLease(t, c)
	wall := time.Date(2026, 10, 15, 18, 19, 0, 0, time.UTC)
	grace := 20.0
	c.Energy.Config.Schedule = &energy.Schedule{TZ: "UTC", Start: "08:00", End: "18:00", GraceMin: &grace}
	c.Energy.Clock = func() time.Time { return wall }
	c.Maintain()
	select {
	case e := <-ch:
		t.Fatal("interrupted within grace", e)
	default:
	}
	wall = wall.Add(time.Minute)
	c.Maintain()
	if e := <-ch; e.Error != "worker_interrupted" {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}
func TestBroadcastWakesAllRegisteredWorkers(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	if _, e := c.Machines.Add(map[string]any{"worker_id": "w2", "secret_sha256": digest("worker2")}); e != nil {
		t.Fatal(e)
	}
	call := func(id, secret, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/worker/"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		c.Worker().ServeHTTP(w, r)
		return w
	}
	call("w2", "worker2", "register", `{"worker_id":"w2"}`)
	call("w2", "worker2", "heartbeat", strings.ReplaceAll(beatBody, `"w"`, `"w2"`))
	done := make(chan *httptest.ResponseRecorder, 2)
	go func() { done <- wr(c, "lease", `{"worker_id":"w"}`) }()
	go func() { done <- call("w2", "worker2", "lease", `{"worker_id":"w2"}`) }()
	time.Sleep(20 * time.Millisecond)
	submitWorker(t, c, "broadcast1", false, false)
	submitWorker(t, c, "broadcast2", false, false)
	ids := map[any]bool{}
	for i := 0; i < 2; i++ {
		select {
		case w := <-done:
			ids[parsed(t, w.Body.String())["request_id"]] = true
		case <-time.After(time.Second):
			t.Fatal("lost broadcast")
		}
	}
	if len(ids) != 2 {
		t.Fatal(ids)
	}
}

type blockedWorkerWriter struct {
	header           http.Header
	entered, release chan struct{}
}

func (w *blockedWorkerWriter) Header() http.Header         { return w.header }
func (w *blockedWorkerWriter) WriteHeader(int)             { close(w.entered); <-w.release }
func (w *blockedWorkerWriter) Write(p []byte) (int, error) { return len(p), nil }
func TestSlowWorkerResponseDoesNotHoldCoordinatorLock(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	writer := &blockedWorkerWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	defer close(writer.release)
	r := httptest.NewRequest("POST", "/worker/register", strings.NewReader(`{"worker_id":"w"}`))
	r.Header.Set("Authorization", "Bearer worker")
	done := make(chan struct{})
	go func() { c.Worker().ServeHTTP(writer, r); close(done) }()
	<-writer.entered
	cancelled := make(chan struct{})
	go func() { c.DropWorker("w"); close(cancelled) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("network writer held coordinator mutex")
	}
}
