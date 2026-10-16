package app

import (
	"context"
	"github.com/lucasfth/nocturne/go/internal/billing"
	"net/http"
	"strings"
	"time"
)

func (a *App) createInvoice(w http.ResponseWriter, r *http.Request) {
	k, key := a.auth(w, r)
	if key == nil {
		return
	}
	v, err := decode(r)
	if err != nil {
		reject(w, 400, "invalid JSON")
		return
	}
	b, _ := v.(map[string]any)
	amount, ok := integer(b["amount_msat"])
	if !ok || amount < a.Config.InvoiceMin || amount > a.Config.InvoiceMax {
		reject(w, 400, "invoice amount outside limits")
		return
	}
	name := a.Lightning.Name()
	if (name == "lnbits" || name == "phoenixd") && amount%1000 != 0 {
		reject(w, 400, "whole satoshis required")
		return
	}
	a.invoiceMu.Lock()
	defer a.invoiceMu.Unlock()
	pending, err := a.Bill.PendingInvoices()
	if err != nil {
		reject(w, 500, "Internal Server Error")
		return
	}
	count := 0
	for _, row := range pending {
		if row["key_id"] == k {
			count++
		}
	}
	if count >= 5 {
		reject(w, 429, "too many pending invoices")
		return
	}
	inv, err := a.Lightning.CreateInvoice(r.Context(), amount, a.serviceName()+" prepaid credits")
	if err != nil {
		reject(w, 502, "invoice backend unavailable")
		return
	}
	if err = a.Bill.AddInvoice(k, amount, inv, name); err != nil {
		reject(w, 500, "Internal Server Error")
		return
	}
	reply(w, 200, object("invoice_id", inv.InvoiceID, "bolt11", inv.Bolt11, "payment_hash", inv.PaymentHash, "expires_at", inv.ExpiresAt))
}
func (a *App) getInvoice(w http.ResponseWriter, r *http.Request) {
	k, key := a.auth(w, r)
	if key == nil {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/credits/invoice/")
	row, err := a.Bill.GetInvoice(id)
	if err != nil {
		reject(w, 500, "Internal Server Error")
		return
	}
	if row == nil || row["key_id"] != k {
		reject(w, 404, "not found")
		return
	}
	row, err = a.PollInvoice(r.Context(), id)
	if err != nil {
		reject(w, 502, "invoice backend unavailable")
		return
	}

	reply(w, 200, object("invoice_id", row["invoice_id"], "state", row["state"], "amount_msat", row["amount_msat"], "expires_at", row["expires_at"], "paid_at", row["paid_at"]))
}

// PollInvoice is shared by public lookup and the startup-owned maintenance loop.
func (a *App) PollInvoice(ctx context.Context, id string) (billing.Row, error) {
	row, err := a.Bill.GetInvoice(id)
	if err != nil || row == nil {
		return row, err
	}
	if row["state"] == "pending" && a.Lightning != nil && row["backend"] == a.Lightning.Name() {
		state := "expired"
		if row["expires_at"].(float64) > float64(a.Now().UnixNano())/1e9 {
			state, err = a.Lightning.CheckInvoice(ctx, id)
		}
		if err == nil {
			err = a.Bill.InvoiceState(id, state)
		}
		if err != nil {
			return nil, err
		}
	}
	return a.Bill.GetInvoice(id)
}

// RunInvoiceMaintenance polls immediately, then every 30 seconds. Startup owns
// cancellation and waiting for shutdown. Errors never log remote data or secrets.
func (a *App) RunInvoiceMaintenance(ctx context.Context) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for {
		rows, err := a.Bill.PendingInvoices()
		if err == nil {
			for _, row := range rows {
				if ctx.Err() != nil {
					return
				}
				if _, err := a.PollInvoice(ctx, row["invoice_id"].(string)); err != nil && a.Logf != nil {
					a.Logf("invoice_poll_failed")
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
