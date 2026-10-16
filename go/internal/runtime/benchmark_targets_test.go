// SPDX-License-Identifier: AGPL-3.0-or-later
package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func targetDocument() []byte {
	h := strings.Repeat("a", 64)
	b, _ := json.Marshal(benchmarkTargetFile{Version: 1, Targets: []benchmarkTargetEntry{{Machine: "w", ModelSHA256: h, BuildDigest: h, CodeRevision: strings.Repeat("b", 40), ConfigGeneration: h, Installation: benchmarkInstallation{1, h, true, true, true, true, true, true, true}}}})
	return b
}
func TestBenchmarkTargetSchema(t *testing.T) {
	raw := targetDocument()
	enabled := func(id string) bool { return id == "w" }
	targets, e := parseBenchmarkTargets(raw, enabled)
	if e != nil || targets["w"].ApplyEnabled || targets["w"].CodeRevision == "" {
		t.Fatal("valid binding rejected or Apply enabled")
	}
	for _, key := range []string{"reviewed_installation", "service_control", "benchmark_execution", "independent_watchdog", "durable_restoration", "admission_fencing", "artifact_verification"} {
		t.Run(key, func(t *testing.T) {
			for _, s := range []string{strings.Replace(string(raw), `"`+key+`":true`, `"`+key+`":false`, 1), strings.Replace(string(raw), `"`+key+`":true`, `"`+key+`":null`, 1)} {
				if _, e := parseBenchmarkTargets([]byte(s), enabled); e == nil {
					t.Fatal("missing capability accepted")
				}
			}
		})
	}
	for _, s := range []string{
		strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(raw), `"version":1`, `"Version":1`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":2`, 1),
		strings.Replace(string(raw), `"capability_version":1`, `"capability_version":2`, 1),
		strings.Replace(string(raw), `"machine":"w"`, `"machine":"unknown"`, 1),
		strings.Replace(string(raw), `"machine":"w"`, `"machine":"w","apply_enabled":true`, 1),
		strings.Replace(string(raw), `"machine":"w"`, `"machine":"w","url":"https://example.invalid"`, 1),
		string(raw) + `{}`, `null`, strings.Repeat(" ", benchmarkTargetsLimit+1),
	} {
		if _, e := parseBenchmarkTargets([]byte(s), enabled); e == nil {
			t.Fatal("invalid schema accepted")
		}
	}
	if _, e := parseBenchmarkTargets(raw, func(string) bool { return false }); e == nil {
		t.Fatal("disabled registry accepted")
	}
	var doc benchmarkTargetFile
	json.Unmarshal(raw, &doc)
	doc.Targets = append(doc.Targets, doc.Targets[0])
	dup, _ := json.Marshal(doc)
	if _, e := parseBenchmarkTargets(dup, enabled); e == nil {
		t.Fatal("duplicate machine accepted")
	}
}
func TestBenchmarkTargetProtectedFailureAndAbsent(t *testing.T) {
	root := t.TempDir()
	if v, e := loadBenchmarkTargets(root, nil); e != nil || v != nil {
		t.Fatal("absent must disable")
	}
	path := filepath.Join(root, "benchmark-targets.json")
	if e := os.WriteFile(path, targetDocument(), 0600); e != nil {
		t.Fatal(e)
	}
	// Deliberately writable ancestor fails even when tests themselves run as root.
	if e := os.Chmod(root, 0777); e != nil {
		t.Fatal(e)
	}
	if _, e := protectedBenchmarkTargets(root); e == nil {
		t.Fatal("writable ancestor accepted")
	}
	os.Remove(path)
	if e := os.Symlink("missing", path); e != nil {
		t.Fatal(e)
	}
	if _, e := loadBenchmarkTargets(root, nil); e == nil {
		t.Fatal("symlink treated as absent")
	}
	if _, e := protectedBenchmarkTargets(filepath.Join(root, "..", filepath.Base(root))); e == nil {
		t.Fatal("unprotected path accepted")
	}
}

func TestProductionApplyRequiresProtectedInstalledCapability(t *testing.T) {
	var doc benchmarkTargetFile
	if err := json.Unmarshal(targetDocument(), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Targets[0].ProductionApply = &productionApplyInstallation{Protocol: 1, ManifestSHA256: strings.Repeat("c", 64), DurableDecisions: true, ConfigurationRollback: true, ReviewedInstallation: true}
	raw, _ := json.Marshal(doc)
	targets, err := parseBenchmarkTargets(raw, func(string) bool { return true })
	if err != nil || !targets["w"].ApplyEnabled {
		t.Fatal("explicit installed capability rejected", err)
	}
	for _, mutate := range []func(*productionApplyInstallation){
		func(a *productionApplyInstallation) { a.Protocol = 0 },
		func(a *productionApplyInstallation) { a.ManifestSHA256 = "" },
		func(a *productionApplyInstallation) { a.DurableDecisions = false },
		func(a *productionApplyInstallation) { a.ConfigurationRollback = false },
		func(a *productionApplyInstallation) { a.ReviewedInstallation = false },
	} {
		copy := *doc.Targets[0].ProductionApply
		mutate(&copy)
		candidate := doc
		candidate.Targets = append([]benchmarkTargetEntry(nil), doc.Targets...)
		candidate.Targets[0].ProductionApply = &copy
		bad, _ := json.Marshal(candidate)
		if _, err := parseBenchmarkTargets(bad, func(string) bool { return true }); err == nil {
			t.Fatal("incomplete Apply attestation accepted")
		}
	}
}
