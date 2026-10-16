package billing

import "github.com/lucasfth/nocturne/go/internal/pyjson"

func (b *Billing) assemble(order []string, agg map[string]pyjson.Object, keys []Row, paid map[any]any, in map[any]bool, inv Row) pyjson.Object {
	dayList := make([]any, 0, len(order))
	var testUsage any = int64(0)
	for _, d := range order {
		dayList = append(dayList, agg[d])
		t, _ := agg[d].Get("test_msat")
		testUsage = add(testUsage, t)
	}
	keyList := make([]any, 0, len(keys))
	var earned, liability any = int64(0), int64(0)
	trials, customers := int64(0), int64(0)
	for _, k := range keys {
		o := obj(k, "key_id", "balance_msat", "created", "spent_msat", "free_credit_msat", "requests", "last_used")
		internal := in[k["key_id"]]
		paidIn, ok := paid[k["key_id"]]
		if !ok {
			paidIn = int64(0)
		}
		// Conservative: free/trial credit is consumed first; only spend beyond it counts as earned.
		var e any = int64(0)
		if !internal {
			m := add(k["spent_msat"], neg(k["free_credit_msat"]))
			if less(paidIn, m) { // min() returns the first argument on ties
				m = paidIn
			}
			if less(e, m) { // max(0, m) keeps 0 on ties
				e = m
			}
		}
		o = append(o, pyjson.Pair{Key: "internal", Val: internal}, pyjson.Pair{Key: "paid_in_msat", Val: paidIn}, pyjson.Pair{Key: "earned_msat", Val: e})
		keyList = append(keyList, o)
		if internal {
			continue
		}
		earned = add(earned, e)
		liability = add(liability, k["balance_msat"])
		if less(int64(0), k["free_credit_msat"]) && f64(paidIn) == 0 {
			trials++
		}
		if less(int64(0), paidIn) {
			customers++
		}
	}
	return pyjson.Object{
		{Key: "days", Val: dayList},
		{Key: "keys", Val: keyList},
		{Key: "invoices", Val: obj(inv, "paid_msat", "paid", "pending")},
		{Key: "earned_msat", Val: earned},
		{Key: "trial_users", Val: trials},
		{Key: "test_usage_msat", Val: testUsage},
		{Key: "customers", Val: customers},
		{Key: "customer_credit_liability_msat", Val: liability},
	}
}
