package app

import (
	"errors"
	"github.com/lucasfth/nocturne/go/internal/edge"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestInvoiceOwnershipPaymentIdempotency(t *testing.T) {
	a, _, l, token := fixture(t)
	w := request(a, "POST", "/v1/credits/invoice", `{"amount_msat":10000}`, token)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	v := parsed(t, w.Body.String())
	if v["invoice_id"] != "invoice" {
		t.Fatal(v)
	}
	for _, state := range []string{"pending", "paid", "paid"} {
		l.state = state
		w = request(a, "GET", "/v1/credits/invoice/invoice", "", token)
		if w.Code != 200 || parsed(t, w.Body.String())["state"] != state {
			t.Fatal(w.Code, w.Body)
		}
	}
	id, _, _ := a.Bill.KeyForDigest(digest(token))
	key, _ := a.Bill.Key(id)
	if key.CreditMsat != 20000 {
		t.Fatal(key)
	}
	other, e := a.Bill.CreateKey()
	if e != nil {
		t.Fatal(e)
	}
	w = request(a, "GET", "/v1/credits/invoice/invoice", "", other.APIKeyPlaintextOnce)
	if w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
}
func TestInvoiceValidationAndFailure(t *testing.T) {
	for _, tc := range []struct {
		body, name string
		status     int
	}{{`null`, "fake", 400}, {`{`, "fake", 400}, {`{"amount_msat":true}`, "fake", 400}, {`{"amount_msat":1000.0}`, "fake", 400}, {`{"amount_msat":999}`, "fake", 400}, {`{"amount_msat":1001}`, "lnbits", 400}, {`{"amount_msat":1001}`, "phoenixd", 400}, {`{"amount_msat":1000}`, "fake", 502}} {
		a, _, l, token := fixture(t)
		l.name = tc.name
		l.err = errors.New("offline")
		w := request(a, "POST", "/v1/credits/invoice", tc.body, token)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body)
		}
	}
}
func TestInvoiceExpiryAndBackendIsolation(t *testing.T) {
	a, _, l, token := fixture(t)
	id, _, _ := a.Bill.KeyForDigest(digest(token))
	inv, _ := l.CreateInvoice(nil, 1000, "")
	inv.ExpiresAt = 1
	if e := a.Bill.AddInvoice(id, 1000, inv, "fake"); e != nil {
		t.Fatal(e)
	}
	l.err = errors.New("must not poll expired")
	w := request(a, "GET", "/v1/credits/invoice/invoice", "", token)
	if w.Code != 200 || parsed(t, w.Body.String())["state"] != "expired" {
		t.Fatal(w.Code, w.Body)
	}
	inv.InvoiceID = "other"
	if e := a.Bill.AddInvoice(id, 1000, inv, "lnd"); e != nil {
		t.Fatal(e)
	}
	w = request(a, "GET", "/v1/credits/invoice/other", "", token)
	if w.Code != 200 || parsed(t, w.Body.String())["state"] != "pending" {
		t.Fatal(w.Code, w.Body)
	}
}
func TestSignupTrialsAndDocs(t *testing.T) {
	a, _, _, _ := fixture(t)
	a.Config.SignupInvite = "invitation"
	if w := request(a, "POST", "/v1/keys", `{}`, ""); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
	a.Config.OpenSignup = true
	a.Config.TrialCreditMsat = 20000
	a.Config.TrialsPerDay = 1
	a.Config.SignupPerIPPerDay = 2
	for i := 0; i < 3; i++ {
		w := request(a, "POST", "/v1/keys", `{}`, "")
		if i == 2 {
			if w.Code != 429 {
				t.Fatal(w.Code, w.Body)
			}
			continue
		}
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		v := parsed(t, w.Body.String())
		_, trial := v["trial_credit_msat"]
		if trial != (i == 0) {
			t.Fatal(v)
		}
	}
	a.Config.Assets = os.DirFS("../../..//gateway")
	for _, path := range []string{"/", "/robots.txt", "/llms.txt", "/openapi.json"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		a.Public(edge.Config{}).ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src") {
			t.Fatal(path, w.Code, w.Body)
		}
		if path == "/openapi.json" {
			parsed(t, w.Body.String())
			if !strings.Contains(w.Body.String(), a.Config.Model) {
				t.Fatal(w.Body)
			}
		}
	}
}
