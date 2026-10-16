// Package runtime wires the opt-in Go gateway. Construction never fetches prices or contacts a wallet.
package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lucasfth/nocturne/go/internal/app"
	"github.com/lucasfth/nocturne/go/internal/edge"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"github.com/lucasfth/nocturne/go/internal/lightning"
)

var errConfig = errors.New("invalid config.json")

type Config struct {
	Model          string                    `json:"model"`
	ServiceName    string                    `json:"service_name"`
	MaxTokens      int64                     `json:"max_tokens"`
	MaxInputChars  int64                     `json:"max_input_chars"`
	MaxContext     int64                     `json:"max_context"`
	RequestTimeout float64                   `json:"request_timeout_s"`
	QueueDepth     int                       `json:"queue_depth"`
	SignupInvite   string                    `json:"signup_invite"`
	OpenSignup     bool                      `json:"open_signup"`
	SignupPerIP    int                       `json:"signup_per_ip_per_day"`
	Trials         int                       `json:"trials_per_day"`
	TrialCredit    int64                     `json:"trial_credit_msat"`
	PublicBaseURL  *string                   `json:"public_base_url"`
	PerKey         int64                     `json:"per_key_max_concurrent"`
	BulkKeys       []string                  `json:"bulk_keys"`
	BulkSlots      int                       `json:"bulk_slots_per_worker"`
	BulkQueue      int                       `json:"bulk_queue_depth"`
	Thinking       *int64                    `json:"default_thinking_budget"`
	InternalKeys   []string                  `json:"internal_keys"`
	Tailnet        []string                  `json:"admin_tailnet_logins"`
	Macs           map[string]map[string]any `json:"macs"`
	DKK            float64                   `json:"dkk_per_btc"`
	Schedule       *energy.Schedule          `json:"schedule"`
	Energy         *energy.EnergyConfig      `json:"energy"`
	MaxBody        int64                     `json:"max_body_bytes"`
	BodyIdle       float64                   `json:"body_idle_timeout_s"`
	BodyRead       float64                   `json:"body_read_timeout_s"`
	Concurrency    int                       `json:"public_limit_concurrency"`
	PublicRate     int                       `json:"public_ip_rate_per_minute"`
	ChatRate       int                       `json:"chat_ip_rate_per_minute"`
	Lightning      struct {
		Backend    string `json:"backend"`
		BaseURL    string `json:"base_url"`
		InvoiceKey string `json:"invoice_key_file"`
		Macaroon   string `json:"macaroon_file"`
		TLS        string `json:"tls_cert_path"`
		Conf       string `json:"conf_file"`
		Expiry     int64  `json:"expiry_s"`
		Min        int64  `json:"min_msat"`
		Max        int64  `json:"max_msat"`
	} `json:"lightning"`
}

func Load(root string) (Config, error) {
	c := Config{ServiceName: "inference", SignupPerIP: 3, Trials: 100, PerKey: 4, BulkSlots: 1, BulkQueue: 16, DKK: 100000, MaxBody: 262144, BodyIdle: 5, BodyRead: 15, Concurrency: 64, PublicRate: 20, ChatRate: 60}
	c.Lightning.Expiry = 3600
	c.Lightning.Min = 1000
	c.Lightning.Max = 100000000
	b, e := os.ReadFile(filepath.Join(root, "config.json"))
	if e != nil || len(b) > 1<<20 {
		return c, errConfig
	}
	// Reject null anywhere: typed JSON otherwise silently converts null to defaults.
	var raw any
	if !uniqueJSON(b) || json.Unmarshal(b, &raw) != nil || hasNull(raw) {
		return c, errConfig
	}
	if _, ok := raw.(map[string]any); !ok {
		return c, errConfig
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, errConfig
	}
	// Only the typed nonsecret settings overlay may override immutable config.
	overlay, e := app.LoadSettingsOverlay(filepath.Join(root, "config.json"))
	if e != nil {
		return c, e
	}
	if overlay != nil && json.Unmarshal(overlay, &c) != nil {
		return c, errConfig
	}
	if c.Model == "" || c.MaxTokens <= 0 || c.MaxInputChars <= 0 || c.MaxContext < c.MaxTokens || c.RequestTimeout <= 0 || c.RequestTimeout > 86400 || c.QueueDepth <= 0 || c.PerKey <= 0 || c.BulkSlots <= 0 || c.BulkQueue <= 0 || c.SignupPerIP <= 0 || c.Trials <= 0 || c.TrialCredit < 0 || c.DKK <= 0 || c.MaxBody <= 0 || c.BodyIdle <= 0 || c.BodyRead <= 0 || c.BodyRead > 3600 || c.BodyIdle > 3600 || c.Concurrency <= 0 || c.PublicRate <= 0 || c.ChatRate <= 0 || c.Lightning.Expiry <= 0 || c.Lightning.Min <= 0 || c.Lightning.Max < c.Lightning.Min {
		return c, errConfig
	}
	switch c.Lightning.Backend {
	case "fake", "phoenixd", "lnbits", "lnd":
	default:
		return c, errConfig
	}
	if c.Thinking != nil && *c.Thinking < 0 {
		return c, errConfig
	}
	if s := c.Schedule; s != nil && *s != (energy.Schedule{}) {
		if _, e := time.LoadLocation(s.TZ); e != nil {
			return c, errConfig
		}
		for _, v := range []string{s.Start, s.End} {
			if _, e := time.Parse("15:04", v); e != nil {
				return c, errConfig
			}
		}
		if s.GraceMin != nil && (*s.GraceMin < 0 || *s.GraceMin > 1440) {
			return c, errConfig
		}
	}
	if en := c.Energy; en != nil {
		for _, p := range []*float64{en.RefreshMin, en.WattsPerWorker, en.InputCostWeight, en.ReserveMargin} {
			if p != nil && *p <= 0 {
				return c, errConfig
			}
		}
	}
	return c, nil
}
func hasNull(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		for _, v := range x {
			if hasNull(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if hasNull(v) {
				return true
			}
		}
	}
	return false
}
func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }
func (c Config) appConfig() app.Config {
	return app.Config{Model: c.Model, ServiceName: c.ServiceName, RequestTimeout: seconds(c.RequestTimeout), SignupInvite: c.SignupInvite, OpenSignup: c.OpenSignup, SignupPerIPPerDay: c.SignupPerIP, TrialsPerDay: c.Trials, TrialCreditMsat: c.TrialCredit, PublicBaseURL: c.PublicBaseURL, MaxTokens: c.MaxTokens, MaxInputChars: c.MaxInputChars, MaxContext: c.MaxContext, QueueDepth: c.QueueDepth, BulkQueueDepth: c.BulkQueue, BulkKeys: c.BulkKeys, DefaultThinkingBudget: c.Thinking, InvoiceMin: c.Lightning.Min, InvoiceMax: c.Lightning.Max}
}
func (c Config) edgeConfig() edge.Config {
	return edge.Config{PublicRate: c.PublicRate, ChatRate: c.ChatRate, MaxBodyBytes: c.MaxBody, BodyIdleTimeout: seconds(c.BodyIdle), BodyReadTimeout: seconds(c.BodyRead), PublicConcurrency: c.Concurrency}
}
func (c Config) lightningConfig() lightning.Config {
	return lightning.Config{Backend: c.Lightning.Backend, BaseURL: c.Lightning.BaseURL, InvoiceKeyFile: c.Lightning.InvoiceKey, MacaroonFile: c.Lightning.Macaroon, TLSCertPath: c.Lightning.TLS, ConfFile: c.Lightning.Conf, ExpiryS: c.Lightning.Expiry, MinMsat: c.Lightning.Min, MaxMsat: c.Lightning.Max}
}

// Reject ambiguous duplicate object keys rather than silently taking the last value.
func uniqueJSON(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func() bool
	value = func() bool {
		t, e := d.Token()
		if e != nil {
			return false
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				s, ok := k.(string)
				if e != nil || !ok || seen[s] {
					return false
				}
				seen[s] = true
				if !value() {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim('}')
		case json.Delim('['):
			for d.More() {
				if !value() {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim(']')
		}
		return true
	}
	if !value() {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}
