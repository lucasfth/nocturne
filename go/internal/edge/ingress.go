package edge

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"
)

// WorkerIngress bounds the transport before JSON parsing or coordinator locking.
// It must wrap the real network writer (not a staged response writer).
func WorkerIngress(next http.Handler, authenticate func(*http.Request) int) http.Handler {
	return ingress(next, true, nil, authenticate)
}

// AdminIngress shares the settings origin policy without coupling edge to app.
// Signed webhooks are exempt from browser checks, but not transport limits.
func AdminIngress(next http.Handler, browserGuard func(*http.Request) bool, authenticate func(*http.Request) int) http.Handler {
	return ingress(next, false, browserGuard, authenticate)
}

func bearerShape(r *http.Request) bool {
	h := r.Header.Values("Authorization")
	if len(h) != 1 || !strings.HasPrefix(h[0], "Bearer ") {
		return false
	}
	token := h[0][7:]
	if len(token) == 0 || len(token) > 512 {
		return false
	}
	for _, c := range []byte(token) {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func ingress(next http.Handler, worker bool, browserGuard func(*http.Request) bool, authenticate func(*http.Request) int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Early rejection must not let net/http drain a stalled request body.
		deny := func(status int, detail string) {
			if r.Body != nil {
				controller := http.NewResponseController(w)
				_ = controller.SetReadDeadline(time.Now())
				_ = r.Body.Close()
				_ = controller.SetReadDeadline(time.Time{})
			}
			w.Header().Set("Connection", "close")
			reject(w, status, detail)
		}
		// Authentication precedes all body reads. The callback returns only a status,
		// never registry details or credential material.
		if status := authenticate(r); status != 0 {
			detail := "invalid admin credentials"
			if worker {
				detail = "invalid worker credentials"
			}
			if status == http.StatusServiceUnavailable {
				detail = "machine registry unavailable"
			}
			deny(status, detail)
			return
		}
		limit := int64(65536)
		if worker {
			if r.Method == "POST" && !bearerShape(r) {
				deny(401, "invalid worker credentials")
				return
			}
			limit = 4096
			switch r.URL.Path {
			case "/worker/heartbeat":
				limit = 65536
			case "/worker/result":
				limit = 262144
			}
		} else {
			if r.URL.Path == "/admin/config" {
				limit = 4096
			}
			mutation := r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS"
			if mutation && r.URL.Path != "/admin/lightning/webhook" {
				if !browserGuard(r) {
					deny(403, "cross-origin administration denied")
					return
				}
				// Headerless CLI clients remain compatible. Browser requests and explicit
				// media types must use JSON, never a CORS-safelisted form content type.
				media := r.Header.Get("Content-Type")
				if (media != "" || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "") && strings.ToLower(strings.TrimSpace(strings.Split(media, ";")[0])) != "application/json" {
					deny(415, "application/json required")
					return
				}
			}
		}
		if r.ContentLength > limit {
			deny(413, "body too large")
			return
		}
		body, status, detail := readBody(w, r, limit, 15*time.Second, 5*time.Second, time.Now)
		if status != 0 {
			w.Header().Set("Connection", "close")
			reject(w, status, detail)
			return
		}
		// Only the bounded in-memory copy reaches downstream JSON decoders. Socket
		// deadlines have been cleared; long polls and SSE retain their own lifetime.
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}
