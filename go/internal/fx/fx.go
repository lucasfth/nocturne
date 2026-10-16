// Package fx is a port of gateway/fx.py: live BTC->DKK rate with an on-disk
// cache (data/fx.json) and a fallback to the config constant.
package fx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Provider returns a BTC-DKK spot rate (injectable for tests).
type Provider func(ctx context.Context) (float64, error)

// Repricer matches (*billing.Billing).Reprice, e.g. fx.RefreshAndReprice(ctx, 600, bill.Reprice).
type Repricer func(dkkPerBTC float64)

// CoinbaseURL is the spot endpoint (overridable in tests).
var CoinbaseURL = "https://api.coinbase.com/v2/prices/BTC-DKK/spot"

// NewClient returns a 10 s client that ignores proxy env vars (trust_env=False).
func NewClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
}

// Coinbase returns a Provider for float(r.json()['data']['amount']).
func Coinbase(client *http.Client) Provider {
	return func(ctx context.Context) (float64, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, CoinbaseURL, nil)
		if err != nil {
			return 0, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return 0, fmt.Errorf("coinbase: HTTP %d", resp.StatusCode)
		}
		var body struct {
			Data struct {
				Amount json.RawMessage `json:"amount"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return 0, err
		}
		return pyFloat(body.Data.Amount)
	}
}

// pyFloat mirrors float(x) for a JSON string or number.
func pyFloat(raw json.RawMessage) (float64, error) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	if s == "" || s == "null" {
		return 0, errors.New("missing amount")
	}
	return strconv.ParseFloat(s, 64)
}

// Fx ports the Python Fx class. Clock returns Unix seconds (time.time()).
type Fx struct {
	Path     string
	Provider Provider
	Clock    func() float64

	mu          sync.Mutex
	rate        any // float64, or int64 when loaded from a cache holding an int
	updated     any // nil, int64 or float64
	source      string
	lastAttempt *float64
}

func num(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}
