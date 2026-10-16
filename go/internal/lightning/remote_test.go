package lightning

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func remoteFixture(t *testing.T, backend string, h http.HandlerFunc) *remote {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	p := writeSecret(t, "fixture.crt", string(cert))
	key := writeSecret(t, "fixture.credential", "fixture-invoice-only")
	b, _ := ledger(t)
	v, e := New(Config{Backend: backend, BaseURL: s.URL, InvoiceKeyFile: key, MacaroonFile: key, TLSCertPath: p}, b)
	if e != nil {
		t.Fatal(e)
	}
	r := v.(*remote)
	if backend == "lnbits" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(cert)
		r.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = pool
	}
	return r
}
func TestRemoteCreateAndStates(t *testing.T) {
	for _, backend := range []string{"lnbits", "lnd"} {
		t.Run(backend, func(t *testing.T) {
			state := "pending"
			r := remoteFixture(t, backend, func(w http.ResponseWriter, r *http.Request) {
				header := "X-Api-Key"
				create, lookup := "/api/v1/payments", "/api/v1/payments/"+testHash
				if backend == "lnd" {
					header = "Grpc-Metadata-macaroon"
					create, lookup = "/v1/invoices", "/v1/invoice/"+testHash
				}
				if r.Header.Get(header) != "fixture-invoice-only" {
					t.Error("credential missing")
				}
				if r.Method == "POST" {
					if r.URL.Path != create {
						t.Error(r.URL.Path)
					}
					var d map[string]any
					json.NewDecoder(r.Body).Decode(&d)
					if backend == "lnbits" {
						if d["out"] != false || d["amount"] != float64(10) || d["expiry"] != float64(3600) {
							t.Error(d)
						}
					} else {
						if d["value_msat"] != "10000" || d["expiry"] != "3600" {
							t.Error(d)
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"payment_hash": testHash, "r_hash": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xab", 32))), "payment_request": "lnbc-test"})
					return
				}
				if r.URL.Path != lookup {
					t.Error(r.URL.Path)
				}
				json.NewEncoder(w).Encode(map[string]any{"paid": state == "paid", "settled": false, "state": map[string]string{"pending": "OPEN", "paid": "SETTLED", "expired": "CANCELED"}[state]})
			})
			inv, e := r.CreateInvoice(context.Background(), 10000, "memo")
			if e != nil || inv.InvoiceID != testHash || inv.PaymentHash != testHash || inv.Bolt11 != "lnbc-test" {
				t.Fatal(inv, e)
			}
			for _, s := range []string{"pending", "paid", "expired"} {
				state = s
				got, e := r.CheckInvoice(context.Background(), testHash)
				want := s
				if backend == "lnbits" && s == "expired" {
					want = "pending"
				}
				if got != want || e != nil {
					t.Fatal(got, e)
				}
			}
			if backend == "lnbits" {
				if _, e = r.CreateInvoice(context.Background(), 1001, ""); e == nil {
					t.Fatal("fractional satoshi")
				}
			}
		})
	}
}
func TestRemoteFailuresAndSecurity(t *testing.T) {
	for _, status := range []int{200, 401, 500, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			r := remoteFixture(t, "lnd", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://credentials.invalid")
				w.WriteHeader(status)
				w.Write([]byte("secret remote detail"))
			})
			_, e := r.CreateInvoice(context.Background(), 10000, "")
			if e == nil || strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), r.base) {
				t.Fatal(e)
			}
		})
	}
	r := remoteFixture(t, "lnd", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := r.CheckInvoice(ctx, testHash); e != errBackend {
		t.Fatal(e)
	}
	r = remoteFixture(t, "lnbits", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
		}
	})
	r.http.Timeout = 5 * time.Millisecond
	if _, e := r.CheckInvoice(context.Background(), testHash); e != errBackend {
		t.Fatal(e)
	}
	r = remoteFixture(t, "lnd", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"state":"OPEN"}`)) })
	r.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = x509.NewCertPool()
	if _, e := r.CheckInvoice(context.Background(), testHash); e != errBackend {
		t.Fatal("untrusted TLS accepted", e)
	}
}
func TestConfigProtectionAndProxyDisabled(t *testing.T) {
	b, _ := ledger(t)
	if _, e := New(Config{}, b); e == nil {
		t.Fatal("implicit fake")
	}
	key := writeSecret(t, "fixture.credential", "fixture-key")
	for _, backend := range []string{"lnbits", "lnd"} {
		for _, base := range []string{"http://127.0.0.1", "https://user:pass@example.invalid", "https://example.invalid?q=credential"} {
			if _, e := New(Config{Backend: backend, BaseURL: base, InvoiceKeyFile: key, MacaroonFile: key}, b); e == nil {
				t.Fatal(base)
			}
		}
	}
	os.Chmod(key, 0644)
	if _, e := New(Config{Backend: "lnbits", BaseURL: "https://example.invalid", InvoiceKeyFile: key}, b); e == nil {
		t.Fatal("key permissions")
	}
	os.Chmod(key, 0600)
	cert := writeSecret(t, "fixture.crt", "invalid pem")
	if _, e := New(Config{Backend: "lnd", BaseURL: "https://example.invalid", MacaroonFile: key, TLSCertPath: cert}, b); e == nil {
		t.Fatal("invalid TLS cert")
	}
	os.Chmod(cert, 0644)
	if _, e := New(Config{Backend: "lnd", BaseURL: "https://example.invalid", MacaroonFile: key, TLSCertPath: cert}, b); e == nil {
		t.Fatal("TLS permissions")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	cl, e := client("")
	if e != nil || cl.Timeout != 15*time.Second || cl.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("unsafe transport", e)
	}
	for _, backend := range []string{"lnbits", "lnd"} {
		r := remoteFixture(t, backend, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"paid":false}`)) })
		if r.http.Transport.(*http.Transport).Proxy != nil {
			t.Fatal("proxy enabled")
		}
		if _, e := r.CheckInvoice(context.Background(), testHash); e != nil {
			t.Fatal(e)
		}
	}
}
func TestFakeExplicitPaymentAndExpiry(t *testing.T) {
	b, k := ledger(t)
	v, e := New(Config{Backend: "fake"}, b)
	if e != nil {
		t.Fatal(e)
	}
	f := v.(*Fake)
	inv, e := f.CreateInvoice(context.Background(), 10000, "")
	if e != nil || !strings.HasPrefix(inv.Bolt11, "fake-not-payable-") {
		t.Fatal(inv, e)
	}
	if e = b.AddInvoice(k, 10000, inv, "fake"); e != nil {
		t.Fatal(e)
	}
	if s, _ := f.CheckInvoice(context.Background(), inv.InvoiceID); s != "pending" {
		t.Fatal(s)
	}
	for i := 0; i < 2; i++ {
		if e = f.MarkPaid(context.Background(), inv.InvoiceID); e != nil {
			t.Fatal(e)
		}
	}
	key, _ := b.Key(k)
	if key.CreditMsat != 10000 {
		t.Fatal(key)
	}
	if s, _ := f.CheckInvoice(context.Background(), "missing"); s != "expired" {
		t.Fatal(s)
	}
	inv.InvoiceID = "expired"
	inv.ExpiresAt = 0
	b.AddInvoice(k, 10000, inv, "fake")
	f.MarkPaid(context.Background(), inv.InvoiceID)
	if s, _ := f.CheckInvoice(context.Background(), inv.InvoiceID); s != "expired" {
		t.Fatal(s)
	}
}
