// Package edge implements the HTTP perimeter, independently of gateway routes.
package edge

import "time"

const LandingCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Config uses Python perimeter defaults when fields are zero. Negative limits
// explicitly disable admission. Now must be monotonic and safe for concurrent use.
type Config struct {
	PublicRate, ChatRate             int
	MaxBodyBytes                     int64
	BodyIdleTimeout, BodyReadTimeout time.Duration
	PublicConcurrency                int
	Now                              func() time.Time
}

func (c Config) defaults() Config {
	if c.PublicRate == 0 {
		c.PublicRate = 20
	}
	if c.ChatRate == 0 {
		c.ChatRate = 60
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 256 * 1024
	}
	if c.BodyIdleTimeout == 0 {
		c.BodyIdleTimeout = 5 * time.Second
	}
	if c.BodyReadTimeout == 0 {
		c.BodyReadTimeout = 15 * time.Second
	}
	if c.PublicConcurrency == 0 {
		c.PublicConcurrency = 64
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}
