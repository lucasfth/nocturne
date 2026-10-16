// Package lightning supplies invoice-only adapters. Construction never contacts a wallet.
package lightning

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lucasfth/nocturne/go/internal/app"
	"github.com/lucasfth/nocturne/go/internal/billing"
)

type Config struct {
	Backend, BaseURL, InvoiceKeyFile, MacaroonFile, TLSCertPath, ConfFile string
	ExpiryS, MinMsat, MaxMsat                                             int64
}

// Backend must be explicitly selected, including fake for tests. No silent fallback.
func New(c Config, bill *billing.Billing) (app.Lightning, error) {
	if c.ExpiryS == 0 {
		c.ExpiryS = 3600
	}
	if c.MinMsat == 0 {
		c.MinMsat = 1000
	}
	if c.MaxMsat == 0 {
		c.MaxMsat = 100000000
	}
	if c.ExpiryS < 0 || c.MinMsat < 0 || c.MaxMsat < c.MinMsat || bill == nil {
		return nil, errors.New("invalid Lightning configuration")
	}
	switch c.Backend {
	case "fake":
		return &Fake{c: c, bill: bill}, nil
	case "lnbits", "lnd":
		return newRemote(c)
	case "phoenixd":
		return newPhoenix(c, bill)
	default:
		return nil, errors.New("explicit Lightning backend required")
	}
}

var errBackend = errors.New("invoice backend unavailable")

func secret(path, message string) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New(message)
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Mode().Perm() != 0600 || s.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, errors.New(message)
	}
	b, e := io.ReadAll(io.LimitReader(f, 1<<20))
	if e != nil || len(b) == 1<<20 {
		return nil, errors.New(message)
	}
	return b, nil
}
func client(cert string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cert != "" {
		b, e := secret(cert, "LND TLS certificate must have mode 0600 and contain valid PEM")
		if e != nil {
			return nil, e
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid LND TLS certificate")
		}
		tr.TLSClientConfig.RootCAs = roots
	}
	return &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func id() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", errBackend
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:]), nil
}
func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }
func endpoint(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("Lightning backend requires HTTPS")
	}
	return strings.TrimRight(raw, "/"), nil
}

// do deliberately discards remote error bodies and URL-bearing transport errors.
func do(ctx context.Context, c *http.Client, method, uri string, body io.Reader, headers http.Header, out any) (int, error) {
	r, e := http.NewRequestWithContext(ctx, method, uri, body)
	if e != nil {
		return 0, errBackend
	}
	r.Header = headers.Clone()
	resp, e := c.Do(r)
	if e != nil {
		return 0, errBackend
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, errBackend
	}
	if resp.StatusCode == 204 {
		return 204, nil
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); e != nil {
		return resp.StatusCode, errBackend
	}
	return resp.StatusCode, nil
}

var _ app.Lightning = (*Fake)(nil)

type Fake struct {
	c    Config
	bill *billing.Billing
}

func (*Fake) Name() string { return "fake" }
func (f *Fake) CreateInvoice(_ context.Context, _ int64, _ string) (billing.Invoice, error) {
	i, e := id()
	return billing.Invoice{InvoiceID: i, PaymentHash: i, Bolt11: "fake-not-payable-" + i, ExpiresAt: now() + float64(f.c.ExpiryS)}, e
}
func (f *Fake) CheckInvoice(_ context.Context, i string) (string, error) {
	r, e := f.bill.GetInvoice(i)
	if e != nil {
		return "", e
	}
	if r == nil || r["expires_at"].(float64) <= now() {
		return "expired", nil
	}
	return r["state"].(string), nil
}
func (f *Fake) MarkPaid(_ context.Context, i string) error { return f.bill.InvoiceState(i, "paid") }
