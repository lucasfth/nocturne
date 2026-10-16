// Offline HTTP shadow driver. Never uses production listeners or external providers.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/energy"
	gateway "github.com/lucasfth/nocturne/go/internal/runtime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// A deny-all transport catches any accidentally introduced outbound HTTP path.
	http.DefaultTransport = denyTransport{}
	r, err := gateway.New(os.Args[1], gateway.Options{
		FX: func(context.Context) (float64, error) { return 100000, nil },
		Energy: func(context.Context, string) ([]energy.Row, error) {
			return []energy.Row{{Time: "2026-10-15T19:00:00+02:00", Spot: float64(100), Dataset: "DayAheadPrices"}}, nil
		},
	})
	if err != nil {
		panic(err)
	}
	defer r.Close()
	fixed := time.Date(2026, 10, 15, 17, 0, 0, 0, time.UTC)
	r.Energy.Clock = func() time.Time { return fixed }
	r.FX.Clock = func() float64 { return float64(fixed.Unix()) }
	r.App.Now = func() time.Time { return fixed }
	r.Energy.Refresh(context.Background())
	_, err = r.FX.Refresh(context.Background(), 600)
	if err != nil {
		panic(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	handlers := []http.Handler{r.Servers.Public.Handler, r.Servers.Worker.Handler, r.Servers.Admin.Handler}
	addresses := []string{}
	servers := []*http.Server{}
	for _, h := range handlers {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			panic(e)
		}
		addresses = append(addresses, "http://"+l.Addr().String())
		s := &http.Server{Handler: h}
		servers = append(servers, s)
		go s.Serve(l)
	}
	go r.Workers.Run(ctx)
	b, _ := json.Marshal(addresses)
	fmt.Println(string(b))
	<-ctx.Done()
	for _, s := range servers {
		s.Close()
	}
}

type denyTransport struct{}

func (denyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("shadow forbids outbound HTTP")
}
