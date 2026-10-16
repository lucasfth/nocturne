package billing

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

// Row is one result row (sqlite3.Row / dict(r) in Python). Values are
// int64, float64, string, []byte or nil, keyed by column name.
type Row map[string]any

var bg = context.Background()

func (b *Billing) exec(q string, args ...any) (sql.Result, error) {
	if b.poison != nil {
		return nil, b.poison
	}
	return b.conn.ExecContext(bg, q, args...)
}

func (b *Billing) rows(q string, args ...any) ([]Row, error) {
	if b.poison != nil {
		return nil, b.poison
	}
	rs, err := b.conn.QueryContext(bg, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range cols {
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func (b *Billing) row(q string, args ...any) (Row, error) {
	rs, err := b.rows(q, args...)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return rs[0], nil
}

// tx runs fn inside BEGIN IMMEDIATE ... COMMIT (ROLLBACK on error or panic),
// holding the connection mutex like Billing.transaction() in Python.
// fn must use the unlocked helpers only.
func (b *Billing) tx(fn func() error) (err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err = b.exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			b.rollback()
			panic(p)
		}
		if err != nil {
			b.rollback()
			return
		}
		_, err = b.exec("COMMIT")
		if err != nil {
			b.rollback()
		}
	}()
	return fn()
}

// log inserts a ledger row; meta must be the kwargs in call order so the
// meta_json text is byte-identical to Python json.dumps(meta).
func (b *Billing) log(event string, rid, k any, amount any, meta pyjson.Object) error {
	if meta == nil {
		meta = pyjson.Object{}
	}
	m, err := pyjson.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = b.exec("INSERT INTO ledger(ts,event,request_id,key_id,amount_msat,meta_json) VALUES(?,?,?,?,?,?)",
		b.now(), event, rid, k, amount, m)
	return err
}

// Log is the public, transactional Billing.log(event, rid, k, amount, **meta).
// rid and k may be nil (SQL NULL).
func (b *Billing) Log(event string, rid, k any, amount int64, meta pyjson.Object) error {
	return b.tx(func() error { return b.log(event, rid, k, amount, meta) })
}

func i64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case nil:
		return 0
	}
	return 0
}

func f64(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

// Exec runs raw SQL under the lock (tests use it like c.bill.db.execute).
func (b *Billing) Exec(q string, args ...any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.exec(q, args...)
	return err
}

// A failed rollback leaves transaction state uncertain. Fail closed on every
// subsequent operation rather than reuse that connection. Caller holds mu.
func (b *Billing) rollback() {
	if _, err := b.exec("ROLLBACK"); err != nil {
		b.poison = fmt.Errorf("billing connection poisoned after rollback failure: %w", err)
	}
}
