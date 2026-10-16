package edge

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func reject(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]string{"detail": detail})
	_, _ = w.Write(b)
}

// ClientIP trusts only a loopback socket peer, never an arbitrary proxy chain.
func ClientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if peer == "" {
		peer = "unknown"
	}
	if addr, err := netip.ParseAddr(peer); err == nil && addr.IsLoopback() {
		first := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		if ip, err := netip.ParseAddr(first); err == nil && ip.Zone() == "" {
			return ip.String()
		}
	}
	return peer
}
func Bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if strings.HasPrefix(v, "Bearer ") {
		return v[7:]
	}
	return ""
}
func Digest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// AuthenticateDigest computes and compares even when the credential is absent.
func AuthenticateDigest(r *http.Request, expected string) bool {
	want := expected
	if want == "" {
		want = strings.Repeat("0", 64)
	}
	ok := subtle.ConstantTimeCompare([]byte(Digest(Bearer(r))), []byte(want)) == 1
	return expected != "" && ok
}
func TailnetGuard(next http.Handler, logins []string) http.Handler {
	allowed := make(map[string]bool, len(logins))
	for _, login := range logins {
		allowed[login] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if values, present := r.Header[http.CanonicalHeaderKey("Tailscale-User-Login")]; present {
			login := ""
			if len(values) > 0 {
				login = values[0]
			}
			if !allowed[login] {
				reject(w, 403, "not allowed")
				return
			}
			if strings.HasPrefix(r.URL.Path, "/admin/lightning/") {
				reject(w, 404, "not found")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
