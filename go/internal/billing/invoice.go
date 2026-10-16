package billing

// Invoice is the backend-issued invoice handed to AddInvoice.
type Invoice struct {
	InvoiceID   string
	Bolt11      string
	PaymentHash string
	ExpiresAt   float64
}

// AddInvoice records a pending invoice for key k.
func (b *Billing) AddInvoice(k string, amount int64, inv Invoice, backend string) error {
	return b.tx(func() error {
		_, err := b.exec("INSERT INTO invoices VALUES(?,?,?,?,?,?,?,?,?,?)", inv.InvoiceID, k, amount, inv.Bolt11,
			inv.PaymentHash, "pending", b.now(), nil, backend, inv.ExpiresAt)
		return err
	})
}

// GetInvoice returns the invoice row or nil (Python Billing.invoice).
func (b *Billing) GetInvoice(id string) (Row, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.row("SELECT * FROM invoices WHERE invoice_id=?", id)
}

// PendingInvoices lists invoices still in state 'pending'.
func (b *Billing) PendingInvoices() ([]Row, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rows("SELECT * FROM invoices WHERE state='pending'")
}

// InvoiceState transitions a pending invoice. A pending invoice past
// expires_at becomes 'expired' regardless of the requested state; 'paid'
// credits the key once and logs invoice_paid. Non-pending invoices are left alone.
func (b *Billing) InvoiceState(id, state string) error {
	return b.tx(func() error {
		r, err := b.row("SELECT * FROM invoices WHERE invoice_id=?", id)
		if err != nil || r == nil || str(r["state"]) != "pending" {
			return err
		}
		if f64(r["expires_at"]) <= b.now() {
			state = "expired"
		}
		switch state {
		case "paid":
			if _, err := b.exec("UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?", r["amount_msat"], r["key_id"]); err != nil {
				return err
			}
			if err := b.log("invoice_paid", id, r["key_id"], r["amount_msat"], nil); err != nil {
				return err
			}
			_, err = b.exec("UPDATE invoices SET state='paid',paid_at=? WHERE invoice_id=?", b.now(), id)
		case "expired":
			_, err = b.exec("UPDATE invoices SET state='expired' WHERE invoice_id=?", id)
		}
		return err
	})
}
