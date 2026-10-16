// Package billing is a faithful Go port of gateway/billing.py: an SQLite
// msat ledger with reserve/settle/release accounting and Lightning invoices.
// The on-disk schema and row contents are interchangeable with the Python code.
package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0
)

// DDL is byte-identical to the executescript block in billing.py.
const DDL = `
        CREATE TABLE IF NOT EXISTS keys(key_id TEXT PRIMARY KEY, secret_sha256 TEXT UNIQUE NOT NULL, balance_msat INTEGER NOT NULL CHECK(balance_msat>=0), rate_limit INTEGER NOT NULL, max_concurrent INTEGER NOT NULL, allow_thinking INTEGER NOT NULL, created REAL NOT NULL);
        CREATE TABLE IF NOT EXISTS reservations(request_id TEXT PRIMARY KEY, key_id TEXT REFERENCES keys(key_id), reserved_msat INTEGER NOT NULL, settled_msat INTEGER, state TEXT NOT NULL, created REAL NOT NULL, updated REAL NOT NULL);
        CREATE TABLE IF NOT EXISTS ledger(id INTEGER PRIMARY KEY, ts REAL NOT NULL, event TEXT NOT NULL, request_id TEXT, key_id TEXT, amount_msat INTEGER NOT NULL, meta_json TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS invoices(invoice_id TEXT PRIMARY KEY, key_id TEXT REFERENCES keys(key_id), amount_msat INTEGER NOT NULL, bolt11 TEXT NOT NULL, payment_hash TEXT NOT NULL, state TEXT NOT NULL, created REAL NOT NULL, paid_at REAL, backend TEXT NOT NULL, expires_at REAL NOT NULL);
        `

var dkkKeys = []string{"input_dkk_per_million_tokens", "output_dkk_per_million_tokens", "min_charge_dkk_per_request"}

// Billing mirrors the Python Billing class. One SQLite connection is shared
// and serialised by mu (the Python RLock).
type Billing struct {
	Data                 string
	InputPrice           float64 // msat per 1k input tokens
	OutputPrice          float64 // msat per 1k output tokens
	Minimum              float64 // msat per request
	DKKPrices            map[string]float64
	DefaultMaxConcurrent int64

	poison error // guarded by mu; blocks reuse after unsuccessful rollback
	mu     sync.Mutex
	db     *sql.DB
	conn   *sql.Conn
	now    func() float64 // time.time(); overridable in tests
}

func pyNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Open loads prices.json, opens/creates ledger.sqlite, migrates a legacy
// api_keys.json and releases reservations left over from a previous run.
func Open(data string) (*Billing, error) {
	raw, err := os.ReadFile(filepath.Join(data, "prices.json"))
	if err != nil {
		return nil, err
	}
	var prices map[string]float64
	if err := json.Unmarshal(raw, &prices); err != nil {
		return nil, fmt.Errorf("prices.json: %w", err)
	}
	get := func(k string, def float64) float64 {
		if v, ok := prices[k]; ok {
			return v
		}
		return def
	}
	per := get("price_per_token_msat", 1) * 1000
	b := &Billing{Data: data, InputPrice: get("input_msat_per_1k_tokens", per),
		OutputPrice: get("output_msat_per_1k_tokens", per), Minimum: get("min_charge_msat_per_request", 0),
		DKKPrices: map[string]float64{}, DefaultMaxConcurrent: 4, now: pyNow}
	for _, k := range dkkKeys {
		if v, ok := prices[k]; ok {
			b.DKKPrices[k] = v
		}
	}
	path := filepath.Join(data, "ledger.sqlite")
	if b.db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)"); err != nil {
		return nil, err
	}
	b.db.SetMaxOpenConns(1)
	if b.conn, err = b.db.Conn(context.Background()); err != nil {
		b.db.Close()
		return nil, err
	}
	if err := b.init(path); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

func (b *Billing) init(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	stmts := []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON"}
	for _, s := range strings.Split(DDL, ";") {
		if strings.TrimSpace(s) != "" {
			stmts = append(stmts, s)
		}
	}
	for _, s := range stmts {
		if _, err := b.exec(s); err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(s)[:20], err)
		}
	}
	if err := b.migrateLegacy(); err != nil {
		return err
	}
	// Jobs cannot survive a coordinator restart; release atomically.
	return b.tx(func() error {
		rows, err := b.rows("SELECT * FROM reservations WHERE state='reserved'")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := b.finishRow(r, 0, "release", nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close releases the SQLite connection.
func (b *Billing) Close() error {
	if b.conn != nil {
		b.conn.Close()
	}
	return b.db.Close()
}
