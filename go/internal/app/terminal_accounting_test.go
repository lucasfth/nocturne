package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type blockedStreamWriter struct {
	header           http.Header
	entered, release chan struct{}
}

func (w *blockedStreamWriter) Header() http.Header { return w.header }
func (w *blockedStreamWriter) WriteHeader(int)     {}
func (w *blockedStreamWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return 0, errors.New("client disconnected")
}

func TestAcceptedFinalSettlesBeforeDelivery(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		t.Run(map[bool]string{false: "write_failure", true: "context_cancel"}[cancelContext], func(t *testing.T) {
			a, c, token := workerFixture(t)
			startWorker(t, c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			q := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(strings.TrimSuffix(chatBody, "}")+`,"stream":true}`)).WithContext(ctx)
			q.Header.Set("Authorization", "Bearer "+token)
			w := &blockedStreamWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
			done := make(chan struct{})
			go func() { defer close(done); a.ServeHTTP(w, q) }()
			deadline := time.Now().Add(2 * time.Second)
			for c.Pending(false) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("no submit")
				}
				time.Sleep(time.Millisecond)
			}
			j := getLease(t, c)
			if r := wr(c, "result", resultBody(j, `"chunk":"hello"`)); !strings.Contains(r.Body.String(), "true") {
				t.Fatal(r.Body)
			}
			select {
			case <-w.entered:
			case <-time.After(time.Second):
				t.Fatal("writer did not block")
			}
			if r := wr(c, "result", resultBody(j, `"chunk":"final answer","final":true`)); !strings.Contains(r.Body.String(), "true") {
				t.Fatal(r.Body)
			}
			rid := j["request_id"].(string)
			assertLedger := func() {
				t.Helper()
				rows, err := a.Bill.Tail()
				if err != nil {
					t.Fatal(err)
				}
				settles, releases, cancels := 0, 0, 0
				for _, row := range rows {
					if row["request_id"] != rid {
						continue
					}
					switch row["event"] {
					case "settle":
						settles++
					case "release":
						releases++
					case "cancel":
						cancels++
					}
				}
				if settles != 1 || releases != 1 || cancels != 0 {
					t.Fatalf("settle=%d release=%d cancel=%d: %v", settles, releases, cancels, rows)
				}
			}
			// Already settled when worker acknowledgement returns, while delivery is blocked.
			assertLedger()
			if cancelContext {
				cancel()
			}
			close(w.release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("public handler stuck")
			}
			c.Cancel(rid)
			cancel()
			assertLedger()
			c.mu.Lock()
			kid := c.jobs[rid].Key
			c.mu.Unlock()
			key, err := a.Bill.Key(kid)
			if err != nil {
				t.Fatal(err)
			}
			if key.CreditMsat != 9993 {
				t.Fatalf("completed inference refunded/mischarged: %d", key.CreditMsat)
			}
		})
	}
}

func TestWorkerTerminalAccountingExactlyOnce(t *testing.T) {
	for _, kind := range []string{"cancel", "context", "overflow", "failure"} {
		t.Run(kind, func(t *testing.T) {
			_, c, _ := workerFixture(t)
			startWorker(t, c)
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{}, 1)
			_, err := c.Submit(ctx, Job{ID: "terminal", Stream: true, Body: map[string]any{"max_tokens": int64(20)}, Finish: func(_ int64, _ *int64, cancelled bool) error {
				if !cancelled {
					t.Error("unexpected settlement")
				}
				calls++
				done <- struct{}{}
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			j := getLease(t, c)
			switch kind {
			case "cancel":
				c.Cancel("terminal")
			case "context":
				cancel()
			case "overflow":
				wr(c, "result", resultBody(j, `"chunk":"`+strings.Repeat("x", 321)+`"`))
			case "failure":
				wr(c, "fail", resultBody(j, `"error":"failed"`))
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("terminal accounting not called")
			}
			c.Cancel("terminal")
			c.mu.Lock()
			defer c.mu.Unlock()
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestWorkerSettlementFailureRejectsFinal(t *testing.T) {
	_, c, _ := workerFixture(t)
	startWorker(t, c)
	settlements, cancels := 0, 0
	ch, err := c.Submit(context.Background(), Job{ID: "billing-error", Stream: true, Body: map[string]any{"max_tokens": int64(20)}, Finish: func(_ int64, _ *int64, cancel bool) error {
		if cancel {
			cancels++
			return nil
		}
		settlements++
		return errors.New("settlement failed")
	}})
	if err != nil {
		t.Fatal(err)
	}
	j := getLease(t, c)
	r := wr(c, "result", resultBody(j, `"chunk":"done","final":true`))
	if !strings.Contains(r.Body.String(), "false") {
		t.Fatal(r.Body)
	}
	if e := <-ch; e.Error != "billing_error" || e.Final {
		t.Fatal(e)
	}
	c.Cancel("billing-error")
	if settlements != 1 || cancels != 1 {
		t.Fatalf("settlements=%d cancellations=%d", settlements, cancels)
	}
}
