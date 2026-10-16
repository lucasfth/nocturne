package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/lucasfth/nocturne/go/internal/benchmark"
	"github.com/lucasfth/nocturne/go/internal/machines"
	"io"
	"net/http"
	"strings"
	"time"
)

// BenchmarkTarget is a trusted startup adapter binding, never browser supplied.
// No production adapters are enabled until local restoration/watchdog support
// exists. Digests bind an independently verified local model and executor build.
type BenchmarkTarget struct {
	// Explicit opt-in only for adapters with verified apply AND rollback support.
	ApplyEnabled             bool
	ModelSHA256, BuildDigest string
	// Optional for legacy test adapters; absent means unknown, not current.
	// Revision is lowercase Git hex (40/64); generation is a nonsecret SHA-256
	// identity of the worker configuration generation, not configuration JSON.
	CodeRevision, ConfigGeneration string
}

func benchmarkRoute(p string) (methods, machine, job string) {
	parts := strings.Split(strings.TrimPrefix(p, "/admin/machines/"), "/")
	if !strings.HasPrefix(p, "/admin/machines/") || len(parts) < 2 || !machines.ValidID(parts[0]) || parts[1] != "benchmarks" {
		return
	}
	machine = parts[0]
	if len(parts) == 2 {
		return "GET, POST", machine, ""
	}
	if len(parts) == 3 && parts[2] != "" {
		return "GET", machine, parts[2]
	}
	if len(parts) == 4 && parts[2] != "" && (parts[3] == "cancel" || parts[3] == "apply") {
		return "POST", machine, parts[2]
	}
	return "", "", ""
}

// benchmarkFenced requires coordinator.mu. Any store failure closes admission.
func (c *WorkerCoordinator) benchmarkFenced(id string) bool {
	if c.Benchmarks == nil {
		return false
	}
	fenced, err := c.Benchmarks.Fenced(id)
	return err != nil || fenced
}

// strictBenchmarkJSON rejects duplicate/case-aliased/unknown keys at every level,
// trailing data, non-objects and bodies over 16 KiB before typed decoding.
func strictBenchmarkJSON(r *http.Request, dst any) error {
	raw, e := io.ReadAll(io.LimitReader(r.Body, 16385))
	if e != nil || len(raw) > 16384 {
		return errors.New("invalid body")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 5 {
			return errors.New("depth")
		}
		tok, e := d.Token()
		if e != nil {
			return e
		}
		if delim, ok := tok.(json.Delim); ok {
			if delim != '{' {
				return errors.New("object required")
			}
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				key, ok := k.(string)
				if !ok || seen[key] || key != strings.ToLower(key) || strings.IndexFunc(key, func(r rune) bool { return r > 127 }) >= 0 {
					return errors.New("key")
				}
				seen[key] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		}
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("object required")
	}
	if e = walk(0); e != nil {
		return e
	}
	if _, e = d.Token(); e != io.EOF {
		return errors.New("trailing")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func benchmarkError(w http.ResponseWriter, err error) {
	status := 409
	switch {
	case errors.Is(err, benchmark.ErrNotFound):
		status = 404
	case errors.Is(err, benchmark.ErrInvalidOptions), errors.Is(err, benchmark.ErrInvalidUpdate):
		status = 400
	case errors.Is(err, benchmark.ErrCapacity):
		status = 429
	case errors.Is(err, benchmark.ErrPoisoned), errors.Is(err, benchmark.ErrClosed), errors.Is(err, benchmark.ErrStorage):
		status = 503
	}
	reject(w, status, "benchmark operation refused")
}
func (c *WorkerCoordinator) serveBenchmarkAdmin(w http.ResponseWriter, r *http.Request) {
	// Authenticate happens in serveAdmin. Never read/write network under c.mu.
	if !settingsBrowserGuard(r) {
		reject(w, 403, "invalid origin")
		return
	}
	if r.Method == "POST" {
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
		raw, e := io.ReadAll(io.LimitReader(r.Body, 16385))
		_ = controller.SetReadDeadline(time.Time{})
		if e != nil || len(raw) > 16384 {
			reject(w, 400, "invalid benchmark body")
			return
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}

	response := &workerResponse{header: make(http.Header), status: 200}
	defer func() {
		for k, v := range response.header {
			w.Header()[k] = v
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
	}()
	c.serveBenchmarkAdminBuffered(response, r)
}
func (c *WorkerCoordinator) serveBenchmarkAdminBuffered(w http.ResponseWriter, r *http.Request) {
	_, machine, id := benchmarkRoute(r.URL.Path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Benchmarks == nil {
		reject(w, 503, "benchmark unavailable")
		return
	}
	if !c.Machines.Enabled(machine) {
		reject(w, 404, "machine unavailable")
		return
	}
	if r.Method == "GET" {
		if id == "" {
			jobs, e := c.Benchmarks.List(machine)
			if e != nil {
				benchmarkError(w, e)
				return
			}
			adminReply(w, 200, jobs)
		} else {
			j, e := c.Benchmarks.Get(machine, id)
			if e != nil {
				benchmarkError(w, e)
				return
			}
			adminReply(w, 200, j)
		}
		return
	}
	if id != "" {
		var empty struct{}
		if strictBenchmarkJSON(r, &empty) != nil {
			reject(w, 400, "invalid benchmark body")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/apply") {
			if _, e := c.Benchmarks.Get(machine, id); e != nil {
				benchmarkError(w, e)
				return
			}
			target := c.BenchmarkTargets[machine]
			j, e := c.Benchmarks.RequestApply(machine, id, benchmark.ApplyTarget{
				ApplyEnabled: target.ApplyEnabled, ModelSHA256: target.ModelSHA256, BuildDigest: target.BuildDigest,
				Audit: benchmark.Audit{Actor: "admin-bearer", CodeRevision: target.CodeRevision, ConfigGeneration: target.ConfigGeneration},
			})
			if e != nil {
				benchmarkError(w, e)
				return
			}
			c.signal()
			adminReply(w, 202, j)
			return
		}
		j, e := c.Benchmarks.Cancel(machine, id)
		if e != nil {
			benchmarkError(w, e)
			return
		}
		c.signal()
		adminReply(w, 200, j)
		return
	}
	var input struct {
		Advanced struct {
			Context       int `json:"context"`
			Parallel      int `json:"parallel"`
			BudgetSeconds int `json:"budget_seconds"`
		} `json:"advanced"`
	}
	if strictBenchmarkJSON(r, &input) != nil {
		reject(w, 400, "invalid benchmark body")
		return
	}
	target, ok := c.BenchmarkTargets[machine]
	if !ok {
		reject(w, 409, "unsupported worker: secure benchmark adapter unavailable")
		return
	}
	o := benchmark.Options{Context: input.Advanced.Context, Parallel: input.Advanced.Parallel, BudgetSeconds: input.Advanced.BudgetSeconds, ModelSHA256: target.ModelSHA256, BuildDigest: target.BuildDigest}
	// Existing foundation supports no more than 32768 context / 2 slots / 600s.
	if o.Context > 32768 || o.Parallel > 2 || o.BudgetSeconds > 600 {
		reject(w, 400, "unsupported benchmark options")
		return
	}
	// Authorization has already established the fleet-wide admin role. Do not
	// treat client identity headers as an independently authenticated actor.
	j, e := c.Benchmarks.StartWithProvenance(machine, o, benchmark.Audit{
		Actor: "admin-bearer", CodeRevision: target.CodeRevision, ConfigGeneration: target.ConfigGeneration,
	})
	if e != nil {
		benchmarkError(w, e)
		return
	}
	c.signal()
	adminReply(w, 202, j)
}
func (c *WorkerCoordinator) serveBenchmarkWorker(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Path
	if route != "/worker/benchmark/lease" && route != "/worker/benchmark/update" && route != "/worker/benchmark/status" && route != "/worker/benchmark/reconcile" {
		reject(w, 404, "Not Found")
		return
	}
	if r.Method != "POST" {
		reject(w, 405, "Method Not Allowed")
		return
	}
	var b struct {
		WorkerID      string                    `json:"worker_id"`
		JobID         string                    `json:"job_id"`
		Lease         string                    `json:"lease"`
		Update        *benchmark.Update         `json:"update"`
		Reconcile     *benchmark.Reconciliation `json:"reconcile"`
		ApplyProtocol *int                      `json:"apply_protocol"`
	}
	if strictBenchmarkJSON(r, &b) != nil {
		reject(w, 400, "invalid benchmark body")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.authWorker(r, b.WorkerID) {
		reject(w, 401, "invalid worker credentials")
		return
	}
	if c.Benchmarks == nil {
		reject(w, 503, "benchmark unavailable")
		return
	}
	if (route != "/worker/benchmark/lease" && b.ApplyProtocol != nil) || (route != "/worker/benchmark/reconcile" && b.Reconcile != nil) {
		reject(w, 400, "invalid benchmark body")
		return
	}
	if route == "/worker/benchmark/reconcile" {
		if b.Reconcile == nil || b.Update != nil {
			reject(w, 400, "reconciliation required")
			return
		}
		j, e := c.Benchmarks.ReconcileApply(b.WorkerID, b.JobID, b.Lease, *b.Reconcile)
		if e != nil {
			benchmarkError(w, e)
			return
		}
		c.signal()
		adminReply(w, 200, j)
		return
	}
	if route == "/worker/benchmark/status" {
		if b.Lease != "" || b.Update != nil {
			reject(w, 400, "invalid benchmark body")
			return
		}
		j, e := c.Benchmarks.Get(b.WorkerID, b.JobID)
		if e != nil {
			benchmarkError(w, e)
			return
		}
		adminReply(w, 200, j)
		return
	}
	if route == "/worker/benchmark/update" {
		if b.Update == nil {
			reject(w, 400, "update required")
			return
		}
		j, e := c.Benchmarks.Update(b.WorkerID, b.JobID, b.Lease, *b.Update)
		if e != nil {
			benchmarkError(w, e)
			return
		}
		c.signal()
		adminReply(w, 200, j)
		return
	}
	if b.JobID != "" || b.Lease != "" || b.Update != nil {
		reject(w, 400, "invalid benchmark body")
		return
	}
	// Drain under the same lock used by inference leasing. Recovery is allowed
	// without fresh sensors because it can only stop owned work and restore state.
	jobs, e := c.Benchmarks.List(b.WorkerID)
	if e != nil {
		benchmarkError(w, e)
		return
	}
	recovery := false
	for _, j := range jobs {
		if j.Action == benchmark.ActionApply && !j.State.Terminal() && (b.ApplyProtocol == nil || *b.ApplyProtocol != 1) {
			reject(w, 409, "apply reconciliation unsupported")
			return
		}
		if j.ApplyPhase == benchmark.ApplyPrepared {
			recovery = true
		}
		if j.Action == benchmark.ActionApply && j.State == benchmark.Queued {
			target := c.BenchmarkTargets[b.WorkerID]
			if !target.ApplyEnabled || target.ModelSHA256 != j.Options.ModelSHA256 || target.BuildDigest != j.Options.BuildDigest || target.CodeRevision != j.Provenance.CodeRevision || target.ConfigGeneration != j.Provenance.ConfigGeneration {
				reject(w, 409, "apply binding unavailable")
				return
			}
		}
		if j.State == benchmark.Cancelling || j.State == benchmark.RecoveryRequired {
			recovery = true
		}
	}
	if !recovery {
		if _, ok := c.BenchmarkTargets[b.WorkerID]; !ok {
			reject(w, 409, "unsupported worker")
			return
		}
		s := c.workers[b.WorkerID]
		reportedFlight, flightOK := int64(-1), false
		if s != nil {
			reportedFlight, flightOK = integer(s.heartbeat["in_flight"])
		}
		if !flightOK || reportedFlight != 0 || s == nil || len(s.jobs) != 0 || !s.view.Policy.OnAC || s.view.Policy.BatteryPct < 50 || s.view.Policy.ThermalState != "nominal" || s.view.Policy.UserActive == nil || *s.view.Policy.UserActive || c.Config.Monotonic()-s.view.Policy.Seen > 5 {
			reject(w, 409, "unsafe or not drained")
			return
		}
	}
	assignment, e := c.Benchmarks.Lease(b.WorkerID)
	if errors.Is(e, benchmark.ErrNoWork) {
		adminReply(w, 200, map[string]any{"job": nil})
		return
	}
	if e != nil {
		benchmarkError(w, e)
		return
	}
	adminReply(w, 200, assignment)
}
