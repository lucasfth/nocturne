// SPDX-License-Identifier: AGPL-3.0-or-later
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lucasfth/nocturne/go/internal/benchmark"
)

// Cleanroom only: real HTTP and production transaction/restoration, synthetic
// artifact evidence and fake launchctl. Does not attest installation or inference.
const productionPython = `
import asyncio, contextlib, json, os, sys
from pathlib import Path
from types import SimpleNamespace
from worker import benchmark_production as p
from worker.benchmark_service import ServiceAdapter, LABELS, LLAMA
from worker.benchmark_executor import Update, atomic, load
root, url, mode = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
def diagnostic(kind, value, tb):
    frames = []
    while tb:
        frames.append(str(tb.tb_lineno))
        tb = tb.tb_next
    (root/'diagnostic').write_text(kind.__name__ + ':' + ','.join(frames))
sys.excepthook = diagnostic
p.STATE = root / 'production'
service_root = root / 'service'
service_root.mkdir(mode=0o700)
class Launchctl:
    def __init__(self):
        self.states = {k: dict(loaded=True, disabled=False) for k in LABELS}
        self.calls = []
    def state(self): return {k: dict(v) for k,v in self.states.items()}
    def loaded(self,k): return self.states[k]['loaded']
    def mutate(self,verb,label):
        assert label == LLAMA
        self.calls.append((verb,label))
        self.states[label][{'enable':'disabled','disable':'disabled',
                           'bootstrap':'loaded','bootout':'loaded'}[verb]] = verb in ('disable','bootstrap')
lc = Launchctl()
service = ServiceAdapter(service_root, lc, lambda: 'c'*64,
                        health=lambda: True, closed=lambda port: True, absent=lambda: True)
# Test-only lifecycle observation; privileged socket/kernel attestation is covered
# separately. No production fallback permits an unbound fencing session.
from worker import benchmark_service as service_module
service.peer = dict(pid=12345, uid=501, start_sec=100, start_usec=42)
service_module.runner_absent = lambda peer: True
service.ping = lambda: {'service_control': True, 'benchmark_execution': True}
service.inventory = lambda: SimpleNamespace(model_directory=root, server=root/'server', code_revision='d'*40)
prior = service.snapshot()
(root/'server').write_bytes(b'cleanroom')
p.installed_models = lambda directory: [SimpleNamespace(sha256='a'*64)]
p.artifacts._protected_open = lambda path, **kw: os.open(path, os.O_RDONLY)
p.artifacts.digest_fd = lambda fd: 'b'*64
@contextlib.asynccontextmanager
async def artifacts(server, models):
    yield SimpleNamespace(config={}, backend_factory=None, verify=lambda: None)
p.open_artifacts = artifacts
p.benchmark.plan = lambda config: {}
p.benchmark.Lock = contextlib.nullcontext
ran = []
async def execute(config, report, **kw):
    # Harmless actual child, reaped before production transaction restores.
    child = await asyncio.create_subprocess_exec(sys.executable, '-c', 'pass')
    assert await child.wait() == 0
    ran.append(True)
    assert not lc.loaded(LLAMA)
    if mode == 'failure': raise RuntimeError('synthetic execution failure')
    profile = dict(context=config['min_context'], parallel=config['profiles'][0]['parallel'], passed=True,
        quality={k: True for k in ('caller_recipient_direction','grounded_retrieval','instruction_following','structured_json')},
        measurements=[dict(length='long',warmup=False,prompt_tokens=config['min_context']-264,
                           decode_tokens=16,decode_s=2,decode_tps=8)])
    report.update(status='completed',selected='synthetic',
        cleanup={k: True for k in ('owned_processes_stopped','owned_temp_removed','final_safety_verified')},
        candidates=[dict(id='synthetic',status='passed',profiles=[profile])])
p.benchmark.execute = execute
http = p.BenchmarkHTTP(url, 'w', 'worker')
try:
    assignment = http.lease()
    assert assignment and not assignment.action and not assignment.recovery_only
    job = assignment.job
    path = p.journal_root() / 'journal.json'
    atomic(path, dict(binding=p.binding(job),job=dict(job),lease=assignment.lease,started=False))
    result = asyncio.run(p.transaction(job,service))
    assert ran and result['restored'] and service.snapshot() == prior
    assert lc.calls and all(label == LLAMA for _,label in lc.calls)
    assert json.loads((service_root/'admission.json').read_text())['active'] is False
    assert p.replay(job,load(path),service) == result
    summary = result['summary']
    assert bool(summary) == (mode == 'success')
    terminal = http.update(assignment,Update(job['sequence']+1,
        'completed' if summary else 'failed','done','' if summary else 'execution',True,summary))
    assert terminal['restored'] is True
finally:
    http.close()
`

func TestBenchmarkProductionTransactionHTTP(t *testing.T) {
	python := os.Getenv("NOCTURNE_TEST_PYTHON")
	if python == "" {
		python = "python3"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := exec.CommandContext(ctx, python, "-c", "import httpx").Run()
	cancel()
	if err != nil {
		t.Skip("Python/httpx unavailable; set NOCTURNE_TEST_PYTHON")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"success", "failure"} {
		t.Run(mode, func(t *testing.T) {
			a, c, cfg := adminFixture(t)
			c.Benchmarks, err = benchmark.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Benchmarks.Close()
			c.BenchmarkTargets = map[string]BenchmarkTarget{"w": {ModelSHA256: strings.Repeat("a", 64), BuildDigest: strings.Repeat("b", 64), ConfigGeneration: strings.Repeat("c", 64), CodeRevision: strings.Repeat("d", 40)}}
			startWorker(t, c)
			admin, worker := httptest.NewServer(a.Admin(cfg)), httptest.NewServer(c.Worker())
			defer admin.Close()
			defer worker.Close()
			call := func(base, method, path, token string, status int) map[string]any {
				t.Helper()
				req, e := http.NewRequest(method, base+path, bytes.NewBufferString(`{}`))
				if e != nil {
					t.Fatal(e)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				res, e := (&http.Client{Timeout: 3 * time.Second}).Do(req)
				if e != nil {
					t.Fatal("cleanroom HTTP failed")
				}
				defer res.Body.Close()
				if res.StatusCode != status {
					t.Fatalf("HTTP status %d want %d", res.StatusCode, status)
				}
				var v map[string]any
				json.NewDecoder(res.Body).Decode(&v)
				return v
			}
			base := "/admin/machines/w/benchmarks"
			call(admin.URL, "POST", base, "wrong", 401)
			call(worker.URL, "POST", "/worker/benchmark/lease", "wrong", 401)
			job := call(admin.URL, "POST", base, "admin", 202)
			id, ok := job["id"].(string)
			if !ok {
				t.Fatal("missing id")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, python, "-c", productionPython, root, worker.URL, mode)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			cmd.WaitDelay = time.Second
			output := &executorOutput{}
			cmd.Stdout, cmd.Stderr = output, output
			if e := cmd.Run(); e != nil {
				diagnostic, _ := os.ReadFile(filepath.Join(root, "diagnostic"))
				t.Fatalf("production cleanroom failed (type/line numbers only: %s; %d bytes; deadline=%v)", diagnostic, output.n, ctx.Err())
			}
			if output.overflow {
				t.Fatal("diagnostic limit exceeded")
			}
			terminal := call(admin.URL, "GET", base+"/"+id, "admin", 200)
			want := "completed"
			if mode == "failure" {
				want = "failed"
			}
			if terminal["state"] != want || terminal["restored"] != true {
				t.Fatal("unexpected terminal restoration")
			}
			if fenced, e := c.Benchmarks.Fenced("w"); e != nil || fenced {
				t.Fatal("restoration did not release fence")
			}
			call(admin.URL, "POST", base+"/"+id+"/apply", "admin", 409)
		})
	}
}
