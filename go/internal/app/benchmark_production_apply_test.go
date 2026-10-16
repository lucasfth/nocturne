// SPDX-License-Identifier: AGPL-3.0-or-later
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Synthetic completed evidence is seeded explicitly; this tests real production
// Apply policy/config/root decisions and Go HTTP reconciliation, NOT inference.
// Kernel peer/ACL attestation and launchctl are faked; no installed service runs.
func TestBenchmarkProductionApplyHTTP(t *testing.T) {
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
	hash := func(value string) string { sum := sha256.Sum256([]byte(value)); return hex.EncodeToString(sum[:]) }
	for _, mode := range []string{"success", "health_failure", "lost_prepared_ack", "lost_coordinator_prepared_ack", "lost_finalize_ack", "crash_before_rename", "crash_after_rename"} {
		t.Run(mode, func(t *testing.T) {
			a, c, cfg := adminFixture(t)
			var err error
			c.Benchmarks, err = benchmark.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Benchmarks.Close()
			c.BenchmarkTargets = map[string]BenchmarkTarget{"w": {
				ModelSHA256: hash("disposable-model"), BuildDigest: hash("disposable-server"),
				ConfigGeneration: strings.Repeat("c", 64), CodeRevision: strings.Repeat("d", 40), ApplyEnabled: true,
			}}
			startWorker(t, c)
			admin, worker := httptest.NewServer(a.Admin(cfg)), httptest.NewServer(c.Worker())
			defer admin.Close()
			defer worker.Close()
			for _, endpoint := range []string{admin.URL + "/admin/machines/w/benchmarks", worker.URL + "/worker/benchmark/lease", worker.URL + "/worker/benchmark/reconcile"} {
				req, err := http.NewRequest("POST", endpoint, strings.NewReader(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer wrong")
				req.Header.Set("Content-Type", "application/json")
				res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
				if err != nil {
					t.Fatal("authentication HTTP check failed")
				}
				res.Body.Close()
				if res.StatusCode != 401 {
					t.Fatalf("unauthenticated status %d", res.StatusCode)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, python, "-m", "tests.test_production_apply_http", root, worker.URL, admin.URL, mode)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			cmd.WaitDelay = time.Second
			output := &executorOutput{}
			cmd.Stdout, cmd.Stderr = output, output
			if err := cmd.Run(); err != nil {
				diagnostic, _ := os.ReadFile(filepath.Join(root, "diagnostic"))
				t.Fatalf("production Apply failed (type/lines only: %s; output bytes=%d; deadline=%v)", diagnostic, output.n, ctx.Err())
			}
			if output.overflow {
				t.Fatal("diagnostic limit exceeded")
			}
			raw, err := os.ReadFile(filepath.Join(root, "result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				ID    string `json:"id"`
				Phase string `json:"phase"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest("GET", admin.URL+"/admin/machines/w/benchmarks/"+result.ID, nil)
			req.Header.Set("Authorization", "Bearer admin")
			res, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
			if err != nil {
				t.Fatal("terminal HTTP check failed")
			}
			defer res.Body.Close()
			var terminal map[string]any
			if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&terminal) != nil {
				t.Fatal("invalid terminal response")
			}
			if terminal["result"] != result.Phase || terminal["apply_phase"] != result.Phase || terminal["summary"] == nil {
				t.Fatal("incorrect terminal decision/evidence")
			}
			if fenced, err := c.Benchmarks.Fenced("w"); err != nil || fenced {
				t.Fatal("terminal agreement did not release Go fence")
			}
		})
	}
}
