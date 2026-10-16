package app

import (
	"github.com/lucasfth/nocturne/go/internal/billing"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
	"net/http"
	"strings"
)

func (a *App) auth(w http.ResponseWriter, r *http.Request) (string, *billing.Key) {
	token := ""
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		token = r.Header.Get("Authorization")[7:]
	}
	k, ok, err := a.Bill.KeyForDigest(digest(token))
	if err != nil {
		reject(w, 500, "Internal Server Error")
		return "", nil
	}
	if !ok {
		reject(w, 401, "invalid API key")
		return "", nil
	}
	key, err := a.Bill.Key(k)
	if err != nil || key == nil {
		reject(w, 500, "Internal Server Error")
		return "", nil
	}
	if r.URL.Path != "/v1/chat/completions" {
		a.mu.Lock()
		ok = a.rate("api:"+k, key.RateLimitPerMinute, false)
		a.mu.Unlock()
		if !ok {
			reject(w, 429, "API key rate limit")
			return "", nil
		}
	}
	return k, key
}

// rate requires mu. A failed admission does not consume the chat rate budget.
func (a *App) rate(k string, limit int64, checkOnly bool) bool {
	now := a.Now()
	v := a.rates[k]
	i := 0
	for i < len(v) && now.Sub(v[i]).Seconds() >= 60 {
		i++
	}
	v = v[i:]
	a.rates[k] = v
	if int64(len(v)) >= limit {
		return false
	}
	if !checkOnly {
		a.rates[k] = append(v, now)
	}
	return true
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowed(r.Method, r.URL.Path) {
		reject(w, 404, "not found")
		return
	}
	switch r.URL.Path {
	case "/", "/robots.txt", "/llms.txt", "/openapi.json":
		a.static(w, r)
	case "/v1/keys":
		a.signup(w, r)
	case "/healthz":
		s, _ := a.Energy.State(a.Coordinator.Workers())
		reply(w, 200, object("status", "ok", "model", a.Config.Model, "eligible_workers", s.EligibleWorkers))
	case "/v1/status":
		a.Energy.Refresh(r.Context())
		s, _ := a.Energy.State(a.Coordinator.Workers())
		reply(w, 200, s.Object())
	case "/v1/pricing":
		reply(w, 200, a.pricing())
	case "/v1/models":
		if _, key := a.auth(w, r); key != nil {
			reply(w, 200, object("object", "list", "data", []any{object("id", a.Config.Model, "object", "model", "owned_by", a.serviceName())}))
		}
	case "/v1/chat/completions":
		a.chat(w, r)
	case "/v1/credits/balance":
		if k, key := a.auth(w, r); key != nil {
			reply(w, 200, object("key_id", k, "balance_msat", key.CreditMsat))
		}
	case "/v1/credits/invoice":
		a.createInvoice(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/v1/credits/invoice/") {
			a.getInvoice(w, r)
		} else {
			reject(w, 404, "not found")
		}
	}
}
func allowed(method, path string) bool {
	if method == "POST" {
		return path == "/v1/keys" || path == "/v1/chat/completions" || path == "/v1/credits/invoice"
	}
	if method != "GET" {
		return false
	}
	switch path {
	case "/", "/robots.txt", "/llms.txt", "/openapi.json", "/healthz", "/v1/status", "/v1/pricing", "/v1/models", "/v1/credits/balance":
		return true
	}
	return strings.HasPrefix(path, "/v1/credits/invoice/") && !strings.Contains(strings.TrimPrefix(path, "/v1/credits/invoice/"), "/")
}
func (a *App) pricing() pyjson.Object {
	r := a.Rate()
	input, output, minimum := a.Bill.Prices()
	return object("input_msat_per_1k_tokens", input, "output_msat_per_1k_tokens", output, "min_charge_msat_per_request", minimum, "input_approx_dkk_per_million_tokens", pyjson.RoundHalfEven(input/1e8*r, 3), "output_approx_dkk_per_million_tokens", pyjson.RoundHalfEven(output/1e8*r, 3), "dkk_per_btc", r, "thinking", "reasoning_effort none|low|medium|high (default ~512 thinking tokens); thinking tokens bill as output")
}
