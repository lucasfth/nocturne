// Package app is the public application boundary, not a runnable service.
// Real worker lifecycle is supplied by WorkerCoordinator; Lightning adapters and
// startup/admin wiring remain integration requirements.
package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/billing"
	"github.com/lucasfth/nocturne/go/internal/edge"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"
)

type Config struct {
	Model, ServiceName              string
	RequestTimeout                  time.Duration
	SignupInvite                    string
	OpenSignup                      bool
	SignupPerIPPerDay, TrialsPerDay int
	TrialCreditMsat                 int64
	PublicBaseURL                   *string
	// Assets is a read-only FS rooted at gateway, supplied by integration.
	Assets                               fs.FS
	MaxTokens, MaxInputChars, MaxContext int64
	QueueDepth, BulkQueueDepth           int
	BulkKeys                             []string
	DefaultThinkingBudget                *int64
	InvoiceMin, InvoiceMax               int64
}

// Coordinator owns scheduling, leases, retry-once and worker interruption.
// Submit returns promptly; events end in Final or Error. Cancel is idempotent.
// Workers returns an independent snapshot safe for energy/policy reads.
// Requeue must reset all output before emitting anything for nonstream jobs;
// streams never retry. Adapter validation rejects invalid chunks/usage before
// emitting events (worker HTTP 400), and guarantees bounded event buffering.
// Pending and Submit are serialized by App for public admission.
type Coordinator interface {
	Workers() map[string]energy.Worker
	Pending(bulk bool) int
	Submit(context.Context, Job) (<-chan Event, error)
	Cancel(requestID string)
}
type Job struct {
	ID, Key      string
	Body         map[string]any
	Bulk, Stream bool
	// Finish performs terminal accounting before worker acknowledgement. It must
	// not call the coordinator or acquire App.mu (coordinator lock is held).
	Finish func(raw int64, completion *int64, cancel bool) error
}
type Event struct {
	Text, Reasoning string
	Final           bool
	// Nil means estimate raw characters / 4. Prompt usage is gateway-owned.
	CompletionTokens *int64
	Error, WorkerID  string
}
type Lightning interface {
	Name() string
	CreateInvoice(context.Context, int64, string) (billing.Invoice, error)
	CheckInvoice(context.Context, string) (string, error)
}
type App struct {
	// Config is immutable after construction; live settings use the synchronized store.
	Config      Config
	Settings    *Settings
	Bill        *billing.Billing
	Energy      *energy.Energy
	Coordinator Coordinator
	Lightning   Lightning
	Rate        func() float64
	Now         func() time.Time
	Logf        func(string, ...any)
	mu          sync.Mutex
	invoiceMu   sync.Mutex
	rates       map[string][]time.Time
	active      map[string]int64
	signups     map[string][]time.Time
	trials      map[string]int
}

func New(c Config, b *billing.Billing, e *energy.Energy, w Coordinator, l Lightning, rate func() float64) *App {
	if c.SignupPerIPPerDay == 0 {
		c.SignupPerIPPerDay = 3
	}
	if c.TrialsPerDay == 0 {
		c.TrialsPerDay = 100
	}
	if c.ServiceName == "" {
		c.ServiceName = "inference"
	}
	if c.BulkQueueDepth == 0 {
		c.BulkQueueDepth = 16
	}
	if c.InvoiceMin == 0 {
		c.InvoiceMin = 1000
	}
	if c.InvoiceMax == 0 {
		c.InvoiceMax = 100000000
	}
	return &App{Config: c, Bill: b, Energy: e, Coordinator: w, Lightning: l, Rate: rate, Now: time.Now, Logf: log.Printf, rates: map[string][]time.Time{}, active: map[string]int64{}, signups: map[string][]time.Time{}, trials: map[string]int{}}
}
func (a *App) Public(c edge.Config) http.Handler { return edge.NewPublic(a, c) }
func object(kv ...any) pyjson.Object {
	o := pyjson.Object{}
	for i := 0; i < len(kv); i += 2 {
		o = append(o, pyjson.Pair{Key: kv[i].(string), Val: kv[i+1]})
	}
	return o
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, pyjson.MustMarshal(v))
}
func reject(w http.ResponseWriter, status int, v any) { reply(w, status, object("detail", v)) }
func digest(s string) string                          { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
