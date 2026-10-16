package billing

import "github.com/lucasfth/nocturne/go/internal/pyjson"

// add/sub/less mirror Python arithmetic on SQLite values: int64 stays int64,
// any float operand promotes to float64, None (nil) is treated by callers as 0.
func add(a, c any) any {
	x, ok1 := a.(int64)
	y, ok2 := c.(int64)
	if ok1 && ok2 {
		return x + y
	}
	return f64(a) + f64(c)
}

func neg(a any) any {
	if x, ok := a.(int64); ok {
		return -x
	}
	return -f64(a)
}

func less(a, c any) bool { return f64(a) < f64(c) }

func orZero(v any) any {
	if v == nil {
		return int64(0)
	}
	return v
}

func obj(r Row, cols ...string) pyjson.Object {
	o := make(pyjson.Object, 0, len(cols))
	for _, c := range cols {
		o = append(o, pyjson.Pair{Key: c, Val: r[c]})
	}
	return o
}

const daysSQL = "SELECT date(ts,'unixepoch') day, key_id, COUNT(*) requests, SUM(amount_msat) revenue_msat, SUM(json_extract(meta_json,'$.prompt_tokens')) prompt_tokens, SUM(json_extract(meta_json,'$.completion_tokens')) completion_tokens FROM ledger WHERE event='settle' AND ts>=? GROUP BY day,key_id ORDER BY day DESC"

const keysSQL = `SELECT k.key_id, k.balance_msat, k.created, COALESCE(SUM(CASE WHEN l.event='settle' THEN l.amount_msat END),0) spent_msat, COALESCE(SUM(CASE WHEN l.event IN ('admin_credit','trial_credit') THEN l.amount_msat END),0) free_credit_msat, COUNT(CASE WHEN l.event='settle' THEN 1 END) requests, MAX(CASE WHEN l.event='settle' THEN l.ts END) last_used FROM keys k LEFT JOIN ledger l ON l.key_id=k.key_id GROUP BY k.key_id ORDER BY spent_msat DESC`

// Summary splits real customer money from internal/test traffic. The result
// is an ordered object whose pyjson.Marshal equals Python json.dumps(summary(...)).
func (b *Billing) Summary(since float64, internal []string, realBackend string) (pyjson.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	in := map[any]bool{}
	for _, k := range internal {
		in[k] = true
	}
	// Keys funded only by test (fake-backend) invoices count as internal too.
	rs, err := b.rows("SELECT DISTINCT key_id FROM invoices WHERE backend!=?", realBackend)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		in[r["key_id"]] = true
	}
	days, err := b.rows(daysSQL, since)
	if err != nil {
		return nil, err
	}
	var order []string
	agg := map[string]pyjson.Object{}
	for _, d := range days {
		day := str(d["day"])
		a, ok := agg[day]
		if !ok {
			a = pyjson.Object{{Key: "day", Val: d["day"]}, {Key: "requests", Val: int64(0)}, {Key: "test_requests", Val: int64(0)},
				{Key: "revenue_msat", Val: int64(0)}, {Key: "test_msat", Val: int64(0)}, {Key: "prompt_tokens", Val: int64(0)}, {Key: "completion_tokens", Val: int64(0)}}
			order = append(order, day)
		}
		reqK, msatK := "requests", "revenue_msat"
		if in[d["key_id"]] {
			reqK, msatK = "test_requests", "test_msat"
		}
		for _, kv := range [][2]string{{reqK, "requests"}, {msatK, "revenue_msat"}, {"prompt_tokens", "prompt_tokens"}, {"completion_tokens", "completion_tokens"}} {
			cur, _ := a.Get(kv[0])
			a.Set(kv[0], add(cur, orZero(d[kv[1]])))
		}
		agg[day] = a
	}
	keys, err := b.rows(keysSQL)
	if err != nil {
		return nil, err
	}
	paidRows, err := b.rows("SELECT key_id, SUM(amount_msat) s FROM invoices WHERE state='paid' AND backend=? GROUP BY key_id", realBackend)
	if err != nil {
		return nil, err
	}
	paid := map[any]any{}
	for _, r := range paidRows {
		paid[r["key_id"]] = r["s"]
	}
	inv, err := b.row("SELECT COALESCE(SUM(CASE WHEN state='paid' THEN amount_msat END),0) paid_msat, COUNT(CASE WHEN state='paid' THEN 1 END) paid, COUNT(CASE WHEN state='pending' THEN 1 END) pending FROM invoices WHERE backend=?", realBackend)
	if err != nil {
		return nil, err
	}
	return b.assemble(order, agg, keys, paid, in, inv), nil
}
