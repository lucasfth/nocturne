package app

import (
	"context"
	"encoding/json"
	"github.com/lucasfth/nocturne/go/internal/energy"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidation(t *testing.T) {
	a, _, _, _ := fixture(t)
	for _, tc := range []struct{ extra, detail string }{{`"tools":[]`, "unsupported parameters"}, {`"model":"other"`, "unsupported model or n"}, {`"n":2`, "unsupported model or n"}, {`"stream":1`, "invalid stream"}, {`"messages":[]`, "messages required"}, {`"messages":[{"role":"tool","content":"x"}]`, "text messages only"}, {`"max_tokens":true`, "input or token limit exceeded"}, {`"max_tokens":1.0`, "input or token limit exceeded"}, {`"temperature":true`, "invalid temperature"}, {`"top_p":2`, "invalid top_p"}, {`"reasoning_effort":"extreme"`, "invalid reasoning_effort"}, {`"stream_options":{}`, "unsupported stream_options"}, {`"stop":[1]`, "invalid stop"}} {
		var b map[string]any
		d := json.NewDecoder(strings.NewReader(chatBody))
		d.UseNumber()
		d.Decode(&b)
		var x map[string]any
		d = json.NewDecoder(strings.NewReader("{" + tc.extra + "}"))
		d.UseNumber()
		d.Decode(&x)
		for k, v := range x {
			b[k] = v
		}
		_, _, _, detail := a.validate(b)
		if detail != tc.detail {
			t.Fatalf("%s: %s", tc.extra, detail)
		}
	}
}
func TestReasoningBudgetAndUnicode(t *testing.T) {
	a, _, _, _ := fixture(t)
	r := httptest.NewRequest("POST", "/", strings.NewReader(strings.Replace(chatBody, "hello", "😀😀😀😀😀", 1)))
	v, e := decode(r)
	if e != nil {
		t.Fatal(e)
	}
	b, chars, _, detail := a.validate(v)
	if chars != 5 || detail != "" {
		t.Fatal(chars, detail)
	}
	body := a.backendBody(b, 100)
	if body["thinking_budget_tokens"] != int64(50) {
		t.Fatal(body)
	}
	b["reasoning_effort"] = "none"
	body = a.backendBody(b, 100)
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatal(body)
	}
	if _, ok := body["thinking_budget_tokens"]; ok {
		t.Fatal(body)
	}
}
func TestThinkStrip(t *testing.T) {
	s := ThinkStrip{}
	if s.Feed("Hello <thi", false) != "Hello " || s.Feed("nk>secret</th", false) != "" || s.Feed("ink>world", true) != "world" {
		t.Fatal(s)
	}
	s = ThinkStrip{Allow: true}
	if s.Feed("<think>secret</think>", false) != "<think>secret</think>" {
		t.Fatal(s)
	}
}
func TestStreamReasoningAndUsage(t *testing.T) {
	a, f, _, token := fixture(t)
	n := int64(8)
	f.events <- Event{Reasoning: "secret plan"}
	f.events <- Event{Text: "answer", Final: true, CompletionTokens: &n}
	b := strings.TrimSuffix(chatBody, "}") + `,"stream":true,"stream_options":{"include_usage":true}}`
	w := request(a, "POST", "/v1/chat/completions", b, token)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatal(w.Code, w.Body)
	}
	s := w.Body.String()
	if strings.Contains(s, "secret") || !strings.HasSuffix(s, "data: [DONE]\n\n") {
		t.Fatal(s)
	}
	frames := strings.Split(strings.TrimSuffix(s, "\n\n"), "\n\n")
	if len(frames) != 4 {
		t.Fatal(s)
	}
	for _, f := range frames[:3] {
		parsed(t, strings.TrimPrefix(f, "data: "))
	}
	if !strings.Contains(s, `"completion_tokens": 8`) {
		t.Fatal(s)
	}
}
func TestInterruptedRefund(t *testing.T) {
	for _, stream := range []bool{false, true} {
		a, f, _, token := fixture(t)
		f.events <- Event{Error: "worker_interrupted"}
		b := chatBody
		if stream {
			b = strings.TrimSuffix(b, "}") + `,"stream":true}`
		}
		w := request(a, "POST", "/v1/chat/completions", b, token)
		if !strings.Contains(w.Body.String(), "worker_interrupted") || f.cancelled != 1 {
			t.Fatal(w.Code, w.Body, f.cancelled)
		}
		key, _ := a.Bill.Key(f.job.Key)
		if key.CreditMsat != 10000 {
			t.Fatal(key)
		}
	}
}
func TestAdmissionOrder(t *testing.T) {
	for _, kind := range []string{"rate", "active", "queue", "credit", "energy"} {
		t.Run(kind, func(t *testing.T) {
			a, f, _, token := fixture(t)
			id, _, _ := a.Bill.KeyForDigest(digest(token))
			key, _ := a.Bill.Key(id)
			status := 429
			switch kind {
			case "rate":
				a.rates[id] = make([]time.Time, key.RateLimitPerMinute)
				for i := range a.rates[id] {
					a.rates[id][i] = a.Now()
				}
			case "active":
				a.active[id] = key.MaxConcurrent
			case "queue":
				f.pending = 2
				status = 503
			case "credit":
				a.Bill.Credit(id, -10000)
				status = 402
			case "energy":
				a.Energy = energy.New(&energy.Config{Schedule: &energy.Schedule{TZ: "UTC", Start: "01:00", End: "02:00"}}, nil, func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) })
				status = 503
			}
			w := request(a, "POST", "/v1/chat/completions", chatBody, token)
			if w.Code != status {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
}
func TestDisconnectRefund(t *testing.T) {
	a, f, _, token := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 499 || f.cancelled != 1 {
		t.Fatal(w.Code, w.Body)
	}
	key, _ := a.Bill.Key(f.job.Key)
	if key.CreditMsat != 10000 {
		t.Fatal(key)
	}
}

func TestTimeoutRefundAndLogging(t *testing.T) {
	a, f, _, token := fixture(t)
	a.Config.RequestTimeout = time.Millisecond
	var logs []string
	a.Logf = func(s string, _ ...any) { logs = append(logs, s) }
	w := request(a, "POST", "/v1/chat/completions", chatBody, token)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "request_timeout") {
		t.Fatal(w.Code, w.Body)
	}
	key, _ := a.Bill.Key(f.job.Key)
	if key.CreditMsat != 10000 || len(logs) != 1 || !strings.HasPrefix(logs[0], "request_id=") {
		t.Fatal(key, logs)
	}
}

type brokenStream struct {
	header http.Header
	status int
}

func (w *brokenStream) Header() http.Header       { return w.header }
func (w *brokenStream) WriteHeader(s int)         { w.status = s }
func (w *brokenStream) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (w *brokenStream) Flush()                    {}
func TestFinalWriteFailureStillSettles(t *testing.T) {
	a, f, _, token := fixture(t)
	n := int64(3)
	f.events <- Event{Text: "hello", Final: true, CompletionTokens: &n}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(strings.TrimSuffix(chatBody, "}")+`,"stream":true}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := &brokenStream{header: make(http.Header)}
	a.ServeHTTP(w, r)
	key, _ := a.Bill.Key(f.job.Key)
	if key.CreditMsat != 9995 || f.cancelled != 0 {
		t.Fatal(key, f.cancelled)
	}
}
func TestPartialWriteFailureRefunds(t *testing.T) {
	a, f, _, token := fixture(t)
	f.events <- Event{Text: "partial"}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(strings.TrimSuffix(chatBody, "}")+`,"stream":true}`))
	r.Header.Set("Authorization", "Bearer "+token)
	a.ServeHTTP(&brokenStream{header: make(http.Header)}, r)
	key, _ := a.Bill.Key(f.job.Key)
	if key.CreditMsat != 10000 || f.cancelled != 1 {
		t.Fatal(key, f.cancelled)
	}
}

func TestOutputLimitAndClosedEventsRefund(t *testing.T) {
	for _, closed := range []bool{false, true} {
		a, f, _, token := fixture(t)
		if closed {
			close(f.events)
		} else {
			f.events <- Event{Text: strings.Repeat("x", 321)}
		}
		w := request(a, "POST", "/v1/chat/completions", chatBody, token)
		if w.Code != 503 {
			t.Fatal(w.Code, w.Body)
		}
		key, _ := a.Bill.Key(f.job.Key)
		if key.CreditMsat != 10000 {
			t.Fatal(key)
		}
	}
}
