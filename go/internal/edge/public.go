package edge

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var invoicePath = regexp.MustCompile(`^/v1/credits/invoice/[a-zA-Z0-9_-]{1,128}$`)

func Allowed(method, path string) bool {
	if method == "POST" {
		return path == "/v1/keys" || path == "/v1/credits/invoice" || path == "/v1/chat/completions"
	}
	if method != "GET" {
		return false
	}
	switch path {
	case "/", "/robots.txt", "/llms.txt", "/openapi.json", "/healthz", "/v1/status", "/v1/models", "/v1/pricing", "/v1/credits/balance":
		return true
	}
	return invoicePath.MatchString(path)
}

type Public struct {
	next   http.Handler
	config Config
	mu     sync.Mutex
	rates  map[string][]time.Time
}

func NewPublic(next http.Handler, config Config) *Public {
	return &Public{next: next, config: config.defaults(), rates: map[string][]time.Time{}}
}
func (p *Public) rate(ip, path string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.config.Now()
	if len(p.rates) > 4096 {
		for key, v := range p.rates {
			if len(v) == 0 || now.Sub(v[len(v)-1]) >= time.Minute {
				delete(p.rates, key)
			}
		}
		if _, exists := p.rates[ip]; !exists && len(p.rates) > 4096 {
			return "rate limiter capacity"
		}
	}
	rate := p.rates[ip]
	i := 0
	for i < len(rate) && now.Sub(rate[i]) >= time.Minute {
		i++
	}
	rate = rate[i:]
	limit := p.config.PublicRate
	if path == "/v1/chat/completions" {
		limit = p.config.ChatRate
	}
	if len(rate) >= limit {
		p.rates[ip] = rate
		return "IP rate limit"
	}
	p.rates[ip] = append(rate, now)
	return ""
}
func (p *Public) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secure := &secureWriter{ResponseWriter: w, landing: r.URL.Path == "/"}
	defer secure.finish()
	w = secure
	if !Allowed(r.Method, r.URL.Path) {
		reject(w, 404, "not found")
		return
	}
	if detail := p.rate(ClientIP(r), r.URL.Path); detail != "" {
		reject(w, 429, detail)
		return
	}
	maximum := p.config.MaxBodyBytes
	if values, ok := r.Header["Content-Length"]; ok {
		if len(values) != 1 {
			reject(w, 400, "invalid content length")
			return
		}
		n, err := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
		if err != nil || n < 0 {
			reject(w, 400, "invalid content length")
			return
		}
		if n > maximum {
			reject(w, 413, "body too large")
			return
		}
	} else if r.ContentLength > maximum {
		reject(w, 413, "body too large")
		return
	}
	body, status, detail := p.readBody(w, r)
	if status != 0 {
		reject(w, status, detail)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	p.next.ServeHTTP(w, r)
}
