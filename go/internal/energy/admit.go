package energy

import (
	"math"
	"strconv"
)

// AdmitError mirrors the HTTPException(503, detail, headers) raised by admit().
type AdmitError struct {
	Status  int
	Detail  string
	Headers map[string]string
}

func (e *AdmitError) Error() string { return e.Detail }

func refuse(detail, k, v string) *AdmitError {
	return &AdmitError{Status: 503, Detail: detail, Headers: map[string]string{k: v}}
}

// Admit ports Energy.admit(); nil means admitted.
func (e *Energy) Admit(workers map[string]Worker, reservedMsat, inputTokens, outputTokens int64, bulk bool) *AdmitError {
	st, _ := e.State(workers)
	if !st.WindowOpen {
		next, _ := ParseISO(st.NextWindowStart)
		d := next.Sub(e.Clock())
		secs := float64(d/1000) / 1e6 // timedelta has microsecond resolution
		retry := int64(math.Max(1, math.Ceil(secs)))
		return refuse("operating window closed", "Retry-After", strconv.FormatInt(retry, 10))
	}
	if st.Tier != "expensive" {
		return nil
	}
	ec := e.Config.Energy
	price := st.DKKPerKWh // non-nil: tier is expensive
	get := func(f func(*EnergyConfig) *float64, d float64) float64 { return ec.get(f, d) }
	if bulk {
		// Own internal jobs earn nothing; they only pause on a real price spike.
		if *price > get(func(c *EnergyConfig) *float64 { return c.BulkMaxDKKKWh }, 4.0) {
			return refuse("energy price above bulk ceiling", "X-Reason", "energy-price")
		}
		return nil
	}
	speed, found := 0.0, false
	for _, w := range workers {
		s := 20.0
		if w.TokensPerSec != nil {
			s = *w.TokensPerSec
		}
		if s > 0 && (!found || s < speed) {
			speed, found = s, true
		}
	}
	if !found {
		speed = 20 // conservative for the slowest worker; default 20
	}
	costPerToken := (get(func(c *EnergyConfig) *float64 { return c.WattsPerWorker }, 30) / 1000 * *price) / (speed * 3600)
	work := float64(outputTokens) + float64(inputTokens)*get(func(c *EnergyConfig) *float64 { return c.InputCostWeight }, 0.1)
	rate := e.rate()
	revenue := float64(reservedMsat) / 1e11 * rate
	if revenue < get(func(c *EnergyConfig) *float64 { return c.ReserveMargin }, 1.5)*costPerToken*math.Max(1, work) {
		return refuse("energy price exceeds reserve margin", "X-Reason", "energy-price")
	}
	return nil
}
