package billing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

// finishRow writes the two ledger rows of a finished reservation:
// state (settle/cancel/release) with `actual`, then release of the remainder.
func (b *Billing) finishRow(r Row, actual any, state string, meta pyjson.Object) error {
	rel := sub(r["reserved_msat"], actual)
	if _, err := b.exec("UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?", rel, r["key_id"]); err != nil {
		return err
	}
	if _, err := b.exec("UPDATE reservations SET settled_msat=?, state=?, updated=? WHERE request_id=?", actual, state, b.now(), r["request_id"]); err != nil {
		return err
	}
	if err := b.log(state, r["request_id"], r["key_id"], actual, meta); err != nil {
		return err
	}
	return b.log("release", r["request_id"], r["key_id"], rel, nil)
}

func sub(a, c any) any {
	if x, ok := a.(int64); ok {
		if y, ok := c.(int64); ok {
			return x - y
		}
	}
	return sqlNum(f64(a) - f64(c))
}

// Finish settles (or cancels) a reserved request. Idempotent: only a row in
// state 'reserved' is touched. Charge = min(reserved, max(Minimum,
// ceil((prompt*in + completion*out)/1000))).
func (b *Billing) Finish(rid string, prompt, completion int64, cancel bool) error {
	return b.tx(func() error {
		r, err := b.row("SELECT * FROM reservations WHERE request_id=? AND state='reserved'", rid)
		if err != nil || r == nil {
			return err
		}
		var actual any = int64(0)
		state := "cancel"
		if !cancel {
			state = "settle"
			c := math.Max(b.Minimum, math.Ceil((float64(prompt)*b.InputPrice+float64(completion)*b.OutputPrice)/1000))
			actual = sqlNum(math.Min(f64(r["reserved_msat"]), c))
		}
		return b.finishRow(r, actual, state, pyjson.Object{{Key: "prompt_tokens", Val: prompt}, {Key: "completion_tokens", Val: completion}})
	})
}

func (b *Billing) addCredit(event, k string, amount int64) error {
	return b.tx(func() error {
		if _, err := b.exec("UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?", amount, k); err != nil {
			return err
		}
		return b.log(event, nil, k, amount, nil)
	})
}

// Credit adds admin credit (ledger event admin_credit).
func (b *Billing) Credit(k string, amount int64) error { return b.addCredit("admin_credit", k, amount) }

// GrantTrial adds trial credit (ledger event trial_credit).
func (b *Billing) GrantTrial(k string, amount int64) error {
	return b.addCredit("trial_credit", k, amount)
}

// NewKey is the create_key() result; the plaintext is shown only once.
type NewKey struct {
	KeyID               string `json:"key_id"`
	APIKeyPlaintextOnce string `json:"api_key_plaintext_once"`
}

// CreateKey makes a zero-balance key: key-<24 hex>, secret token_urlsafe(32).
func (b *Billing) CreateKey() (NewKey, error) {
	id, sec := make([]byte, 12), make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return NewKey{}, err
	}
	if _, err := rand.Read(sec); err != nil {
		return NewKey{}, err
	}
	nk := NewKey{"key-" + hex.EncodeToString(id), base64.RawURLEncoding.EncodeToString(sec)}
	sum := sha256.Sum256([]byte(nk.APIKeyPlaintextOnce))
	err := b.tx(func() error {
		_, err := b.exec("INSERT INTO keys VALUES(?,?,?,?,?,?,?)", nk.KeyID, hex.EncodeToString(sum[:]), 0, 30, b.DefaultMaxConcurrent, 0, b.now())
		return err
	})
	if err != nil {
		return NewKey{}, err
	}
	return nk, nil
}
