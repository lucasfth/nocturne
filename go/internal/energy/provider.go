package energy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Row is one price record: Python dict(time, spot_dkk_mwh, dataset).
// Spot holds int64 or float64 (JSON number as decoded by Python), or a string.
type Row struct {
	Time    string
	Spot    any
	Dataset string
}

// Provider fetches price rows for a price area (injectable for tests).
type Provider func(ctx context.Context, area string) ([]Row, error)

// ErrUnavailable mirrors RuntimeError('price provider unavailable').
var ErrUnavailable = errors.New("price provider unavailable")

// NewClient returns a 15 s client that ignores proxy env vars (trust_env=False).
func NewClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
}

// BaseURL is the Energi Data Service dataset endpoint (overridable in tests).
var BaseURL = "https://api.energidataservice.dk/dataset/"

// PublicPrices returns a Provider backed by Energi Data Service:
// DayAheadPrices first, then the Elspotprices fallback.
func PublicPrices(client *http.Client) Provider {
	return func(ctx context.Context, area string) ([]Row, error) {
		for _, d := range [][3]string{{"DayAheadPrices", "TimeDK", "DayAheadPriceDKK"}, {"Elspotprices", "HourDK", "SpotPriceDKK"}} {
			if rows, ok := fetch(ctx, client, area, d[0], d[1], d[2]); ok {
				return rows, nil
			}
		}
		return nil, ErrUnavailable
	}
}

func fetch(ctx context.Context, client *http.Client, area, dataset, stamp, price string) ([]Row, bool) {
	q := "filter=" + url.QueryEscape(`{"PriceArea":["`+area+`"]}`) + "&sort=" + url.QueryEscape(stamp+" DESC") + "&limit=400"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+dataset+"?"+q, nil)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	var body struct {
		Records []map[string]json.RawMessage `json:"records"`
	}
	dec := json.NewDecoder(resp.Body)
	if dec.Decode(&body) != nil || body.Records == nil {
		return nil, false // ValueError / KeyError
	}
	recs := body.Records
	if len(recs) == 0 || recs[0][stamp] == nil || recs[0][price] == nil {
		return nil, false
	}
	out := []Row{}
	for _, r := range recs {
		p, okP := r[price]
		s, okS := r[stamp]
		if !okP {
			return nil, false // KeyError
		}
		if bytes.Equal(p, []byte("null")) {
			continue
		}
		var ts string
		if !okS || json.Unmarshal(s, &ts) != nil {
			return nil, false
		}
		out = append(out, Row{Time: ts, Spot: number(p), Dataset: dataset})
	}
	return out, true
}

// number decodes a JSON scalar the way Python's json does (int vs float).
func number(raw json.RawMessage) any {
	s := strings.TrimSpace(string(raw))
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}
