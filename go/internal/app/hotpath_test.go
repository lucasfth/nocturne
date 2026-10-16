package app

import (
	"context"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"github.com/lucasfth/nocturne/go/internal/policy"
	"net/http"
	"strings"
	"testing"
	"time"
)

func assertQueuedCounts(t *testing.T, c *WorkerCoordinator) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, bulk := range []bool{false, true} {
		want := 0
		for _, j := range c.jobs {
			if j.status == "queued" && j.Bulk == bulk {
				want++
			}
		}
		if got := c.pending(bulk); got != want || got < 0 {
			t.Fatalf("bulk=%v pending=%d scan=%d", bulk, got, want)
		}
	}
}
func TestQueuedCountersTransitions(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(fmt.Sprint(bulk), func(t *testing.T) {
			_, c, _ := workerFixture(t)
			startWorker(t, c)
			now := c.Config.Monotonic()
			c.Config.Monotonic = func() float64 { return now }
			submitWorker(t, c, "cancel", false, bulk)
			assertQueuedCounts(t, c)
			c.Cancel("cancel")
			c.Cancel("cancel")
			assertQueuedCounts(t, c)
			ch := submitWorker(t, c, "retry", false, bulk)
			assertQueuedCounts(t, c)
			first := getLease(t, c)
			assertQueuedCounts(t, c)
			wr(c, "result", resultBody(first, `"chunk":"discard","reasoning":"old"`))
			wr(c, "fail", resultBody(first, `"error":"retry"`))
			assertQueuedCounts(t, c)
			second := getLease(t, c)
			assertQueuedCounts(t, c)
			wr(c, "result", resultBody(second, `"chunk":"new😀","reasoning":"fresh","final":true`))
			assertQueuedCounts(t, c)
			if e := <-ch; e.Text != "new😀" || e.Reasoning != "fresh" {
				t.Fatal(e)
			}
			wr(c, "result", resultBody(second, `"final":true`))
			c.Cancel("retry")
			assertQueuedCounts(t, c)
			submitWorker(t, c, "twice", false, bulk)
			failFirst := getLease(t, c)
			assertQueuedCounts(t, c)
			wr(c, "fail", resultBody(failFirst, `"error":"retry"`))
			assertQueuedCounts(t, c)
			failSecond := getLease(t, c)
			assertQueuedCounts(t, c)
			wr(c, "fail", resultBody(failSecond, `"error":"terminal"`))
			assertQueuedCounts(t, c)
			ctx, cancel := context.WithCancel(context.Background())
			_, err := c.Submit(ctx, Job{ID: "context", Bulk: bulk})
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			deadline := time.Now().Add(time.Second)
			for c.Pending(bulk) != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			assertQueuedCounts(t, c)
			submitWorker(t, c, "timeout", false, bulk)
			now += 3
			c.Maintain()
			assertQueuedCounts(t, c)
			now += 61
			c.Maintain()
			assertQueuedCounts(t, c)
			if len(c.jobs) != 0 || len(c.queue) != 0 || len(c.bulk) != 0 {
				t.Fatalf("stale jobs/queues: %d/%d/%d", len(c.jobs), len(c.queue), len(c.bulk))
			}
		})
	}
}
func TestResultLargeUnicodeAndFinalOrdering(t *testing.T) {
	c := NewWorkerCoordinator(WorkerConfig{}, nil, nil)
	for _, stream := range []bool{false, true} {
		var billed int64
		ch, err := c.Submit(context.Background(), Job{ID: fmt.Sprint(stream), Stream: stream, Body: map[string]any{"max_tokens": int64(100000)}, Finish: func(raw int64, _ *int64, cancel bool) error {
			if cancel {
				t.Fatal("cancelled")
			}
			billed = raw
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		j := c.jobs[fmt.Sprint(stream)]
		c.setStatus(j, "leased")
		w := &workerResponse{header: make(http.Header)}
		for i := 0; i < 4096; i++ {
			w.body.Reset()
			c.result(w, map[string]any{"chunk": "😀abcdefg", "reasoning": "éthought", "final": i == 4095}, j)
			if stream {
				e := <-ch
				if e.Text != "😀abcdefg" || e.Reasoning != "éthought" || e.Final != (i == 4095) {
					t.Fatal(e)
				}
				if e.Final && billed != 4096*16 {
					t.Fatal(billed)
				}
			}
		}
		if !stream {
			e := <-ch
			if e.Text != strings.Repeat("😀abcdefg", 4096) || e.Reasoning != strings.Repeat("éthought", 4096) || !e.Final {
				t.Fatal("accumulation mismatch")
			}
		}
		if billed != 4096*16 {
			t.Fatal(billed)
		}
		if stream && (j.text.Len() != 0 || j.reasoning.Len() != 0) {
			t.Fatal("stream retained output")
		}
		if _, ok := <-ch; ok {
			t.Fatal("not closed")
		}
	}
}

var hotpathSink int

func BenchmarkHotpathPendingBatch(b *testing.B) {
	for _, n := range []int{256, 1024, 4096} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			c := NewWorkerCoordinator(WorkerConfig{}, nil, nil)
			for i := 0; i < n; i++ {
				j := &workerJob{status: "queued"}
				c.jobs[fmt.Sprint(i)] = j
			}
			c.queuedInteractive = n
			b.ReportAllocs()
			b.ResetTimer()
			for z := 0; z < b.N; z++ {
				for i := 0; i < n; i++ {
					hotpathSink = c.pending(false)
				}
			}
		})
	}
}
func BenchmarkHotpathWorkerResult(b *testing.B) {
	for _, n := range []int{256, 1024, 4096} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			c := NewWorkerCoordinator(WorkerConfig{}, nil, nil)
			chunk := map[string]any{"chunk": strings.Repeat("x", 16)}
			w := &workerResponse{header: make(http.Header), status: 200}
			b.ReportAllocs()
			b.ResetTimer()
			for z := 0; z < b.N; z++ {
				j := &workerJob{Job: Job{Body: map[string]any{"max_tokens": int64(1000000)}}, events: make(chan Event, 65), status: "leased"}
				for i := 0; i < n; i++ {
					w.body.Reset()
					c.result(w, chunk, j)
				}
				hotpathSink = j.text.Len()
			}
		})
	}
}
func BenchmarkHotpathServingBatch(b *testing.B) {
	for _, n := range []int{256, 1024, 4096} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := energy.New(&energy.Config{}, nil, func() time.Time { return time.Unix(0, 0) })
			c := NewWorkerCoordinator(WorkerConfig{}, nil, e)
			active := false
			for i := 0; i < n; i++ {
				c.workers[fmt.Sprint(i)] = &workerSession{jobs: map[string]bool{}, view: energy.Worker{Policy: policy.Worker{OnAC: true, BatteryPct: 100, ThermalState: "nominal", UserActive: &active, Ready: true, Seen: energy.Monotonic()}}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for z := 0; z < b.N; z++ {
				for _, s := range c.workers {
					p := s.view.Policy
					p.Jobs = len(s.jobs)
					if e.Serves(p) {
						hotpathSink++
					}
				}
			}
		})
	}
}
