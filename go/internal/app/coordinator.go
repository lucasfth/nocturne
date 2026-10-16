package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/benchmark"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"github.com/lucasfth/nocturne/go/internal/machines"
	"github.com/lucasfth/nocturne/go/internal/policy"
	"log"
	"strings"
	"sync"
	"time"
)

// WorkerCoordinator owns worker sessions and scheduling. Billing/formatting
// remain in App. Lock order: coordinator -> machines/energy, never reversed.
type WorkerCoordinator struct {
	// Benchmarks is initialized before serving; lock order coordinator -> store.
	Benchmarks                    *benchmark.Store
	BenchmarkTargets              map[string]BenchmarkTarget
	mu                            sync.Mutex
	Config                        WorkerConfig
	Machines                      *machines.Machines
	Energy                        *energy.Energy
	workers                       map[string]*workerSession
	jobs                          map[string]*workerJob
	queue, bulk                   []string
	queuedInteractive, queuedBulk int
	wake                          chan struct{}
}
type WorkerConfig struct {
	Model              string
	RequestTimeout     time.Duration
	BulkSlotsPerWorker int
	// Monotonic must use energy.Monotonic's domain. Set before concurrent use.
	Monotonic func() float64
	Logf      func(string, ...any)
}
type workerSession struct {
	view      energy.Worker
	capacity  int
	jobs      map[string]bool
	heartbeat map[string]any
}
type workerJob struct {
	Job
	events                chan Event
	started               float64
	status, worker, lease string
	retries               int
	raw                   int64
	// Builders belong to this pointer-owned job; never copy a workerJob.
	text, reasoning strings.Builder
	stop            func() bool
}

var _ Coordinator = (*WorkerCoordinator)(nil)

func NewWorkerCoordinator(c WorkerConfig, m *machines.Machines, e *energy.Energy) *WorkerCoordinator {
	if c.Monotonic == nil {
		c.Monotonic = energy.Monotonic
	}
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	if c.BulkSlotsPerWorker == 0 {
		c.BulkSlotsPerWorker = 1
	}
	return &WorkerCoordinator{Config: c, Machines: m, Energy: e, workers: map[string]*workerSession{}, jobs: map[string]*workerJob{}, wake: make(chan struct{})}
}
func (c *WorkerCoordinator) signal() { close(c.wake); c.wake = make(chan struct{}) }
func (c *WorkerCoordinator) workersLocked() map[string]energy.Worker {
	out := map[string]energy.Worker{}
	for id, w := range c.workers {
		v := w.view
		v.Policy.Jobs = len(w.jobs)
		if c.benchmarkFenced(id) {
			v.Policy.Ready = false
		}
		if v.Policy.UserActive != nil {
			b := *v.Policy.UserActive
			v.Policy.UserActive = &b
		}
		if v.TokensPerSec != nil {
			n := *v.TokensPerSec
			v.TokensPerSec = &n
		}
		out[id] = v
	}
	return out
}
func (c *WorkerCoordinator) Workers() map[string]energy.Worker {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workersLocked()
}
func (c *WorkerCoordinator) pending(bulk bool) int {
	if bulk {
		return c.queuedBulk
	}
	return c.queuedInteractive
}
func (c *WorkerCoordinator) Pending(bulk bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending(bulk)
}

// setStatus updates queue counts with every transition. Caller holds mu.
func (c *WorkerCoordinator) setStatus(j *workerJob, status string) {
	count := &c.queuedInteractive
	if j.Bulk {
		count = &c.queuedBulk
	}
	if j.status == "queued" {
		*count--
	}
	j.status = status
	if status == "queued" {
		*count++
	}
}
func (c *WorkerCoordinator) enqueue(j *workerJob) {
	if j.Bulk {
		c.bulk = append(c.bulk, j.ID)
	} else {
		c.queue = append(c.queue, j.ID)
	}
	c.signal()
}
func (c *WorkerCoordinator) Submit(ctx context.Context, j Job) (<-chan Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.jobs[j.ID]; ok {
		return nil, fmt.Errorf("duplicate request_id")
	}
	body := make(map[string]any, len(j.Body))
	for k, v := range j.Body {
		body[k] = v
	}
	j.Body = body
	w := &workerJob{Job: j, events: make(chan Event, 65), started: c.Config.Monotonic()}
	c.jobs[j.ID] = w
	c.setStatus(w, "queued")
	w.stop = context.AfterFunc(ctx, func() { c.Cancel(j.ID) })
	c.enqueue(w)
	return w.events, nil
}
func (c *WorkerCoordinator) closed(j *workerJob) bool {
	return j.status == "done" || j.status == "cancelled"
}
func (c *WorkerCoordinator) closeJob(j *workerJob, cancel bool) {
	if c.closed(j) {
		return
	}
	if cancel && j.Finish != nil {
		if err := j.Finish(j.raw, nil, true); err != nil {
			c.Config.Logf("request_id=%s event=billing_finish_failed error=%v", j.ID, err)
		}
	}
	c.setStatus(j, "done")
	if cancel {
		c.setStatus(j, "cancelled")
	}
	if w := c.workers[j.worker]; w != nil {
		delete(w.jobs, j.ID)
	}
	if j.stop != nil {
		j.stop()
	}
	close(j.events)
	c.signal()
}
func (c *WorkerCoordinator) failJob(j *workerJob, reason string) {
	if c.closed(j) {
		return
	}
	j.events <- Event{Error: reason, WorkerID: j.worker}
	c.closeJob(j, true)
}
func (c *WorkerCoordinator) Cancel(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.jobs[id]; j != nil {
		c.closeJob(j, true)
	}
}
func (c *WorkerCoordinator) interrupt(id string) {
	w := c.workers[id]
	if w == nil {
		return
	}
	for rid := range w.jobs {
		j := c.jobs[rid]
		delete(w.jobs, rid)
		if j == nil || c.closed(j) {
			continue
		}
		j.lease = ""
		if !j.Stream && j.retries == 0 {
			j.retries++
			j.worker = ""
			c.setStatus(j, "queued")
			j.raw = 0
			j.text.Reset()
			j.reasoning.Reset()
			c.enqueue(j)
			c.Config.Logf("request_id=%s worker_id=%s event=cancel_requeue", rid, id)
		} else {
			c.failJob(j, "worker_interrupted")
		}
	}
	c.signal()
}

// DropWorker follows registry rotation/disable/delete and invalidates poll sessions.
func (c *WorkerCoordinator) DropWorker(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropWorkerLocked(id)
}

// Maintain is a deterministic sweep; startup owns energy/fx refresh and repricing.
func (c *WorkerCoordinator) Maintain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.Config.Monotonic()
	opened, _, end := c.Energy.Window()
	grace := 20.0
	if s := c.Energy.Config.Schedule; s != nil && s.GraceMin != nil {
		grace = *s.GraceMin
	}
	for id, w := range c.workers {
		if now-w.view.Policy.Seen >= 45 || policy.Unsafe(w.view.Policy) || (!opened && !c.Energy.Clock().Before(end.Add(time.Duration(grace*float64(time.Minute))))) {
			c.interrupt(id)
		}
	}
	for id, j := range c.jobs {
		age := now - j.started
		timeout := c.Config.RequestTimeout.Seconds()
		if timeout > 0 && !c.closed(j) && age > timeout {
			c.failJob(j, "request_timeout")
		}
		if timeout > 0 && c.closed(j) && age > timeout+60 {
			delete(c.jobs, id)
		}
	}
	compact := func(q []string) []string {
		out := q[:0]
		for _, id := range q {
			if j := c.jobs[id]; j != nil && j.status == "queued" {
				out = append(out, id)
			}
		}
		return out
	}
	c.queue = compact(c.queue)
	c.bulk = compact(c.bulk)
	c.signal()
}
func (c *WorkerCoordinator) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Maintain()
		}
	}
}
func leaseID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return fmt.Sprintf("%x", b)
}
