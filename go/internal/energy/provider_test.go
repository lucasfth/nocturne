package energy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicPricesFallback(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		if strings.HasSuffix(r.URL.Path, "DayAheadPrices") {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"records":[{"HourDK":"2026-10-10T21:00:00","SpotPriceDKK":512.5},{"HourDK":"2026-10-10T20:00:00","SpotPriceDKK":null},{"HourDK":"2026-10-10T19:00:00","SpotPriceDKK":400}]}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL + "/dataset/"
	defer func() { BaseURL = old }()
	rows, err := PublicPrices(NewClient())(context.Background(), "DK2")
	if err != nil || len(rows) != 2 || rows[0].Spot != 512.5 || rows[1].Spot != int64(400) || rows[0].Dataset != "Elspotprices" {
		t.Fatalf("%v %+v", err, rows)
	}
	if len(paths) != 2 || !strings.Contains(paths[1], "sort=HourDK+DESC") || !strings.Contains(paths[0], "limit=400") ||
		!strings.Contains(paths[0], "filter=%7B%22PriceArea%22%3A%5B%22DK2%22%5D%7D") {
		t.Fatal(paths)
	}
	if NewClient().Transport.(*http.Transport).Proxy != nil {
		t.Fatal("proxy must be nil")
	}
}

func TestPublicPricesUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"records":[{"Other":1}]}`))
	}))
	defer srv.Close()
	old := BaseURL
	BaseURL = srv.URL + "/"
	defer func() { BaseURL = old }()
	if _, err := PublicPrices(NewClient())(context.Background(), "DK1"); err != ErrUnavailable {
		t.Fatal(err)
	}
}
