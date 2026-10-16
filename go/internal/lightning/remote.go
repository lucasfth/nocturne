package lightning

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/lucasfth/nocturne/go/internal/billing"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type remote struct {
	c       Config
	base    string
	headers http.Header
	http    *http.Client
}

func newRemote(c Config) (*remote, error) {
	base, e := endpoint(c.BaseURL)
	if e != nil {
		return nil, e
	}
	file, header, cert := c.InvoiceKeyFile, "X-Api-Key", ""
	if c.Backend == "lnd" {
		file, header, cert = c.MacaroonFile, "Grpc-Metadata-macaroon", c.TLSCertPath
		if cert == "" {
			return nil, errors.New("LND TLS certificate required")
		}
	}
	key, e := secret(file, "Lightning secret file must have mode 0600")
	if e != nil {
		return nil, e
	}
	value := strings.TrimSpace(string(key))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return nil, errors.New("invalid Lightning credential")
	}
	h := http.Header{}
	h.Set(header, value)
	h.Set("Content-Type", "application/json")
	client, e := client(cert)
	if e != nil {
		return nil, e
	}
	return &remote{c, base, h, client}, nil
}
func (r *remote) Name() string { return r.c.Backend }
func (r *remote) CreateInvoice(ctx context.Context, amount int64, memo string) (billing.Invoice, error) {
	var payload any
	path := "/api/v1/payments"
	if r.Name() == "lnbits" {
		if amount%1000 != 0 {
			return billing.Invoice{}, errors.New("LNbits invoices require whole satoshis")
		}
		payload = map[string]any{"out": false, "amount": amount / 1000, "memo": memo, "expiry": r.c.ExpiryS}
	} else {
		path = "/v1/invoices"
		payload = map[string]any{"value_msat": strconv.FormatInt(amount, 10), "memo": memo, "expiry": strconv.FormatInt(r.c.ExpiryS, 10)}
	}
	b, _ := json.Marshal(payload)
	var d struct {
		PaymentHash    string `json:"payment_hash"`
		PaymentRequest string `json:"payment_request"`
		RHash          string `json:"r_hash"`
	}
	if _, e := do(ctx, r.http, "POST", r.base+path, bytes.NewReader(b), r.headers, &d); e != nil {
		return billing.Invoice{}, e
	}
	h := d.PaymentHash
	if r.Name() == "lnd" {
		decoded, e := base64.StdEncoding.DecodeString(d.RHash)
		if e != nil || len(decoded) != 32 {
			return billing.Invoice{}, errBackend
		}
		h = hex.EncodeToString(decoded)
	}
	if h == "" || d.PaymentRequest == "" {
		return billing.Invoice{}, errBackend
	}
	return billing.Invoice{InvoiceID: h, PaymentHash: h, Bolt11: d.PaymentRequest, ExpiresAt: now() + float64(r.c.ExpiryS)}, nil
}
func (r *remote) CheckInvoice(ctx context.Context, id string) (string, error) {
	path := "/api/v1/payments/"
	if r.Name() == "lnd" {
		path = "/v1/invoice/"
	}
	var d struct {
		Paid    bool   `json:"paid"`
		Settled bool   `json:"settled"`
		State   string `json:"state"`
	}
	if _, e := do(ctx, r.http, "GET", r.base+path+url.PathEscape(id), nil, r.headers, &d); e != nil {
		return "", e
	}
	if (r.Name() == "lnbits" && d.Paid) || (r.Name() == "lnd" && (d.Settled || d.State == "SETTLED")) {
		return "paid", nil
	}
	if r.Name() == "lnd" && d.State == "CANCELED" {
		return "expired", nil
	}
	return "pending", nil
}
