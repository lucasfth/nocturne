package lightning

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/lucasfth/nocturne/go/internal/app"
	"github.com/lucasfth/nocturne/go/internal/billing"
)

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Phoenix struct {
	c         Config
	bill      *billing.Billing
	base      string
	headers   http.Header
	http      *http.Client
	signing   []byte
	webhookMu sync.Mutex
}

func newPhoenix(c Config, b *billing.Billing) (*Phoenix, error) {
	if c.BaseURL == "" {
		c.BaseURL = "http://127.0.0.1:9740"
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base != "http://127.0.0.1:9740" {
		return nil, errors.New("phoenixd must use the private loopback endpoint")
	}
	p := c.ConfFile
	if p == "" {
		p = "~/.phoenix/phoenix.conf"
	}
	if strings.HasPrefix(p, "~/") {
		home, e := os.UserHomeDir()
		if e != nil {
			return nil, errors.New("phoenix.conf unavailable")
		}
		p = filepath.Join(home, p[2:])
	}
	data, e := secret(p, "phoenix.conf must have mode 0600")
	if e != nil {
		return nil, e
	}
	conf := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			conf[k] = v
		}
	}
	password, ok := conf["http-password-limited-access"]
	if !ok {
		password = conf["http-password"]
	}
	if password == "" || conf["webhook-secret"] == "" {
		return nil, errors.New("phoenix.conf missing required credentials")
	}
	req, _ := http.NewRequest("GET", base, nil)
	req.SetBasicAuth("", password)
	cl, e := client("")
	if e != nil {
		return nil, e
	}
	return &Phoenix{c: c, bill: b, base: base, headers: req.Header, http: cl, signing: []byte(conf["webhook-secret"])}, nil
}
func (*Phoenix) Name() string { return "phoenixd" }
func (p *Phoenix) CreateInvoice(ctx context.Context, amount int64, memo string) (billing.Invoice, error) {
	return p.CreateInvoiceWithExpiry(ctx, amount, memo, p.c.ExpiryS)
}
func (p *Phoenix) CreateInvoiceWithExpiry(ctx context.Context, amount int64, memo string, expiry int64) (billing.Invoice, error) {
	if amount%1000 != 0 || amount < p.c.MinMsat || amount > p.c.MaxMsat {
		return billing.Invoice{}, errors.New("invoice amount outside whole-satoshi limits")
	}
	if expiry <= 0 {
		return billing.Invoice{}, errors.New("invalid expiry")
	}
	i, e := id()
	if e != nil {
		return billing.Invoice{}, e
	}
	created := now()
	runes := []rune(memo)
	if len(runes) > 128 {
		memo = string(runes[:128])
	}
	form := url.Values{"amountSat": {strconv.FormatInt(amount/1000, 10)}, "description": {memo}, "externalId": {i}, "expirySeconds": {strconv.FormatInt(expiry, 10)}}
	h := p.headers.Clone()
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	var d struct {
		PaymentHash string `json:"paymentHash"`
		Serialized  string `json:"serialized"`
	}
	if _, e = do(ctx, p.http, "POST", p.base+"/createinvoice", strings.NewReader(form.Encode()), h, &d); e != nil {
		return billing.Invoice{}, e
	}
	if !hashPattern.MatchString(d.PaymentHash) || !strings.HasPrefix(d.Serialized, "lnbc") {
		return billing.Invoice{}, errors.New("invalid mainnet invoice response")
	}
	return billing.Invoice{InvoiceID: i, PaymentHash: d.PaymentHash, Bolt11: d.Serialized, ExpiresAt: created + float64(expiry)}, nil
}
func (p *Phoenix) Wallet(ctx context.Context) (any, error) {
	var b struct {
		Balance int64 `json:"balanceSat"`
		Fee     int64 `json:"feeCreditSat"`
	}
	var i struct {
		NodeID   *string `json:"nodeId"`
		Channels []struct {
			Inbound int64 `json:"inboundLiquiditySat"`
		} `json:"channels"`
	}
	if _, e := do(ctx, p.http, "GET", p.base+"/getbalance", nil, p.headers, &b); e != nil {
		return nil, e
	}
	if _, e := do(ctx, p.http, "GET", p.base+"/getinfo", nil, p.headers, &i); e != nil {
		return nil, e
	}
	var inbound int64
	for _, ch := range i.Channels {
		inbound += ch.Inbound
	}
	return map[string]any{"balance_sat": b.Balance, "fee_credit_sat": b.Fee, "channels": len(i.Channels), "inbound_liquidity_sat": inbound, "node_id": i.NodeID}, nil
}
func (p *Phoenix) CheckInvoice(ctx context.Context, i string) (string, error) {
	r, e := p.bill.GetInvoice(i)
	if e != nil {
		return "", e
	}
	if r == nil || r["backend"] != "phoenixd" {
		return "pending", nil
	}
	return p.checkPaid(ctx, r["payment_hash"].(string), r)
}
func (p *Phoenix) checkPaid(ctx context.Context, hash string, row billing.Row) (string, error) {
	if !hashPattern.MatchString(hash) {
		return "", errors.New("invalid payment hash")
	}
	var d struct {
		PaymentHash  string `json:"paymentHash"`
		ExternalID   string `json:"externalId"`
		RequestedSat *int64 `json:"requestedSat"`
		Paid         bool   `json:"isPaid"`
		Expired      bool   `json:"isExpired"`
	}
	status, e := do(ctx, p.http, "GET", p.base+"/payments/incoming/"+hash, nil, p.headers, &d)
	if e != nil {
		return "", e
	}
	if status == 204 {
		return "pending", nil
	}
	if d.PaymentHash != hash {
		return "", errors.New("payment hash mismatch")
	}
	if row != nil && (d.ExternalID != row["invoice_id"] || d.RequestedSat == nil || *d.RequestedSat != row["amount_msat"].(int64)/1000) {
		return "", errors.New("invoice metadata mismatch")
	}
	if d.Paid {
		return "paid", nil
	}
	if d.Expired {
		return "expired", nil
	}
	return "pending", nil
}
func (p *Phoenix) Webhook(ctx context.Context, body []byte, signature string) (bool, error) {
	mac := hmac.New(sha256.New, p.signing)
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return false, app.ErrWebhookSignature
	}
	var event map[string]json.RawMessage
	if json.Unmarshal(body, &event) != nil || event == nil {
		return false, app.ErrWebhookEvent
	}
	var typ string
	if raw, ok := event["type"]; ok {
		if json.Unmarshal(raw, &typ) != nil {
			return false, app.ErrWebhookEvent
		}
	}
	if typ != "payment_received" {
		return false, nil
	}
	var i, hash string
	if raw, ok := event["externalId"]; ok {
		if json.Unmarshal(raw, &i) != nil {
			return false, app.ErrWebhookEvent
		}
	}
	if raw, ok := event["paymentHash"]; ok {
		if json.Unmarshal(raw, &hash) != nil {
			return false, app.ErrWebhookEvent
		}
	}
	// Serializes duplicate notifications; billing also serializes poller/notification credit.
	p.webhookMu.Lock()
	defer p.webhookMu.Unlock()
	row, e := p.bill.GetInvoice(i)
	if e != nil {
		return false, e
	}
	if row == nil || row["backend"] != "phoenixd" || row["state"] != "pending" {
		return false, nil
	}
	if hash != row["payment_hash"] {
		return false, app.ErrWebhookEvent
	}
	state, e := p.checkPaid(ctx, hash, row)
	if e != nil {
		if e == errBackend {
			return false, e
		}
		return false, app.ErrWebhookEvent
	}
	if e = p.bill.InvoiceState(i, state); e != nil {
		return false, e
	}
	row, e = p.bill.GetInvoice(i)
	return e == nil && row != nil && row["state"] == "paid", e
}

var _ app.Lightning = (*Phoenix)(nil)
var _ app.WalletBackend = (*Phoenix)(nil)
var _ app.WebhookBackend = (*Phoenix)(nil)
var _ app.MarkPaidBackend = (*Fake)(nil)
