package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lucasfth/nocturne/go/internal/benchmark"
)

func TestBenchmarkApplyHTTP(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "rollback"}[rollback], func(t *testing.T) {
			a, c, cfg := benchmarkFixture(t)
			server := httptest.NewServer(c.Worker())
			defer server.Close()
			call := func(route, body, credential string) *httptest.ResponseRecorder {
				t.Helper()
				req, _ := http.NewRequest("POST", server.URL+"/worker/"+route, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+credential)
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				out := httptest.NewRecorder()
				out.WriteHeader(res.StatusCode)
				if _, err := io.Copy(out, res.Body); err != nil {
					t.Fatal(err)
				}
				return out
			}
			workerCall := func(route, body string) *httptest.ResponseRecorder { return call(route, body, "worker") }
			target := c.BenchmarkTargets["w"]
			target.CodeRevision = strings.Repeat("c", 40)
			target.ConfigGeneration = strings.Repeat("d", 64)
			c.BenchmarkTargets["w"] = target
			base := "/admin/machines/w/benchmarks"
			w := ar(a, cfg, "POST", base, `{}`, "admin", "")
			var j benchmark.Job
			if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &j) != nil {
				t.Fatal(w.Body)
			}
			path := base + "/" + j.ID + "/apply"
			if w = ar(a, cfg, "POST", path, `{}`, "admin", ""); w.Code != 409 {
				t.Fatal(w.Body)
			}
			leased := workerCall("benchmark/lease", `{"worker_id":"w"}`)
			var assignment benchmark.Assignment
			if leased.Code != 200 || json.Unmarshal(leased.Body.Bytes(), &assignment) != nil {
				t.Fatal(leased.Body)
			}
			send := func(u benchmark.Update, token string) int {
				raw, _ := json.Marshal(map[string]any{"worker_id": "w", "job_id": j.ID, "lease": token, "update": u})
				return workerCall("benchmark/update", string(raw)).Code
			}
			summary := &benchmark.Summary{Quality: benchmark.QualityPass, Context: j.Options.Context, Parallel: j.Options.Parallel, PromptTokens: j.Options.Context - 100, OutputTokens: 100, DurationSeconds: 2, TokensPerSecond: 50, ModelVerified: true, ModelSHA256: target.ModelSHA256, BuildDigest: target.BuildDigest}
			if code := send(benchmark.Update{Sequence: 1, State: benchmark.Completed, Progress: benchmark.Done, Restored: true, Summary: summary}, assignment.Token); code != 200 {
				t.Fatal(code)
			}
			if fenced, _ := c.Benchmarks.Fenced("w"); fenced {
				t.Fatal("automatic apply")
			}
			if w = ar(a, cfg, "POST", path, `{}`, "admin", ""); w.Code != 409 {
				t.Fatal("missing opt-in", w.Body)
			}
			target.ApplyEnabled = true
			c.BenchmarkTargets["w"] = target
			if w = ar(a, cfg, "POST", path, `{}`, "wrong", ""); w.Code != 401 {
				t.Fatal(w.Code)
			}
			for _, body := range []string{`{"context":8192}`, `{"config_generation":"x"}`, `{"path":"x"}`, `null`, `{} {}`} {
				if w = ar(a, cfg, "POST", path, body, "admin", ""); w.Code != 400 {
					t.Fatal(body, w.Code)
				}
			}
			target.ConfigGeneration = strings.Repeat("e", 64)
			c.BenchmarkTargets["w"] = target
			if w = ar(a, cfg, "POST", path, `{}`, "admin", ""); w.Code != 409 {
				t.Fatal("stale accepted")
			}
			target.ConfigGeneration = strings.Repeat("d", 64)
			c.BenchmarkTargets["w"] = target
			if w = ar(a, cfg, "POST", path, `{}`, "admin", ""); w.Code != 202 {
				t.Fatal(w.Code, w.Body)
			}
			if w = ar(a, cfg, "POST", path, `{}`, "admin", ""); w.Code != 409 {
				t.Fatal("apply replay")
			}
			old := assignment.Token
			if oldWorker := workerCall("benchmark/lease", `{"worker_id":"w"}`); oldWorker.Code != 409 {
				t.Fatal("old worker dispatched")
			}
			leased = workerCall("benchmark/lease", `{"worker_id":"w","apply_protocol":1}`)
			if leased.Code != 200 || json.Unmarshal(leased.Body.Bytes(), &assignment) != nil || assignment.Action != benchmark.ActionApply {
				t.Fatal(leased.Body)
			}
			u := benchmark.Update{Action: benchmark.ActionApply, Sequence: 1, State: benchmark.Completed, Progress: benchmark.Done, Result: benchmark.Applied}
			if rollback {
				if code := send(benchmark.Update{Action: benchmark.ActionApply, Sequence: 1, State: benchmark.RecoveryRequired, Progress: benchmark.Restoring, Failure: benchmark.FailureRestoration}, assignment.Token); code != 200 {
					t.Fatal(code)
				}
				if fenced, _ := c.Benchmarks.Fenced("w"); !fenced {
					t.Fatal("failed rollback released fence")
				}
				u.Sequence = 2
				u.State = benchmark.Failed
				u.Failure = benchmark.FailureExecution
				u.Restored = true
				u.Result = benchmark.RolledBack
			}
			if code := send(u, old); code != 409 {
				t.Fatal("old lease accepted", code)
			}
			if code := send(u, assignment.Token); code != 400 {
				t.Fatal("legacy terminal accepted", code)
			}
			reconcile := benchmark.Reconciliation{TransactionID: assignment.Job.ApplyRequest.TransactionID, Generation: 1, Phase: benchmark.ApplyPrepared, Durable: true}
			report := func(credential string, want int) benchmark.Job {
				t.Helper()
				raw, _ := json.Marshal(map[string]any{"worker_id": "w", "job_id": j.ID, "lease": assignment.Token, "reconcile": reconcile})
				res := call("benchmark/reconcile", string(raw), credential)
				if res.Code != want {
					t.Fatalf("reconcile: %d %s", res.Code, res.Body)
				}
				var got benchmark.Job
				if want == 200 && json.Unmarshal(res.Body.Bytes(), &got) != nil {
					t.Fatal("invalid job")
				}
				return got
			}
			if rollback {
				reconcile.Phase = benchmark.ApplyRolledBack
			} else {
				reconcile.Phase = benchmark.ApplyApplied
				report("worker", 409)
				reconcile.Phase = benchmark.ApplyPrepared
				report("wrong", 401)
				reconcile.Durable = false
				report("worker", 400)
				reconcile.Durable = true
				reconcile.Generation++
				report("worker", 409)
				reconcile.Generation--
				report("worker", 200)
				report("worker", 200)
				if fenced, _ := c.Benchmarks.Fenced("w"); !fenced {
					t.Fatal("prepared unfenced")
				}
				status := workerCall("benchmark/status", `{"worker_id":"w","job_id":"`+j.ID+`"}`)
				var got benchmark.Job
				if status.Code != 200 || json.Unmarshal(status.Body.Bytes(), &got) != nil || got.ApplyPhase != benchmark.ApplyPrepared || strings.Contains(status.Body.String(), assignment.Token) {
					t.Fatal("bad status")
				}
				reconcile.Phase = benchmark.ApplyApplied
			}
			report("worker", 200)
			report("wrong", 401) // even terminal replay reauthenticates
			report("worker", 200)
			c.UpdateMachine("w", map[string]any{"secret_sha256": digest("rotated")})
			report("worker", 401)
			report("rotated", 200)
			if rollback {
				reconcile.Phase = benchmark.ApplyApplied
			} else {
				reconcile.Phase = benchmark.ApplyRolledBack
			}
			report("rotated", 409)
			if fenced, _ := c.Benchmarks.Fenced("w"); fenced {
				t.Fatal("terminal still fenced")
			}
		})
	}
}
