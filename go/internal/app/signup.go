package app

import (
	"crypto/subtle"
	"github.com/lucasfth/nocturne/go/internal/edge"
	"io/fs"
	"net/http"
	"strings"
)

func (a *App) signup(w http.ResponseWriter, r *http.Request) {
	v, err := decode(r)
	if err != nil {
		reject(w, 400, "invalid JSON")
		return
	}
	b, ok := v.(map[string]any)
	if !ok {
		reject(w, 400, "invalid body")
		return
	}
	invite, _ := b["invite"].(string)
	invited := a.Config.SignupInvite != "" && subtle.ConstantTimeCompare([]byte(invite), []byte(a.Config.SignupInvite)) == 1
	if !invited && !a.Config.OpenSignup {
		reject(w, 403, "invite required")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Now()
	ip := edge.ClientIP(r)
	hist := a.signups[ip]
	i := 0
	for i < len(hist) && now.Sub(hist[i]).Seconds() >= 86400 {
		i++
	}
	hist = hist[i:]
	if len(hist) >= a.Config.SignupPerIPPerDay {
		reject(w, 429, "signup limit for this address; try tomorrow")
		return
	}
	a.signups[ip] = append(hist, now)
	if len(a.signups) > 20000 {
		for k, v := range a.signups {
			if len(v) == 0 || now.Sub(v[len(v)-1]).Seconds() >= 86400 {
				delete(a.signups, k)
			}
		}
	}
	key, err := a.Bill.CreateKey()
	if err != nil {
		reject(w, 500, "Internal Server Error")
		return
	}
	out := object("key_id", key.KeyID, "api_key_plaintext_once", key.APIKeyPlaintextOnce)
	day := now.UTC().Format("2006-01-02")
	if a.Config.TrialCreditMsat != 0 && a.trials[day] < a.Config.TrialsPerDay {
		a.trials = map[string]int{day: a.trials[day] + 1}
		if err = a.Bill.GrantTrial(key.KeyID, a.Config.TrialCreditMsat); err != nil {
			reject(w, 500, "Internal Server Error")
			return
		}
		out.Set("trial_credit_msat", a.Config.TrialCreditMsat)
	}
	reply(w, 200, out)
}
func (a *App) static(w http.ResponseWriter, r *http.Request) {
	path := "landing.html"
	media := "text/html; charset=utf-8"
	if r.URL.Path != "/" {
		path = "public" + r.URL.Path
		media = "text/plain; charset=utf-8"
		if r.URL.Path == "/openapi.json" {
			media = "application/json"
		}
	}
	if a.Config.Assets == nil {
		reject(w, 503, "assets unavailable")
		return
	}
	raw, err := fs.ReadFile(a.Config.Assets, path)
	if err != nil {
		reject(w, 503, "assets unavailable")
		return
	}
	s := strings.ReplaceAll(strings.ReplaceAll(string(raw), "{{SERVICE}}", a.serviceName()), "{{MODEL}}", a.Config.Model)
	base := ""
	if r.Host != "" {
		base = "https://" + r.Host
	}
	if a.Config.PublicBaseURL != nil {
		base = *a.Config.PublicBaseURL
	}
	s = strings.ReplaceAll(s, "{{BASE}}", base)
	w.Header().Set("Content-Type", media)
	w.WriteHeader(200)
	w.Write([]byte(s))
}
