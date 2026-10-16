package billing

import (
	"math"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

// Key is the legacy-shaped key record returned by Billing.key()/keys.
type Key struct {
	HashedSecret       string `json:"hashed_secret"`
	CreditMsat         int64  `json:"credit_msat"`
	RateLimitPerMinute int64  `json:"rate_limit_per_minute"`
	MaxConcurrent      int64  `json:"max_concurrent"`
	AllowThinking      bool   `json:"allow_thinking"`
}

func toKey(r Row) Key {
	return Key{str(r["secret_sha256"]), i64(r["balance_msat"]), i64(r["rate_limit"]), i64(r["max_concurrent"]), i64(r["allow_thinking"]) != 0}
}

// sqlNum binds an msat amount the way sqlite3 binds the Python value:
// integral results are ints, anything else stays a float.
func sqlNum(f float64) any {
	if f == math.Trunc(f) && math.Abs(f) < 1<<62 {
		return int64(f)
	}
	return f
}

// KeyForDigest looks a key up by SHA-256 hex digest of its secret.
func (b *Billing) KeyForDigest(digest string) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.row("SELECT key_id FROM keys WHERE secret_sha256=?", digest)
	if err != nil || r == nil {
		return "", false, err
	}
	return str(r["key_id"]), true, nil
}

// Key returns one key, or nil when absent.
func (b *Billing) Key(k string) (*Key, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.row("SELECT * FROM keys WHERE key_id=?", k)
	if err != nil || r == nil {
		return nil, err
	}
	key := toKey(r)
	return &key, nil
}

// Keys returns every key by key_id (the Python `keys` property).
func (b *Billing) Keys() (map[string]Key, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs, err := b.rows("SELECT * FROM keys")
	out := map[string]Key{}
	for _, r := range rs {
		out[str(r["key_id"])] = toKey(r)
	}
	return out, err
}

// Reprice converts DKK prices to msat at dkkPerBTC (1 BTC = 1e11 msat),
// rounding half-even like Python round(); no-op unless all 3 DKK prices exist.
func (b *Billing) Reprice(dkkPerBTC float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.DKKPrices) != 3 || !(dkkPerBTC > 0) {
		return
	}
	msat := func(dkk float64) float64 { return float64(max(1, pyjson.RoundInt(dkk*1e11/dkkPerBTC))) }
	b.InputPrice = msat(b.DKKPrices["input_dkk_per_million_tokens"] / 1000)
	b.OutputPrice = msat(b.DKKPrices["output_dkk_per_million_tokens"] / 1000)
	b.Minimum = msat(b.DKKPrices["min_charge_dkk_per_request"])
}

// Tail returns the last 50 ledger rows, oldest first.
func (b *Billing) Tail() ([]Row, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs, err := b.rows("SELECT * FROM ledger ORDER BY id DESC LIMIT 50")
	// Python list(sqlite cursor) is [] rather than JSON null on an empty ledger.
	if err == nil && rs == nil {
		rs = []Row{}
	}
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return rs, err
}

// Quote is the msat hold for a request: ceil(chars/4) input tokens plus
// maxTokens output tokens, ceil'd to whole msat, at least Minimum.
func (b *Billing) Quote(chars, maxTokens int64) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.quoteLocked(chars, maxTokens)
}
func (b *Billing) quoteLocked(chars, maxTokens int64) float64 {
	in := math.Ceil(float64(chars) / 4)
	return math.Max(b.Minimum, math.Ceil((in*b.InputPrice+float64(maxTokens)*b.OutputPrice)/1000))
}

// Reserve holds Quote() msat for request rid. Idempotent: a known rid returns
// true without charging again. Returns false on insufficient balance.
func (b *Billing) Reserve(rid, k string, chars, maxTokens int64) (bool, error) {
	ok := false
	err := b.tx(func() error {
		amount := sqlNum(b.quoteLocked(chars, maxTokens))
		if r, err := b.row("SELECT 1 FROM reservations WHERE request_id=?", rid); err != nil || r != nil {
			ok = r != nil
			return err
		}
		res, err := b.exec("UPDATE keys SET balance_msat=balance_msat-? WHERE key_id=? AND balance_msat>=?", amount, k, amount)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		now := b.now()
		if _, err := b.exec("INSERT INTO reservations VALUES(?,?,?,?,?,?,?)", rid, k, amount, nil, "reserved", now, b.now()); err != nil {
			return err
		}
		ok = true
		return b.log("reserve", rid, k, amount, nil)
	})
	return ok && err == nil, err
}

// Prices returns a synchronized snapshot for HTTP pricing responses.
func (b *Billing) Prices() (float64, float64, float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.InputPrice, b.OutputPrice, b.Minimum
}
