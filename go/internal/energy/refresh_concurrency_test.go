package energy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshDoesNotBlockCachedStateAndIsSingleFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e := New(cfg(), func(context.Context, string) ([]Row, error) {
		calls.Add(1)
		close(entered)
		<-release
		return nil, ErrUnavailable
	}, nil)
	done := make(chan struct{})
	go func() { defer close(done); e.Refresh(context.Background()) }()
	<-entered
	defer func() { close(release); <-done }()
	responsive := make(chan struct{})
	go func() {
		e.State(nil)
		e.Rows()
		e.LastSuccess()
		e.SetLiveRate(123)
		for i := 0; i < 10; i++ {
			e.Refresh(context.Background())
		}
		close(responsive)
	}()
	select {
	case <-responsive:
	case <-time.After(time.Second):
		t.Fatal("cached operations blocked on provider")
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent provider calls=%d", calls.Load())
	}
}
