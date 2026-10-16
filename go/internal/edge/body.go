package edge

import (
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// Native server read deadlines avoid leaking a goroutine for a stalled client.
// Custom transports must provide a body implementing their own cancellation.
func (p *Public) readBody(w http.ResponseWriter, r *http.Request) ([]byte, int, string) {
	return readBody(w, r, p.config.MaxBodyBytes, p.config.BodyReadTimeout, p.config.BodyIdleTimeout, p.config.Now)
}

func readBody(w http.ResponseWriter, r *http.Request, maxBytes int64, total, idle time.Duration, clock func() time.Time) ([]byte, int, string) {
	if r.Body == nil {
		return nil, 0, ""
	}
	controller := http.NewResponseController(w)
	// Keep unread-body draining from blocking error responses. SetReadDeadline
	// also cancels the net/http read before closing a rejected body.
	complete := false
	defer func() {
		if !complete {
			_ = controller.SetReadDeadline(time.Now())
		}
		_ = r.Body.Close()
		_ = controller.SetReadDeadline(time.Time{})
	}()
	end := clock().Add(total)
	var body []byte
	buf := make([]byte, 32*1024)
	for {
		now := clock()
		deadline := now.Add(idle)
		if end.Before(deadline) {
			deadline = end
		}
		if !deadline.After(now) {
			return nil, 408, "body read timeout"
		}
		// Now is injectable; socket deadlines must remain in the real clock domain.
		err := controller.SetReadDeadline(time.Now().Add(deadline.Sub(now)))
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			return nil, 400, "invalid body"
		}
		remaining := maxBytes - int64(len(body)) + 1
		if remaining <= 0 {
			return nil, 413, "body too large"
		}
		size := len(buf)
		if remaining < int64(size) {
			size = int(remaining)
		}
		n, err := r.Body.Read(buf[:size])
		body = append(body, buf[:n]...)
		if int64(len(body)) > maxBytes {
			return nil, 413, "body too large"
		}
		if err == io.EOF {
			complete = true
			return body, 0, ""
		}
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return nil, 408, "body read timeout"
			}
			return nil, 400, "invalid body"
		}
	}
}
