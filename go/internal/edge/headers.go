package edge

import "net/http"

type secureWriter struct {
	http.ResponseWriter
	landing, wrote bool
}

func (w *secureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *secureWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	if status >= 200 {
		w.wrote = true
	}
	h := w.Header()
	h.Del("Server")
	h.Del("Access-Control-Allow-Origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	csp := "default-src 'none'; frame-ancestors 'none'"
	if w.landing {
		csp = LandingCSP
	}
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Frame-Options", "DENY")
	h.Set("Strict-Transport-Security", "max-age=31536000")
	h.Set("Referrer-Policy", "no-referrer")
	w.ResponseWriter.WriteHeader(status)
}
func (w *secureWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *secureWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// finish applies headers even when the downstream handler writes no body.
func (w *secureWriter) finish() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
}
