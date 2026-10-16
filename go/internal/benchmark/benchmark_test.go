// SPDX-License-Identifier: AGPL-3.0-or-later

package benchmark

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Lookup benchmarks intentionally exclude fsync and allocation/setup of fixture
// maps. Every job is outstanding and unexpired. The fixed clock isolates lookup
// and locking costs rather than wall-clock calls or deadline recovery writes.
func lookupFixture(size int) (*Store, string, string) {
	s := newStore("", nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.persist = func([]byte) error { panic("lookup benchmark must not write") }
	o, _ := testOptions().Normalize()
	machine, id := "", ""
	for i := range size {
		machine, id = fmt.Sprintf("worker-%03d", i), fmt.Sprintf("%064x", i)
		s.jobs[id] = record{Job: Job{
			ID: id, Machine: machine, Options: o, State: Queued, Progress: Pending, UpdatedAt: now,
			Provenance: Provenance{
				SchemaVersion: SchemaVersion, PolicyVersion: PolicyVersion, FixtureVersion: FixtureVersion,
				WorkerID: machine, CreatedAt: now, DeadlineAt: now.Add(600 * time.Second),
				ModelSHA256: o.ModelSHA256, BuildDigest: o.BuildDigest,
			},
		}}
	}
	s.index()
	return s, machine, id
}

func BenchmarkGetMapLookup(b *testing.B) {
	for _, size := range []int{1, 16, 64, 128, MaxJobs} {
		b.Run(fmt.Sprintf("jobs=%d", size), func(b *testing.B) {
			s, machine, id := lookupFixture(size)
			b.ReportAllocs()
			for b.Loop() {
				j, err := s.Get(machine, id)
				if err != nil || j.ID != id {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkFencedMapLookup(b *testing.B) {
	for _, size := range []int{1, 16, 64, 128, MaxJobs} {
		b.Run(fmt.Sprintf("jobs=%d", size), func(b *testing.B) {
			s, machine, _ := lookupFixture(size)
			b.ReportAllocs()
			for b.Loop() {
				fenced, err := s.Fenced(machine)
				if err != nil || !fenced {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMissingMapLookup(b *testing.B) {
	for _, size := range []int{1, 16, 64, 128, MaxJobs} {
		b.Run(fmt.Sprintf("jobs=%d", size), func(b *testing.B) {
			s, _, _ := lookupFixture(size)
			missingID := strings.Repeat("f", 64)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.Get("unknown", missingID); err != ErrNotFound {
					b.Fatal(err)
				}
			}
		})
	}
}
