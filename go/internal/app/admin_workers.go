package app

import (
	"errors"
	"github.com/lucasfth/nocturne/go/internal/machines"
	"github.com/lucasfth/nocturne/go/internal/policy"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
	"sort"
)

// Mutations share worker auth's coordinator -> registry lock order.
func (c *WorkerCoordinator) UpdateMachine(id string, b map[string]any) (machines.Public, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.Machines.Update(id, b)
	if errors.Is(err, machines.ErrUnavailable) {
		c.signal()
	}
	if err == nil {
		_, rotate := b["secret_sha256"]
		if !m.Enabled || rotate {
			c.dropWorkerLocked(id)
		}
	}
	return m, err
}
func (c *WorkerCoordinator) DeleteMachine(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.Machines.Delete(id)
	if errors.Is(err, machines.ErrUnavailable) {
		c.signal()
	}
	if err == nil {
		c.dropWorkerLocked(id)
	}
	return err
}
func (c *WorkerCoordinator) dropWorkerLocked(id string) {
	c.interrupt(id)
	delete(c.workers, id)
	c.signal()
}
func cloneTelemetry(v any) any {
	if m, ok := v.(map[string]any); ok {
		out := map[string]any{}
		for k, x := range m {
			out[k] = x
		}
		return out
	}
	return v
}

// Snapshots are detached so callers write responses outside coordinator locks.
func (c *WorkerCoordinator) AdminWorkers() []pyjson.Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []pyjson.Object{}
	now := c.Config.Monotonic()
	ids := []string{}
	for id := range c.workers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := c.workers[id]
		p := s.view.Policy
		p.Jobs = len(s.jobs)
		o := object("worker_id", id, "eligible", policy.Eligible(p, now), "heartbeat_age", now-p.Seen, "ready", p.Ready)
		for k, v := range s.heartbeat {
			if k != "worker_id" {
				o.Set(k, cloneTelemetry(v))
			}
		}
		if s.view.TokensPerSec != nil {
			o.Set("tokens_per_sec", *s.view.TokensPerSec)
		}
		jobs := []string{}
		for rid := range s.jobs {
			jobs = append(jobs, rid)
		}
		sort.Strings(jobs)
		o.Set("active_requests", jobs)
		out = append(out, o)
	}
	return out
}
func (c *WorkerCoordinator) MachineRows() ([]pyjson.Object, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta := map[string]machines.Public{}
	ids := map[string]bool{}
	for _, m := range c.Machines.List() {
		meta[m.WorkerID] = m
		ids[m.WorkerID] = true
	}
	for id := range c.workers {
		ids[id] = true
	}
	order := []string{}
	for id := range ids {
		order = append(order, id)
	}
	sort.Strings(order)
	out := []pyjson.Object{}
	active := 0
	now := c.Config.Monotonic()
	for _, id := range order {
		m := meta[id]
		s, online := c.workers[id]
		mac := m.Meta
		if mac == nil {
			mac = map[string]string{}
		}
		o := object("worker_id", id, "online", online, "enabled", m.Enabled, "mac", mac, "eligible", false, "heartbeat_age_s", nil, "battery_pct", nil, "on_ac", nil, "thermal_state", nil, "user_active", nil, "tokens_per_sec", nil, "busy", 0, "capacity", nil, "metrics", map[string]any{})
		if online {
			p := s.view.Policy
			p.Jobs = len(s.jobs)
			active += p.Jobs
			o.Set("eligible", policy.Eligible(p, now))
			o.Set("heartbeat_age_s", pyjson.RoundHalfEven(now-p.Seen, 1))
			for _, k := range []string{"battery_pct", "on_ac", "thermal_state", "user_active"} {
				o.Set(k, s.heartbeat[k])
			}
			if s.view.TokensPerSec != nil {
				o.Set("tokens_per_sec", *s.view.TokensPerSec)
			}
			o.Set("busy", p.Jobs)
			o.Set("capacity", s.capacity)
			if v, ok := s.heartbeat["metrics"]; ok {
				o.Set("metrics", cloneTelemetry(v))
			}
		}
		out = append(out, o)
	}
	return out, active, c.pending(false)
}
