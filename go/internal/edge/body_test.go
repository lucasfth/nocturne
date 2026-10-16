package edge

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"sync"

	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type advancingBody struct {
	now   *time.Time
	step  time.Duration
	calls int
}

func (b *advancingBody) Read(p []byte) (int, error) {
	*b.now = b.now.Add(b.step)
	b.calls++
	p[0] = 'x'
	return 1, nil
}
func (b *advancingBody) Close() error { return nil }

type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	calls    int
}

func (w *deadlineWriter) SetReadDeadline(d time.Time) error { w.deadline = d; w.calls++; return nil }

type timedBody struct{ w *deadlineWriter }

func (b timedBody) Read([]byte) (int, error) { return 0, timeoutError{} }
func (b timedBody) Close() error             { return nil }

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
func TestBodyDeadlines(t *testing.T) {
	now := time.Unix(1, 0)
	p := NewPublic(okHandler(), Config{Now: func() time.Time { return now }, BodyReadTimeout: 3 * time.Second, BodyIdleTimeout: 2 * time.Second})
	r := httptest.NewRequest("POST", "/v1/keys", nil)
	b := &advancingBody{now: &now, step: time.Second}
	r.Body = b
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	p.ServeHTTP(w, r)
	detail(t, w.ResponseRecorder, 408, "body read timeout")
	if b.calls != 3 || !w.deadline.IsZero() {
		t.Fatalf("calls=%d deadline=%v", b.calls, w.deadline)
	}
	r = httptest.NewRequest("POST", "/v1/keys", nil)
	r.Body = timedBody{w}
	w = &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	p.ServeHTTP(w, r)
	detail(t, w.ResponseRecorder, 408, "body read timeout")
}
func TestContentLengthAndOrdering(t *testing.T) {
	for _, value := range []string{"-1", "nope", "1.5", "9223372036854775808", "1, 1"} {
		p := NewPublic(okHandler(), Config{})
		r := httptest.NewRequest("POST", "/v1/keys", nil)
		r.Header.Set("Content-Length", value)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		detail(t, w, 400, "invalid content length")
	}
	p := NewPublic(okHandler(), Config{PublicRate: 1, MaxBodyBytes: 3})
	detail(t, request(p, "POST", "/private", "127.0.0.1:1", "", "large"), 404, "not found")
	detail(t, request(p, "POST", "/v1/keys", "127.0.0.1:1", "", "large"), 413, "body too large")
	detail(t, request(p, "POST", "/v1/keys", "127.0.0.1:1", "", "large"), 429, "IP rate limit")
}

type badBody struct{}

func (badBody) Read([]byte) (int, error) { return 0, errors.New("disconnected") }
func (badBody) Close() error             { return nil }
func TestHostileBodies(t *testing.T) {
	p := NewPublic(okHandler(), Config{MaxBodyBytes: 3})
	r := httptest.NewRequest("POST", "/v1/keys", nil)
	r.Body = io.NopCloser(strings.NewReader("1234"))
	r.ContentLength = -1
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	detail(t, w, 413, "body too large")
	r.Body = badBody{}
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	detail(t, w, 400, "invalid body")
}
func TestAllowlist(t *testing.T) {
	for _, path := range []string{"/", "/robots.txt", "/llms.txt", "/openapi.json", "/healthz", "/v1/status", "/v1/models", "/v1/pricing", "/v1/credits/balance", "/v1/credits/invoice/a_Z-09"} {
		if !Allowed("GET", path) {
			t.Fatal(path)
		}
	}
	for _, path := range []string{"/v1/keys", "/v1/credits/invoice", "/v1/chat/completions"} {
		if !Allowed("POST", path) {
			t.Fatal(path)
		}
	}
	for _, path := range []string{"/v1/credits/invoice/", "/v1/credits/invoice/a/b", "/v1/credits/invoice/" + strings.Repeat("a", 129), "/healthz/"} {
		if Allowed("GET", path) {
			t.Fatal(path)
		}
	}
	if Allowed("HEAD", "/healthz") {
		t.Fatal("HEAD must not bypass allowlist")
	}
}

// httptest remains entirely in-process: a real net/http server connection is
// driven through net.Pipe, so timeout tests need neither ports nor external IO.
func TestNativeIdleDeadline(t *testing.T) {
	client, server := net.Pipe()
	listener := &pipeListener{conn: server, closed: make(chan struct{})}
	handler := NewPublic(okHandler(), Config{BodyIdleTimeout: 20 * time.Millisecond, BodyReadTimeout: time.Second})
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = listener
	srv.Start()
	defer srv.Close()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	_, err := io.WriteString(client, "POST /v1/keys HTTP/1.1\r\nHost: test\r\nContent-Length: 5\r\n\r\nx")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 408 || string(body) != `{"detail":"body read timeout"}` {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

type pipeListener struct {
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	if l.conn != nil {
		c := l.conn
		l.conn = nil
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
