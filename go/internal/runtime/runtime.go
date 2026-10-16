package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lucasfth/nocturne/go/internal/app"
	"github.com/lucasfth/nocturne/go/internal/benchmark"
	"github.com/lucasfth/nocturne/go/internal/billing"
	"github.com/lucasfth/nocturne/go/internal/edge"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"github.com/lucasfth/nocturne/go/internal/fx"
	"github.com/lucasfth/nocturne/go/internal/lightning"
	"github.com/lucasfth/nocturne/go/internal/machines"
)

type Options struct {
	Energy  energy.Provider
	FX      fx.Provider
	Payment func(lightning.Config, *billing.Billing) (app.Lightning, error)
	// Listen is injectable for ephemeral loopback tests; production always binds fixed loopback ports.
	Listen          func(string, string) (net.Listener, error)
	ShutdownTimeout time.Duration
}
type Runtime struct {
	App     *app.App
	Workers *app.WorkerCoordinator
	Bill    *billing.Billing
	FX      *fx.Fx
	Energy  *energy.Energy
	Servers edge.Servers
	options Options
	once    sync.Once
}

func New(root string, o Options) (r *Runtime, err error) {
	if root == "" {
		root = "."
	}
	c, e := Load(root)
	if e != nil {
		return nil, e
	}
	data := filepath.Join(root, "data")
	p := filepath.Join(data, "admin-secret.sha256")
	st, e := os.Stat(p)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return nil, errors.New("admin digest must be a private mode 0600 file")
	}
	b, e := os.ReadFile(p)
	if e != nil || len(b) > 256 {
		return nil, errors.New("invalid admin digest")
	}
	hash := strings.TrimSpace(string(b))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
		return nil, errors.New("invalid admin digest")
	}
	bill, e := billing.Open(data)
	if e != nil {
		return nil, errors.New("billing startup failed")
	}
	defer func() {
		if err != nil {
			bill.Close()
		}
	}()
	f := fx.New(c.DKK, data, o.FX, nil)
	bill.Reprice(f.Rate())
	bill.DefaultMaxConcurrent = c.PerKey
	en := energy.New(&energy.Config{Schedule: c.Schedule, Energy: c.Energy, DKKPerBTC: c.DKK}, o.Energy, nil)
	en.SetLiveRate(f.Rate())
	legacy := machines.Legacy{Meta: c.Macs}
	if _, e = os.Stat(filepath.Join(data, "machines.json")); errors.Is(e, os.ErrNotExist) {
		raw, re := os.ReadFile(filepath.Join(data, "worker_secrets.json"))
		if re == nil {
			if json.Unmarshal(raw, &legacy.Secrets) != nil {
				return nil, errors.New("invalid legacy worker registry")
			}
		} else if !errors.Is(re, os.ErrNotExist) {
			return nil, errors.New("legacy worker registry unavailable")
		}
		auth := os.Getenv("NOCTURNE_AUTHORIZED_KEYS")
		if auth == "" {
			home, _ := os.UserHomeDir()
			auth = filepath.Join(home, ".ssh", "authorized_keys")
		}
		legacy.Keys = machines.LegacyTunnelKeys(auth)
	}
	m, e := machines.Open(data, legacy)
	if e != nil {
		return nil, errors.New("machine registry startup failed")
	}
	payment := o.Payment
	if payment == nil {
		payment = lightning.New
	}
	l, e := payment(c.lightningConfig(), bill)
	if e != nil || l == nil {
		return nil, errors.New("Lightning startup failed")
	}
	w := app.NewWorkerCoordinator(app.WorkerConfig{Model: c.Model, RequestTimeout: seconds(c.RequestTimeout), BulkSlotsPerWorker: c.BulkSlots}, m, en)
	w.Benchmarks, e = benchmark.Open(data)
	if e != nil {
		return nil, errors.New("benchmark startup failed")
	}
	defer func() {
		if err != nil {
			w.Benchmarks.Close()
		}
	}()
	w.BenchmarkTargets, e = loadBenchmarkTargets(root, m)
	if e != nil {
		return nil, e
	}
	ac := c.appConfig()
	ac.Assets = os.DirFS(filepath.Join(root, "gateway"))
	a := app.New(ac, bill, en, w, l, f.Rate)
	a.Settings, e = app.OpenSettings(filepath.Join(root, "config.json"), c.ServiceName, c.Concurrency)
	if e != nil {
		return nil, errors.New("configuration settings startup failed")
	}
	servers, e := edge.NewServers(edge.Handlers{Public: a, Worker: w.Worker(), Admin: a.Admin(app.AdminConfig{SecretSHA256: hash, TailnetLogins: c.Tailnet, Coordinator: w, Fx: f, Dist: os.DirFS(filepath.Join(root, "web", "dist")), InternalKeys: c.InternalKeys})}, c.edgeConfig(), c.Tailnet)
	if e != nil {
		return nil, e
	}
	return &Runtime{App: a, Workers: w, Bill: bill, FX: f, Energy: en, Servers: servers, options: o}, nil
}
func (r *Runtime) Close() { r.once.Do(func() { r.Workers.Benchmarks.Close(); r.Bill.Close() }) }

// Serve owns every listener, request context and maintenance task. It returns only
// after shutdown and cleanup. A Runtime is single-use.
func (r *Runtime) Serve(parent context.Context) error {
	defer r.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	servers := []*http.Server{r.Servers.Public, r.Servers.Worker, r.Servers.Admin}
	listen := r.options.Listen
	if listen == nil {
		listen = net.Listen
	}
	listeners := []net.Listener{}
	for _, s := range servers {
		l, e := listen("tcp", s.Addr)
		if e != nil {
			for _, l := range listeners {
				l.Close()
			}
			return errors.New("gateway loopback listener unavailable")
		}
		listeners = append(listeners, l)
		s.BaseContext = func(net.Listener) context.Context { return ctx }
	}
	// Track handlers separately: forced Close unblocks sockets but does not wait
	// for handler settlement. SQLite must outlive request cancellation cleanup.
	var requests sync.WaitGroup
	for _, s := range servers {
		next := s.Handler
		s.Handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			requests.Add(1)
			defer requests.Done()
			next.ServeHTTP(w, q)
		})
	}
	var tasks sync.WaitGroup
	launch := func(f func()) { tasks.Add(1); go func() { defer tasks.Done(); f() }() }
	launch(func() { r.App.RunInvoiceMaintenance(ctx) })
	launch(func() { r.Workers.Run(ctx) })
	launch(func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			r.Energy.Refresh(ctx)
			changed, e := r.FX.Refresh(ctx, 600)
			if changed {
				r.Bill.Reprice(r.FX.Rate())
				r.Energy.SetLiveRate(r.FX.Rate())
			}
			if e != nil {
				r.App.Logf("fx_cache_write_failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
	result := make(chan error, 3)
	for i, s := range servers {
		i, s := i, s
		launch(func() { result <- s.Serve(listeners[i]) })
	}
	var failure error
	select {
	case <-parent.Done():
	case e := <-result:
		if !errors.Is(e, http.ErrServerClosed) {
			failure = errors.New("gateway listener failed")
		}
	}
	cancel()
	timeout := r.options.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	shutdown, done := context.WithTimeout(context.Background(), timeout)
	defer done()
	var drains sync.WaitGroup
	for _, s := range servers {
		s := s
		drains.Add(1)
		go func() {
			defer drains.Done()
			if s.Shutdown(shutdown) != nil {
				s.Close()
			}
		}()
	}
	drains.Wait()
	tasks.Wait()
	requests.Wait()
	return failure
}
