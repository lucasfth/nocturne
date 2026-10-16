// SPDX-License-Identifier: AGPL-3.0-or-later
package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lucasfth/nocturne/go/internal/benchmark"
)

func TestBenchmarkTrustedProvenance(t *testing.T) {
	a, c, cfg := benchmarkFixture(t)
	target := c.BenchmarkTargets["w"]
	target.CodeRevision = strings.Repeat("c", 40)
	target.ConfigGeneration = strings.Repeat("d", 64)
	c.BenchmarkTargets["w"] = target
	path := "/admin/machines/w/benchmarks"
	for _, body := range []string{`{"actor":"someone"}`, `{"code_revision":"abc"}`, `{"config_generation":"abc"}`, `{"advanced":{"reserved_tokens":8}}`, `{"advanced":{"output_allowance":128}}`} {
		if w := ar(a, cfg, "POST", path, body, "admin", ""); w.Code != 400 {
			t.Fatal("browser supplied trusted fields", w.Code)
		}
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.RemoteAddr = "127.0.0.1:123"
	req.Header.Set("Tailscale-User-Login", "allowed")
	w := httptest.NewRecorder()
	a.Admin(cfg).ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	var j benchmark.Job
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	if j.Provenance.Actor != "admin-bearer" || j.Provenance.CodeRevision != target.CodeRevision || j.Provenance.ConfigGeneration != target.ConfigGeneration {
		t.Fatal("incorrect provenance")
	}
	original := j.Provenance
	target.ConfigGeneration = strings.Repeat("e", 64)
	c.BenchmarkTargets["w"] = target
	got, err := c.Benchmarks.Get("w", j.ID)
	if err != nil || got.Provenance != original {
		t.Fatal("startup binding changed historical provenance", err)
	}
	if w := ar(a, cfg, "POST", path+"/"+j.ID+"/apply", `{}`, "admin", ""); w.Code != 409 {
		t.Fatal("apply must remain unsupported", w.Code)
	}
}
