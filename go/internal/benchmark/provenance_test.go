// SPDX-License-Identifier: AGPL-3.0-or-later
package benchmark

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestAuditDurableImmutableAndLegacy(t *testing.T) {
	s, dir := openTest(t)
	audit := Audit{Actor: "admin-bearer", CodeRevision: strings.Repeat("c", 40), ConfigGeneration: strings.Repeat("d", 64)}
	j, err := s.StartWithProvenance("worker", testOptions(), audit)
	if err != nil {
		t.Fatal(err)
	}
	original := j.Provenance
	j.Provenance.Actor = "forged"
	a := leaseTest(t, s, "worker")
	if a.Job.Provenance != original {
		t.Fatal("lease changed provenance")
	}
	completion := completeFor(a.Job)
	completion.Summary.OutputAllowance = completion.Summary.OutputTokens
	completion.Summary.ReservedTokens = 8
	completion.Summary.PromptTokens -= 104 // 8 reserved + maximum 96 packing slack
	if _, err = s.Update("worker", j.ID, a.Token, completion); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get("worker", j.ID)
	if err != nil || got.Provenance != original {
		t.Fatal("provenance not durable", err)
	}
	if got.Summary == nil || *got.Summary != *completion.Summary {
		t.Fatal("measured evidence not durable")
	}
	legacy := startTest(t, s, "old-worker")
	raw, err := encode(s.jobs)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := decode(raw)
	if err != nil || jobs[legacy.ID].Job.Provenance.Audit != (Audit{}) {
		t.Fatal("legacy audit fabricated", err)
	}
	// Optional fields really are omitted, so pre-extension JSON remains readable.
	b, _ := json.Marshal(legacy.Provenance)
	if strings.Contains(string(b), "actor") || strings.Contains(string(b), "code_revision") {
		t.Fatal("legacy fields not absent")
	}
	for _, bad := range []Audit{{Actor: "Bearer secret"}, {CodeRevision: "/bin/tool"}, {ConfigGeneration: "generation"}} {
		if _, err := s.StartWithProvenance("invalid", testOptions(), bad); err != ErrInvalidOptions {
			t.Fatal(err)
		}
	}
	got.Provenance.CodeRevision = "bad"
	r := jobs[j.ID]
	r.Job = got
	if validRecord(r) {
		t.Fatal("invalid persisted audit accepted")
	}
}

func TestPythonLongRowEvidence(t *testing.T) {
	o, _ := testOptions().Normalize()
	j := Job{Options: o, Provenance: Provenance{ModelSHA256: o.ModelSHA256, BuildDigest: o.BuildDigest}}
	base := *summaryFor(j)
	base.OutputAllowance = base.OutputTokens
	base.ReservedTokens = 8
	base.PromptTokens = o.Context - base.OutputAllowance - 8
	for _, slack := range []int{0, 1, 96} {
		row := base
		row.PromptTokens -= slack
		if !row.valid(o) {
			t.Fatalf("Python packing slack %d rejected", slack)
		}
	}
	for name, mutate := range map[string]func(*Summary){
		"packing slack":          func(s *Summary) { s.PromptTokens -= 97 },
		"over budget":            func(s *Summary) { s.PromptTokens += 9 },
		"reserve overlap":        func(s *Summary) { s.PromptTokens++ },
		"short row":              func(s *Summary) { s.PromptTokens = 1024 },
		"negative reserve":       func(s *Summary) { s.ReservedTokens = -1 },
		"overflow allowance":     func(s *Summary) { s.OutputAllowance = math.MaxInt },
		"overflow reserve":       func(s *Summary) { s.ReservedTokens = math.MaxInt },
		"allowance below actual": func(s *Summary) { s.OutputAllowance-- },
		"wall instead of decode": func(s *Summary) { s.DurationSeconds *= 2 },
	} {
		t.Run(name, func(t *testing.T) {
			row := base
			mutate(&row)
			if row.valid(o) {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	// Output allowance is a budget, not a reason to invent generated tokens.
	row := base
	row.OutputTokens /= 2
	row.TokensPerSecond /= 2
	if !row.valid(o) {
		t.Fatal("honest measured output rejected")
	}
}
