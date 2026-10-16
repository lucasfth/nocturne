package energy

// Config is the subset of config.json read by Energy. Pointer fields are
// optional; nil means "absent" and the Python .get default applies.
type Config struct {
	Schedule      *Schedule     `json:"schedule,omitempty"`
	Energy        *EnergyConfig `json:"energy,omitempty"`
	DKKPerBTC     float64       `json:"dkk_per_btc"`
	LiveDKKPerBTC *float64      `json:"live_dkk_per_btc,omitempty"` // set by the app from fx
}

// Schedule is config['schedule']; Start/End are "HH:MM" local to TZ.
type Schedule struct {
	TZ       string   `json:"tz"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
	GraceMin *float64 `json:"grace_min,omitempty"` // used by the app loop, not here
}

// EnergyConfig is config['energy'].
type EnergyConfig struct {
	PriceArea       *string  `json:"price_area,omitempty"`
	RefreshMin      *float64 `json:"refresh_min,omitempty"`
	TariffDKKPerKWh *float64 `json:"tariff_dkk_per_kwh,omitempty"`
	Tiers           *Tiers   `json:"tiers,omitempty"`
	BulkMaxDKKKWh   *float64 `json:"bulk_max_dkk_kwh,omitempty"`
	WattsPerWorker  *float64 `json:"watts_per_worker,omitempty"`
	InputCostWeight *float64 `json:"input_cost_weight,omitempty"`
	ReserveMargin   *float64 `json:"reserve_margin,omitempty"`
}

// Tiers is config['energy']['tiers'].
type Tiers struct {
	CheapMax  *float64 `json:"cheap_max_dkk_kwh,omitempty"`
	NormalMax *float64 `json:"normal_max_dkk_kwh,omitempty"`
}

func or(p *float64, d float64) float64 {
	if p == nil {
		return d
	}
	return *p
}

// truthy mirrors Python truthiness of the config dicts ({} is falsy).
func (s *Schedule) truthy() bool     { return s != nil && *s != Schedule{} }
func (e *EnergyConfig) truthy() bool { return e != nil && *e != EnergyConfig{} }

func (e *EnergyConfig) area() string {
	if e == nil || e.PriceArea == nil {
		return "DK2"
	}
	return *e.PriceArea
}

func (e *EnergyConfig) get(f func(*EnergyConfig) *float64, d float64) float64 {
	if e == nil {
		return d
	}
	return or(f(e), d)
}
