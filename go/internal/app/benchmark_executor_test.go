// SPDX-License-Identifier: AGPL-3.0-or-later
package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lucasfth/nocturne/go/internal/benchmark"
)

// Keep subprocess diagnostics bounded, including on import or assertion failure.
// Diagnostics are deliberately not printed: HTTP exceptions can contain credentials.
type executorOutput struct {
	n        int
	overflow bool
}

func (b *executorOutput) Write(p []byte) (int, error) {
	b.n += len(p)
	if b.n > 16384 {
		b.overflow = true
	}
	return len(p), nil
}

const executorPython = `
import sys
from pathlib import Path
from worker.benchmark_executor import (FilesystemFixture, FixtureExecutor, WorkerHTTP,
    FixtureBinding, Update, digest, load, group_absent)
mode, root, url, revision = sys.argv[1:]
root = Path(root)
if mode == 'init':
    FilesystemFixture.initialize(root)
    sys.exit(0)
if mode == 'verify':
    a = FilesystemFixture(root)
    s, j = a.state(), load(root / 'journal')
    assert s == dict(config='original', generation=digest(b'original'), service=True,
                     healthy=True, active=0, fenced=False)
    assert j['phase'] == 'restored' and j['restored'] is True
    assert not j.get('launch_pending') and group_absent(j.get('pgid'))
    sys.exit(0)
class WaitingFixture(FixtureExecutor):
    def run(self, **kw):
        return super().run(mode='wait', **kw)
c = WorkerHTTP(url, 'w', 'worker')
try:
    if mode == 'synthetic-prerequisite':
        # SYNTHETIC prerequisite, NOT inference or measured model performance.
        # Only this worker attestation is seeded; Apply below is actually executed.
        assignment = c.lease()
        assert assignment and not assignment.recovery_only and not assignment.action
        binding = FixtureBinding(b'model', b'build', revision, digest(b'original'))
        binding.check(assignment.job, FilesystemFixture(root), apply=False)
        o = assignment.job['options']
        summary = dict(quality='pass', context=o['context'], parallel=o['parallel'],
            prompt_tokens=o['context'] - 100, output_tokens=100,
            duration_seconds=2, tokens_per_second=50, model_verified=True,
            model_sha256=digest(binding.model), build_digest=digest(binding.build))
        result = c.update(assignment, Update(assignment.job['sequence'] + 1,
            'completed', 'done', restored=True, summary=summary))
        assert result['state'] == 'completed' and not result.action
        assert load(root / 'journal')['phase'] == 'idle'
    elif mode == 'http-apply-rollback':
        a = FilesystemFixture(root)
        before = a.state()
        binding = FixtureBinding(b'model', b'build', revision, digest(b'original'),
                                 apply_enabled=True, mode='fail')
        result = c.poll_fixture(FixtureExecutor(root), binding)
        assert result.action == 'apply' and result.result == 'rolled_back'
        assert result['state'] == 'failed' and result['failure'] == 'execution'
        assert result['restored'] is True and a.state() == before
        j = load(root / 'journal')
        assert j['action'] == 'apply' and j['job_id'] == result['id']
        assert j['fixture_mode'] == 'fail' and j['phase'] == 'restored'
        assert j['restored'] is True and j['target']['config'] != before['config']
        assert load(root / 'apply_ids') == [result['id']]
    elif mode == 'lost-report':
        assignment = c.lease()
        assert assignment and not assignment.recovery_only
        result = FixtureExecutor(root).run(job_id=assignment.job['id'], budget_seconds=2)
        assert result['restored'] is True
        # Simulate loss of the terminal network report, not inference success.
        assert c.status(assignment.job['id'])['state'] == 'running'
    else:
        result = c.poll_fixture(WaitingFixture(root) if mode == 'cancel' else FixtureExecutor(root))
        assert result['restored'] is True and not result.get('summary')
        assert result['state'] == ('cancelled' if mode == 'cancel' else 'failed')
        assert result.get('failure', '') == {'cancel':'', 'poll':'execution', 'recover':'restart'}[mode]
finally:
    c.close()
`

func TestBenchmarkPythonExecutorHTTP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires POSIX flock and process groups")
	}
	python := os.Getenv("NOCTURNE_TEST_PYTHON")
	if python == "" {
		var err error
		python, err = exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 absent; set NOCTURNE_TEST_PYTHON to a Python with httpx")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	probe := exec.CommandContext(ctx, python, "-c", "import httpx")
	err := probe.Run()
	cancel()
	if err != nil {
		t.Skip("Python/httpx unavailable; set NOCTURNE_TEST_PYTHON to a Python with httpx")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	revCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	revCmd.Dir = repo
	revBytes, err := revCmd.Output()
	cancel()
	if err != nil {
		t.Fatal("cannot resolve fixture code revision")
	}
	revision := strings.TrimSpace(string(revBytes))
	run := func(t *testing.T, mode, root, url string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, python, "-c", executorPython, mode, root, url, revision)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		output := &executorOutput{}
		cmd.Stdout, cmd.Stderr = output, output
		cmd.WaitDelay = time.Second
		if err := cmd.Run(); err != nil {
			t.Errorf("Python fixture %s failed (diagnostics redacted, %d bytes; deadline=%v)", mode, output.n, ctx.Err())
		}
		if output.overflow {
			t.Errorf("Python fixture %s exceeded diagnostic limit", mode)
		}
	}
	call := func(t *testing.T, base, method, path, token string, status int) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, base+path, bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			t.Fatal("fixture HTTP request failed")
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 16385))
		if err != nil || len(data) > 16384 {
			t.Fatal("fixture response limit/read failure")
		}
		if res.StatusCode != status {
			t.Fatalf("%s %s: status %d, want %d", method, path, res.StatusCode, status)
		}
		var v map[string]any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatal("invalid fixture JSON")
		}
		return v
	}
	t.Run("explicit-apply-health-rollback", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "fixture")
		run(t, "init", root, "")
		storeDir := t.TempDir()
		hash := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
		setup := func() (*WorkerCoordinator, *httptest.Server, *httptest.Server) {
			a, c, cfg := adminFixture(t)
			var err error
			c.Benchmarks, err = benchmark.Open(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Benchmarks.Close() })
			c.BenchmarkTargets = map[string]BenchmarkTarget{"w": {
				ModelSHA256: hash("model"), BuildDigest: hash("build"),
				CodeRevision: revision, ConfigGeneration: hash("original"), ApplyEnabled: true,
			}}
			startWorker(t, c)
			admin, worker := httptest.NewServer(a.Admin(cfg)), httptest.NewServer(c.Worker())
			t.Cleanup(admin.Close)
			t.Cleanup(worker.Close)
			return c, admin, worker
		}
		c, admin, worker := setup()
		base := "/admin/machines/w/benchmarks"
		created := call(t, admin.URL, "POST", base, "admin", 202)
		id, ok := created["id"].(string)
		if !ok {
			t.Fatal("missing job id")
		}
		path := base + "/" + id
		call(t, admin.URL, "POST", path+"/apply", "admin", 409)
		// SYNTHETIC worker measurement attestation only: no inference occurred.
		run(t, "synthetic-prerequisite", root, worker.URL)
		prior := call(t, admin.URL, "GET", path, "admin", 200)
		if prior["state"] != "completed" || prior["summary"] == nil || prior["action"] != nil {
			t.Fatal("invalid synthetic prerequisite or automatic Apply")
		}
		call(t, admin.URL, "POST", path+"/apply", "wrong", 401)
		queued := call(t, admin.URL, "POST", path+"/apply", "admin", 202) // literal {} body
		if queued["action"] != "apply" || queued["state"] != "queued" {
			t.Fatal("explicit Apply not queued")
		}
		if fenced, err := c.Benchmarks.Fenced("w"); err != nil || !fenced {
			t.Fatal("Apply did not reserve fence")
		}
		// Actual typed WorkerHTTP -> FixtureExecutor.apply -> independent watchdog.
		// Trusted fixture mode makes target health fail; no result is fabricated.
		run(t, "http-apply-rollback", root, worker.URL)
		run(t, "verify", root, "")
		check := func() map[string]any {
			t.Helper()
			j := call(t, admin.URL, "GET", path, "admin", 200)
			if j["action"] != "apply" || j["result"] != "rolled_back" || j["state"] != "failed" ||
				j["failure"] != "execution" || j["restored"] != true || j["apply_request"] == nil ||
				!reflect.DeepEqual(j["summary"], prior["summary"]) || !reflect.DeepEqual(j["provenance"], prior["provenance"]) {
				t.Fatal("unexpected persisted Apply rollback/evidence")
			}
			if fenced, err := c.Benchmarks.Fenced("w"); err != nil || fenced {
				t.Fatal("verified rollback did not release fence")
			}
			return j
		}
		terminal := check()
		worker.Close()
		admin.Close()
		if err := c.Benchmarks.Close(); err != nil {
			t.Fatal(err)
		}
		c, admin, worker = setup() // Reopen disk state, not just an in-memory GET.
		if !reflect.DeepEqual(check(), terminal) {
			t.Fatal("restart changed terminal rollback")
		}
		call(t, admin.URL, "POST", path+"/apply", "admin", 409)
		// Interrupted Apply and failed restoration/fencing are exercised by worker
		// tests test_apply_watchdog_death_durable_restart and
		// test_apply_rollback_unverified_keeps_fence.
	})
	for _, mode := range []string{"poll", "cancel", "recover"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "fixture")
			run(t, "init", root, "")
			storeDir := t.TempDir()
			setup := func() (*WorkerCoordinator, *httptest.Server, *httptest.Server) {
				a, c, cfg := adminFixture(t)
				var err error
				c.Benchmarks, err = benchmark.Open(storeDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Benchmarks.Close() })
				c.BenchmarkTargets = map[string]BenchmarkTarget{"w": {ModelSHA256: strings.Repeat("a", 64), BuildDigest: strings.Repeat("b", 64)}}
				startWorker(t, c)
				admin, worker := httptest.NewServer(a.Admin(cfg)), httptest.NewServer(c.Worker())
				t.Cleanup(admin.Close)
				t.Cleanup(worker.Close)
				return c, admin, worker
			}
			c, admin, worker := setup()
			path := "/admin/machines/w/benchmarks"
			call(t, admin.URL, "POST", path, "wrong", 401)
			call(t, worker.URL, "POST", "/worker/benchmark/lease", "wrong", 401)
			job := call(t, admin.URL, "POST", path, "admin", 202)
			id, ok := job["id"].(string)
			if !ok {
				t.Fatal("missing job id")
			}
			if mode == "recover" {
				run(t, "lost-report", root, worker.URL)
				worker.Close()
				admin.Close()
				if err := c.Benchmarks.Close(); err != nil {
					t.Fatal(err)
				}
				c, admin, worker = setup() // New coordinator AND reopened durable store.
				j := call(t, admin.URL, "GET", path+"/"+id, "admin", 200)
				if j["state"] != "recovery_required" {
					t.Fatal("restart did not require recovery")
				}
				if fenced, err := c.Benchmarks.Fenced("w"); err != nil || !fenced {
					t.Fatal("restart lost fence")
				}
			}
			if mode == "cancel" {
				done := make(chan struct{})
				go func() { defer close(done); run(t, mode, root, worker.URL) }()
				// Join even on a failed assertion before temporary directories are removed.
				defer func() { <-done }()
				deadline := time.Now().Add(8 * time.Second)
				for {
					data, _ := os.ReadFile(filepath.Join(root, "journal"))
					var j struct {
						Phase string
						PGID  int
						JobID string `json:"job_id"`
					}
					_ = json.Unmarshal(data, &j)
					if j.Phase == "running" && j.PGID > 0 && j.JobID == id {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("watchdog child never reached running state")
					}
					time.Sleep(20 * time.Millisecond)
				}
				j := call(t, admin.URL, "POST", path+"/"+id+"/cancel", "admin", 200)
				if j["state"] != "cancelling" {
					t.Fatal("cancel did not await restoration")
				}
				<-done
			} else {
				run(t, mode, root, worker.URL)
			}
			j := call(t, admin.URL, "GET", path+"/"+id, "admin", 200)
			want, failure := "failed", "execution"
			if mode == "cancel" {
				want, failure = "cancelled", ""
			}
			if mode == "recover" {
				failure = "restart"
			}
			gotFailure, _ := j["failure"].(string)
			if j["state"] != want || j["restored"] != true || gotFailure != failure || j["summary"] != nil {
				t.Fatal("unexpected terminal fixture report")
			}
			if fenced, err := c.Benchmarks.Fenced("w"); err != nil || fenced {
				t.Fatal("verified restoration did not release fence")
			}
			run(t, "verify", root, "")
			call(t, admin.URL, "POST", path+"/"+id+"/apply", "admin", 409)
		})
	}
}
