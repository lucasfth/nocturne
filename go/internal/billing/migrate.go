package billing

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type legacyKey struct {
	HashedSecret  string      `json:"hashed_secret"`
	CreditMsat    json.Number `json:"credit_msat"`
	RateLimit     json.Number `json:"rate_limit_per_minute"`
	MaxConcurrent json.Number `json:"max_concurrent"`
	AllowThinking any         `json:"allow_thinking"`
}

type legacyLine struct {
	Event      string      `json:"event"`
	RequestID  any         `json:"request_id"`
	KeyID      any         `json:"key_id"`
	AmountMsat json.Number `json:"amount_msat"`
}

// num converts a JSON number to int64 when integral, else float64 (what
// sqlite3 would bind from the Python value).
func num(n json.Number) any {
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, _ := n.Float64()
	return f
}

func truthy(v any) int64 {
	switch x := v.(type) {
	case bool:
		if x {
			return 1
		}
	case float64:
		if x != 0 {
			return 1
		}
	case string:
		if x != "" {
			return 1
		}
	case nil:
	default:
		return 1
	}
	return 0
}

func decode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(v)
}

// migrateLegacy imports data/api_keys.json (and outstanding reservations
// from ledger.jsonl when the SQL ledger is empty), then retires the file.
func (b *Billing) migrateLegacy() error {
	p := filepath.Join(b.Data, "api_keys.json")
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	// Python iterates dict items in file order; keep it via a token walk.
	order, keys, err := orderedKeys(raw)
	if err != nil {
		return err
	}
	err = b.tx(func() error {
		for _, k := range order {
			v := keys[k]
			if _, err := b.exec("INSERT OR IGNORE INTO keys VALUES(?,?,?,?,?,?,?)", k, v.HashedSecret,
				num(v.CreditMsat), num(v.RateLimit), num(v.MaxConcurrent), truthy(v.AllowThinking), b.now()); err != nil {
				return err
			}
		}
		// Recover legacy outstanding reservations before retiring its ledger.
		if r, err := b.row("SELECT 1 FROM ledger LIMIT 1"); err != nil || r != nil {
			return err
		}
		old, err := os.ReadFile(filepath.Join(b.Data, "ledger.jsonl"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		var pending []legacyLine // insertion-ordered dict semantics
		for _, line := range strings.Split(strings.ReplaceAll(string(old), "\r\n", "\n"), "\n") {
			if line == "" {
				continue
			}
			var r legacyLine
			if err := decode([]byte(line), &r); err != nil {
				return err
			}
			idx := -1
			for i, p := range pending {
				if p.RequestID == r.RequestID {
					idx = i
				}
			}
			if r.Event == "reserve" {
				if idx >= 0 {
					pending[idx] = r
				} else {
					pending = append(pending, r)
				}
			} else if idx >= 0 {
				pending = append(pending[:idx], pending[idx+1:]...)
			}
		}
		for _, r := range pending {
			if _, err := b.exec("UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?", num(r.AmountMsat), r.KeyID); err != nil {
				return err
			}
			if err := b.log("legacy_release", r.RequestID, r.KeyID, num(r.AmountMsat), nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.Chmod(p, 0o400); err != nil {
		return err
	}
	return os.Rename(p, filepath.Join(b.Data, "api_keys.json.migrated"))
}

func orderedKeys(raw []byte) ([]string, map[string]legacyKey, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var order []string
	if _, err := d.Token(); err != nil { // '{'
		return nil, nil, err
	}
	keys := map[string]legacyKey{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, nil, err
		}
		k := t.(string)
		var v legacyKey
		var rm json.RawMessage
		if err := d.Decode(&rm); err != nil {
			return nil, nil, err
		}
		if err := decode(rm, &v); err != nil {
			return nil, nil, err
		}
		if _, dup := keys[k]; !dup {
			order = append(order, k)
		}
		keys[k] = v
	}
	return order, keys, nil
}
