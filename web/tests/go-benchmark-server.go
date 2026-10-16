// SPDX-License-Identifier: AGPL-3.0-or-later
// Disposable browser fixture. ALL benchmark evidence is synthetic test data:
// no inference, model loading, service changes, or production adapter execution.
// Internal imports require copying this source beneath go before building:
//
//	d=$(mktemp -d go/browser-fixture.XXXXXX)
//	cp web/tests/go-benchmark-server.go "$d/main.go"
//	(cd go && go build -o ../"$d/server" ./"${d#go/}")
//	"$d/server" -web-dir "$PWD/web/dist"
//
// Remove $d after stopping the server. Runtime data is separately temporary.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lucasfth/nocturne/go/internal/app"
	"github.com/lucasfth/nocturne/go/internal/benchmark"
	"github.com/lucasfth/nocturne/go/internal/billing"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"github.com/lucasfth/nocturne/go/internal/fx"
	"github.com/lucasfth/nocturne/go/internal/lightning"
	"github.com/lucasfth/nocturne/go/internal/machines"
)

func must(err error) {
	if err != nil {
		panic("disposable benchmark fixture failed (details redacted)")
	}
}
func digest(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
func evidence(o benchmark.Options) *benchmark.Summary {
	return &benchmark.Summary{Quality: benchmark.QualityPass, Context: o.Context, Parallel: o.Parallel,
		PromptTokens: o.Context - 100, OutputTokens: 100, DurationSeconds: 2, TokensPerSecond: 50,
		ModelVerified: true, ModelSHA256: o.ModelSHA256, BuildDigest: o.BuildDigest}
}

// Dispatch through actual authenticated worker routes without exposing a worker listener.
func worker(h http.Handler, machine, route string, body map[string]any, out any) {
	body["worker_id"] = machine
	raw, err := json.Marshal(body)
	must(err)
	r := httptest.NewRequest("POST", "/worker/"+route, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer synthetic-"+machine)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		panic(fmt.Sprintf("synthetic worker route %s: status %d", route, w.Code))
	}
	if out != nil {
		must(json.Unmarshal(w.Body.Bytes(), out))
	}
}

func run() error {
	web := flag.String("web-dir", "web/dist", "built production SPA directory (read only)")
	flag.Parse()
	webDir, err := filepath.Abs(*web)
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(webDir, "index.html")); err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "nocturne-synthetic-browser-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	must(os.WriteFile(filepath.Join(root, "prices.json"), []byte(`{"price_per_token_msat":1}`), 0600))
	b, err := billing.Open(root)
	if err != nil {
		return err
	}
	defer b.Close()
	l, err := lightning.New(lightning.Config{Backend: "fake"}, b)
	if err != nil {
		return err
	}
	e := energy.New(&energy.Config{}, nil, nil)
	m, err := machines.Open(root, machines.Legacy{})
	if err != nil {
		return err
	}
	c := app.NewWorkerCoordinator(app.WorkerConfig{Model: "synthetic-test-no-inference", RequestTimeout: 2 * time.Second}, m, e)
	store, err := benchmark.Open(root)
	if err != nil {
		return err
	}
	defer store.Close()
	c.Benchmarks = store
	target := app.BenchmarkTarget{ModelSHA256: digest("synthetic-model"), BuildDigest: digest("synthetic-build"), CodeRevision: digest("synthetic-revision"), ConfigGeneration: digest("synthetic-config"), ApplyEnabled: true}
	c.BenchmarkTargets = map[string]app.BenchmarkTarget{}
	ids := []string{"fixture-success", "fixture-cancel", "fixture-recovery", "production-unbound"}
	h := c.Worker()
	for _, id := range ids {
		_, err = m.Add(map[string]any{"worker_id": id, "secret_sha256": digest("synthetic-" + id), "meta": map[string]any{"owner": "SYNTHETIC browser test — no inference"}})
		must(err)
		if id != "production-unbound" {
			c.BenchmarkTargets[id] = target
		}
		worker(h, id, "register", map[string]any{}, nil)
	}
	// Historical synthetic evidence with NO current adapter binding. Both new
	// starts and Apply must be refused by the real production admission checks.
	j, err := store.StartWithProvenance("production-unbound", benchmark.Options{ModelSHA256: target.ModelSHA256, BuildDigest: target.BuildDigest}, benchmark.Audit{Actor: "synthetic-test", CodeRevision: target.CodeRevision, ConfigGeneration: target.ConfigGeneration})
	must(err)
	lease, err := store.Lease(j.Machine)
	must(err)
	_, err = store.Update(j.Machine, j.ID, lease.Token, benchmark.Update{Sequence: 1, State: benchmark.Completed, Progress: benchmark.Done, Restored: true, Summary: evidence(j.Options)})
	must(err)
	a := app.New(app.Config{Model: "synthetic-test-no-inference", MaxTokens: 100, MaxInputChars: 1000, MaxContext: 1000, QueueDepth: 2}, b, e, c, l, func() float64 { return 500000 })
	cfg := app.AdminConfig{SecretSHA256: digest("browser-test"), Coordinator: c, Dist: os.DirFS(webDir), Fx: fx.New(500000, filepath.Join(root, "fx"), func(context.Context) (float64, error) { return 500000, nil }, nil)}
	listener, err := net.Listen("tcp", "127.0.0.1:5201")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: a.Admin(cfg), ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	tokens := map[string]string{}
	fmt.Println("Synthetic benchmark UI fixture ready at http://127.0.0.1:5201/admin (no inference)")
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-served:
			return err
		case <-ticker.C:
			for _, id := range ids[:3] {
				worker(h, id, "heartbeat", map[string]any{"model": "synthetic-test-no-inference", "ready": true, "on_ac": true, "battery_pct": 90, "thermal_state": "nominal", "user_active": false, "in_flight": 0, "capacity": 2}, nil)
				jobs, err := store.List(id)
				must(err)
				for _, job := range jobs {
					if job.State.Terminal() || job.State == benchmark.RecoveryRequired {
						continue
					}
					if job.State == benchmark.Queued {
						var assignment benchmark.Assignment
						worker(h, id, "benchmark/lease", map[string]any{"apply_protocol": 1}, &assignment)
						tokens[job.ID] = assignment.Token
						job = assignment.Job
					}
					u := benchmark.Update{Action: job.Action, Sequence: job.Sequence + 1, State: benchmark.Completed, Progress: benchmark.Done, Restored: true}
					switch {
					case job.Action == benchmark.ActionApply:
						// Synthetic protocol evidence only: production Apply stays disabled.
						reconcile := benchmark.Reconciliation{TransactionID: job.ApplyRequest.TransactionID, Generation: job.ApplyRequest.Generation, Phase: benchmark.ApplyPrepared, Durable: true}
						worker(h, id, "benchmark/reconcile", map[string]any{"job_id": job.ID, "lease": tokens[job.ID], "reconcile": reconcile}, nil)
						reconcile.Phase = benchmark.ApplyApplied
						worker(h, id, "benchmark/reconcile", map[string]any{"job_id": job.ID, "lease": tokens[job.ID], "reconcile": reconcile}, nil)
						continue
					case job.State == benchmark.Cancelling:
						u.State = benchmark.Cancelled
					case id == "fixture-cancel":
						continue // Stay leased until explicit UI cancellation.
					case id == "fixture-recovery":
						u.State = benchmark.RecoveryRequired
						u.Progress = benchmark.Restoring
						u.Restored = false
						u.Failure = benchmark.FailureRestoration
					default:
						u.Summary = evidence(job.Options)
					}
					worker(h, id, "benchmark/update", map[string]any{"job_id": job.ID, "lease": tokens[job.ID], "update": u}, nil)
				}
			}
		}
	}
}
func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, "disposable benchmark fixture failed (details redacted)")
		os.Exit(1)
	}
}
