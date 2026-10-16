package app

import (
	"bytes"
	"encoding/json"
	"github.com/lucasfth/nocturne/go/internal/edge"
	"github.com/lucasfth/nocturne/go/internal/machines"
	"github.com/lucasfth/nocturne/go/internal/policy"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Worker exposes only the five Python worker POST routes, on a separate listener.
func (c *WorkerCoordinator) Worker() http.Handler {
	return edge.WorkerIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stage the bounded JSON response so an arbitrarily slow network writer
		// never holds the coordinator lock or prevents cancellation/heartbeats.
		response := &workerResponse{header: make(http.Header), status: 200}
		c.serveWorker(response, r)
		if r.Context().Err() != nil {
			return
		}
		for k, v := range response.header {
			w.Header()[k] = v
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
	}), func(r *http.Request) int {
		if r.Method != "POST" {
			return 0
		}
		// This is only preliminary admission. The parsed worker ID and current
		// secret are checked again under the coordinator lock before mutation.
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ok, err := c.Machines.KnownSecret(digest(token))
		if err != nil {
			return http.StatusServiceUnavailable
		}
		if !ok {
			return http.StatusUnauthorized
		}
		return 0
	})
}

type workerResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *workerResponse) Header() http.Header         { return w.header }
func (w *workerResponse) WriteHeader(status int)      { w.status = status }
func (w *workerResponse) Write(p []byte) (int, error) { return w.body.Write(p) }

func (c *WorkerCoordinator) authWorker(r *http.Request, id string) bool {
	token := ""
	if s := r.Header.Get("Authorization"); strings.HasPrefix(s, "Bearer ") {
		token = s[7:]
	}
	return machines.ValidID(id) && c.Machines.CheckSecret(id, digest(token))
}
func (c *WorkerCoordinator) serveWorker(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Path
	if strings.HasPrefix(route, "/worker/benchmark/") {
		c.serveBenchmarkWorker(w, r)
		return
	}
	known := route == "/worker/register" || route == "/worker/heartbeat" || route == "/worker/lease" || route == "/worker/result" || route == "/worker/fail"
	if !known {
		reject(w, 404, "Not Found")
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		reject(w, 405, "Method Not Allowed")
		return
	}
	v, err := decode(r)
	b, ok := v.(map[string]any)
	if err != nil {
		reject(w, 400, "invalid JSON")
		return
	}
	if !ok {
		reject(w, 400, "invalid body")
		return
	}
	id, _ := b["worker_id"].(string)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Machines.Available() {
		reject(w, 503, "machine registry unavailable")
		return
	}
	if !c.authWorker(r, id) {
		reject(w, 401, "invalid worker credentials")
		return
	}
	switch route {
	case "/worker/register":
		c.interrupt(id)
		c.workers[id] = &workerSession{capacity: 1, jobs: map[string]bool{}}
		c.workers[id].view.Policy.Seen = c.Config.Monotonic()
		c.signal()
		reply(w, 200, object("registered", true))
	case "/worker/heartbeat":
		c.heartbeat(w, b, id)
	case "/worker/lease":
		c.lease(w, r, id)
	default:
		rid, _ := b["request_id"].(string)
		lid, _ := b["lease_id"].(string)
		j := c.jobs[rid]
		if j == nil || c.closed(j) || j.worker != id || j.lease != lid || lid == "" {
			reply(w, 200, object("accepted", false))
			return
		}
		if route == "/worker/fail" {
			c.interrupt(id)
			reply(w, 200, object("accepted", true))
			return
		}
		c.result(w, b, j)
	}
}
func realNumber(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, e := n.Float64()
	return f, e == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}
func validMetrics(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) > 16 {
		return false
	}
	for k, v := range m {
		if utf8.RuneCountInString(k) > 32 {
			return false
		}
		if v != nil {
			n, ok := realNumber(v)
			if !ok || math.Abs(n) >= 1e7 {
				return false
			}
		}
	}
	return true
}
func (c *WorkerCoordinator) heartbeat(w http.ResponseWriter, b map[string]any, id string) {
	s := c.workers[id]
	if s == nil {
		reject(w, 409, "register first")
		return
	}
	required := []string{"worker_id", "model", "ready", "on_ac", "battery_pct", "thermal_state", "user_active", "in_flight", "capacity"}
	valid := true
	allowed := map[string]bool{"tokens_per_sec": true, "metrics": true}
	for _, k := range required {
		allowed[k] = true
		if _, ok := b[k]; !ok {
			valid = false
		}
	}
	for k := range b {
		if !allowed[k] {
			valid = false
		}
	}
	ready, rOK := b["ready"].(bool)
	ac, aOK := b["on_ac"].(bool)
	user, uOK := b["user_active"].(bool)
	battery, bOK := realNumber(b["battery_pct"])
	capacity, cOK := integer(b["capacity"])
	flight, fOK := integer(b["in_flight"])
	thermal, _ := b["thermal_state"].(string)
	if v, ok := b["metrics"]; ok && !validMetrics(v) {
		valid = false
	}
	var tps *float64
	if v, ok := b["tokens_per_sec"]; ok {
		n, ok := realNumber(v)
		if !ok || n <= 0 || n > 100000 {
			valid = false
		}
		tps = &n
	}
	model, _ := b["model"].(string)
	if !valid || model != c.Config.Model || !rOK || !aOK || !uOK || !bOK || battery < 0 || battery > 100 || !cOK || capacity < 1 || capacity > 4 || !fOK || flight < 0 || flight > capacity || (thermal != "nominal" && thermal != "fair" && thermal != "serious" && thermal != "critical") {
		reject(w, 400, "invalid heartbeat")
		return
	}
	s.view.Policy = policy.Worker{OnAC: ac, BatteryPct: battery, ThermalState: thermal, UserActive: &user, Ready: ready, Seen: c.Config.Monotonic(), Jobs: len(s.jobs)}
	if tps != nil {
		s.view.TokensPerSec = tps
	}
	s.capacity = int(capacity)
	if s.heartbeat == nil {
		s.heartbeat = map[string]any{}
	}
	for k, v := range b {
		s.heartbeat[k] = v
	}
	if policy.Unsafe(s.view.Policy) {
		c.interrupt(id)
	}
	c.signal()
	p := s.view.Policy
	p.Jobs = len(s.jobs)
	serve := c.Energy.Serves(p) && !c.benchmarkFenced(id)
	active := []any{}
	for rid := range s.jobs {
		active = append(active, rid)
	}
	reply(w, 200, object("eligible", policy.Eligible(p, c.Config.Monotonic()) && !c.benchmarkFenced(id), "serve", serve, "active_requests", active))
}

// Called with mu held and returns with it held. wake is captured under the same
// lock as scheduling so broadcasts cannot be lost between check and wait.
func (c *WorkerCoordinator) lease(w http.ResponseWriter, r *http.Request, id string) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	reg := c.workers[id]
	for {
		if !c.Machines.Available() {
			reject(w, 503, "machine registry unavailable")
			return
		}
		if !c.authWorker(r, id) {
			reject(w, 401, "invalid worker credentials")
			return
		}
		s := c.workers[id]
		if s == nil || s != reg {
			reply(w, 200, object("job", nil))
			return
		}
		p := s.view.Policy
		p.Jobs = len(s.jobs)
		serve := c.Energy.Serves(p) && !c.benchmarkFenced(id)
		if serve && len(s.jobs) < s.capacity {
			lanes := []*[]string{&c.queue}
			bulk := 0
			for rid := range s.jobs {
				if j := c.jobs[rid]; j != nil && j.Bulk {
					bulk++
				}
			}
			if c.pending(false) == 0 && bulk < min(c.Config.BulkSlotsPerWorker, s.capacity-1) {
				lanes = append(lanes, &c.bulk)
			}
			for _, q := range lanes {
				for len(*q) > 0 {
					rid := (*q)[0]
					*q = (*q)[1:]
					j := c.jobs[rid]
					if j == nil || j.status != "queued" {
						continue
					}
					c.setStatus(j, "leased")
					j.worker = id
					j.lease = leaseID()
					s.jobs[rid] = true
					reply(w, 200, object("request_id", rid, "lease_id", j.lease, "body", j.Body))
					return
				}
			}
		}
		wake := c.wake
		c.mu.Unlock()
		cancelled := false
		expired := false
		select {
		case <-r.Context().Done():
			cancelled = true
		case <-timer.C:
			expired = true
		case <-wake:
		case <-c.Machines.Failed():
		}
		c.mu.Lock()
		if cancelled {
			return
		}
		if expired && c.Machines.Available() {
			reply(w, 200, object("job", nil))
			return
		}
	}
}
func (c *WorkerCoordinator) result(w http.ResponseWriter, b map[string]any, j *workerJob) {
	raw, thought := "", ""
	valid := true
	if v, ok := b["chunk"]; ok {
		raw, ok = v.(string)
		valid = valid && ok
	}
	if v, ok := b["reasoning"]; ok {
		thought, ok = v.(string)
		valid = valid && ok
	}
	if !valid || utf8.RuneCountInString(raw) > 16384 || utf8.RuneCountInString(thought) > 16384 {
		reject(w, 400, "invalid chunk")
		return
	}
	final := false
	if v, ok := b["final"]; ok {
		final = truth(v)
	}
	var completion *int64
	if final {
		usage := map[string]any{}
		if v, ok := b["usage"]; ok {
			var valid bool
			usage, valid = v.(map[string]any)
			if !valid {
				reject(w, 400, "invalid usage")
				return
			}
		}
		for _, k := range []string{"prompt_tokens", "completion_tokens"} {
			if v, ok := usage[k]; ok {
				n, valid := integer(v)
				if !valid || n < 0 {
					reject(w, 400, "invalid usage")
					return
				}
				if k == "completion_tokens" {
					completion = &n
				}
			}
		}
	}
	mt := tokenLimit(j.Body["max_tokens"])
	total := j.raw + int64(utf8.RuneCountInString(raw)+utf8.RuneCountInString(thought))
	if total > mt*16 {
		c.failJob(j, "output_limit")
		reply(w, 200, object("accepted", false))
		return
	}
	// Keep one terminal slot free. Never block on a slow/cancelled public stream
	// while holding mu. Overflow cancels only that job, preserving accepted order.
	if j.Stream && len(j.events) >= cap(j.events)-1 {
		c.failJob(j, "output_limit")
		reply(w, 200, object("accepted", false))
		return
	}
	if final && j.Finish != nil {
		if err := j.Finish(total, completion, false); err != nil {
			c.failJob(j, "billing_error")
			reply(w, 200, object("accepted", false))
			return
		}
	}
	j.raw = total
	if j.Stream {
		j.events <- Event{Text: raw, Reasoning: thought, Final: final, CompletionTokens: completion, WorkerID: j.worker}
	} else {
		j.text.WriteString(raw)
		j.reasoning.WriteString(thought)
		if final {
			j.events <- Event{Text: j.text.String(), Reasoning: j.reasoning.String(), Final: true, CompletionTokens: completion, WorkerID: j.worker}
		}
	}
	if final {
		c.closeJob(j, false)
	}
	reply(w, 200, object("accepted", true))
}
func tokenLimit(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}
func truth(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		n, _ := x.Float64()
		return n != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}
