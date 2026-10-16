package edge

import (
	"fmt"
	"net/http"
	"time"
)

// Handlers must be supplied independently by later port phases. This package
// neither binds sockets nor claims the gateway is ready to serve.
type Handlers struct{ Public, Worker, Admin http.Handler }
type Servers struct{ Public, Worker, Admin *http.Server }

func NewServers(h Handlers, config Config, tailnetLogins []string) (Servers, error) {
	if h.Public == nil || h.Worker == nil || h.Admin == nil {
		return Servers{}, fmt.Errorf("public, worker and admin handlers are required")
	}
	c := config.defaults()
	makeServer := func(address string, handler http.Handler, limit int) *http.Server {
		return &http.Server{Addr: address, Handler: ConcurrencyLimit(handler, limit), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second}
	}
	return Servers{
		Public: makeServer("127.0.0.1:8700", NewPublic(h.Public, c), c.PublicConcurrency),
		Worker: makeServer("127.0.0.1:8701", h.Worker, 256),
		Admin:  makeServer("127.0.0.1:8702", TailnetGuard(h.Admin, tailnetLogins), 256),
	}, nil
}

// ConcurrencyLimit mirrors uvicorn's overload response, not an application
// rate rejection: 503, text/plain, connection close, and no Retry-After.
func ConcurrencyLimit(next http.Handler, limit int) http.Handler {
	if limit < 0 {
		limit = 0
	}
	slots := make(chan struct{}, limit)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Connection", "close")
			w.WriteHeader(503)
			_, _ = w.Write([]byte("Service Unavailable"))
		}
	})
}
