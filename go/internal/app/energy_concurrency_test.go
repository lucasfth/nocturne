package app

import (
	"context"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"testing"
	"time"
)

func TestStalledEnergyProviderDoesNotBlockWorkerLifecycle(t *testing.T) {
	a, c, _ := workerFixture(t)
	startWorker(t, c)
	submitWorker(t, c, "result", true, false)
	j := getLease(t, c)
	submitWorker(t, c, "cancel", true, false)
	area := "DK2"
	a.Energy.Config.Energy = &energy.EnergyConfig{PriceArea: &area}
	entered, release := make(chan struct{}), make(chan struct{})
	a.Energy.Provider = func(context.Context, string) ([]energy.Row, error) {
		close(entered)
		<-release
		return nil, energy.ErrUnavailable
	}
	done := make(chan struct{})
	go func() { defer close(done); a.Energy.Refresh(context.Background()) }()
	<-entered
	defer func() { close(release); <-done }()
	responsive := make(chan struct{})
	go func() {
		wr(c, "heartbeat", beatBody)
		wr(c, "result", resultBody(j, `"chunk":"done","final":true`))
		c.Cancel("cancel")
		c.DropWorker("w")
		c.Maintain()
		close(responsive)
	}()
	select {
	case <-responsive:
	case <-time.After(time.Second):
		t.Fatal("provider stalled worker results/cancel/rotation/maintenance")
	}
}
