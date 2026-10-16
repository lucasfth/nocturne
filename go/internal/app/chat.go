package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func (a *App) chat(w http.ResponseWriter, r *http.Request) {
	k, key := a.auth(w, r)
	if key == nil {
		return
	}
	v, err := decode(r)
	if err != nil {
		reject(w, 400, "invalid JSON")
		return
	}
	b, chars, mt, detail := a.validate(v)
	if detail != "" {
		reject(w, 400, detail)
		return
	}
	bulk := false
	for _, x := range a.Config.BulkKeys {
		if x == k {
			bulk = true
		}
	}
	a.Energy.Refresh(r.Context())
	if e := a.Energy.Admit(a.Coordinator.Workers(), int64(a.Bill.Quote(chars, mt)), (chars+3)/4, mt, bulk); e != nil {
		for k, v := range e.Headers {
			w.Header().Set(k, v)
		}
		reject(w, e.Status, e.Detail)
		return
	}
	a.mu.Lock()
	if !a.rate(k, key.RateLimitPerMinute, true) || a.active[k] >= key.MaxConcurrent {
		a.mu.Unlock()
		reject(w, 429, "rate or concurrency limit")
		return
	}
	depth := a.Config.QueueDepth
	detail = "queue full"
	if bulk {
		depth = a.Config.BulkQueueDepth
		detail = "bulk queue full"
	}
	if a.Coordinator.Pending(bulk) >= depth {
		a.mu.Unlock()
		reject(w, 503, detail)
		return
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		a.mu.Unlock()
		reject(w, 500, "Internal Server Error")
		return
	}
	rid := fmt.Sprintf("chatcmpl-%x", random)
	ok, err := a.Bill.Reserve(rid, k, chars, mt)
	if err != nil || !ok {
		a.mu.Unlock()
		if err != nil {
			reject(w, 500, "Internal Server Error")
		} else {
			reject(w, 402, "insufficient credit")
		}
		return
	}
	a.rate(k, key.RateLimitPerMinute, false)
	a.active[k]++
	created := a.Now().Unix()
	start := a.Now()
	stream, _ := b["stream"].(bool)
	body := a.backendBody(b, mt)
	ctx, cancel := context.WithCancel(r.Context())
	prompt := (chars + 3) / 4
	var mismatch sync.Once
	var finishMu sync.Mutex
	finished := false
	finish := func(raw int64, reported *int64, cancelled bool) error {
		finishMu.Lock()
		defer finishMu.Unlock()
		if finished {
			return nil
		}
		c := (raw + 3) / 4
		if reported != nil {
			c = *reported
		}
		cap := min(mt, (raw+1)/2)
		if !cancelled && c > cap {
			mismatch.Do(func() { _ = a.Bill.Log("token_mismatch", rid, k, 0, object("reported", c, "cap", cap)) })
		}
		if err := a.Bill.Finish(rid, prompt, min(c, cap), cancelled); err != nil {
			return err
		}
		finished = true
		return nil
	}
	events, submitErr := a.Coordinator.Submit(ctx, Job{ID: rid, Key: k, Body: body, Bulk: bulk, Stream: stream, Finish: finish})
	a.mu.Unlock()
	var completion int64
	cancelled := true
	worker := ""
	var terminalErr string
	defer func() {
		cancel()
		if cancelled {
			a.Coordinator.Cancel(rid)
		}
		// Consult the same terminal accounting state used by accepted worker finals.
		// Cancel above serializes with worker acceptance; never refund a settled final.
		if e := finish(0, nil, true); e != nil {
			a.Logf("request_id=%s event=billing_finish_failed error=%v", rid, e)
		}
		a.mu.Lock()
		a.active[k] = max(0, a.active[k]-1)
		a.mu.Unlock()
		a.Logf("request_id=%s key_id=%s worker_id=%s prompt_tokens=%d completion_tokens=%d latency_ms=%d", rid, k, worker, prompt, completion, a.Now().Sub(start).Milliseconds())
	}()
	if submitErr != nil {
		reject(w, 503, object("type", "worker_interrupted"))
		return
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(200)
	}
	controller := http.NewResponseController(w)
	if stream && a.Config.RequestTimeout > 0 {
		_ = controller.SetWriteDeadline(time.Now().Add(a.Config.RequestTimeout))
		defer controller.SetWriteDeadline(time.Time{})
	}
	send := func(payload any) bool {
		var s string
		if x, ok := payload.(string); ok {
			s = x
		} else {
			s = pyjson.MustMarshal(payload)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", s); err != nil {
			return false
		}
		return http.NewResponseController(w).Flush() == nil
	}
	chunk := func(delta any, finish any) pyjson.Object {
		return object("id", rid, "object", "chat.completion.chunk", "created", created, "model", a.Config.Model, "choices", []any{object("index", 0, "delta", delta, "finish_reason", finish)})
	}
	var timeout <-chan time.Time
	if a.Config.RequestTimeout > 0 {
		timer := time.NewTimer(a.Config.RequestTimeout)
		defer timer.Stop()
		timeout = timer.C
	}
	strip := ThinkStrip{Allow: key.AllowThinking}
	var text, reasoning strings.Builder
	var raw int64
	for terminalErr == "" {
		select {
		case <-timeout:
			terminalErr = "request_timeout"
		case <-r.Context().Done():
			if !stream {
				reject(w, 499, "client disconnected")
			}
			return
		case e, open := <-events:
			if !open {
				terminalErr = "worker_interrupted"
				break
			}
			worker = e.WorkerID
			if e.Error != "" {
				terminalErr = e.Error
				break
			}
			raw += int64(utf8.RuneCountInString(e.Reasoning))
			if key.AllowThinking && e.Reasoning != "" {
				if !stream {
					reasoning.WriteString(e.Reasoning)
				}
				if stream && !e.Final && !send(chunk(object("reasoning_content", e.Reasoning), nil)) {
					return
				}
			}
			raw += int64(utf8.RuneCountInString(e.Text))
			if raw > mt*16 {
				terminalErr = "output_limit"
				break
			}
			visible := strip.Feed(e.Text, e.Final)
			if !stream {
				text.WriteString(visible)
			}
			if stream && !e.Final && visible != "" && !send(chunk(object("content", visible), nil)) {
				return
			}
			if !e.Final {
				continue
			}
			c := (raw + 3) / 4
			if e.CompletionTokens != nil {
				c = *e.CompletionTokens
			}
			if c < 0 {
				terminalErr = "invalid_usage"
				break
			}
			cap := min(mt, (raw+1)/2)
			if c > cap {
				mismatch.Do(func() { _ = a.Bill.Log("token_mismatch", rid, k, 0, object("reported", c, "cap", cap)) })
			}
			completion = min(c, cap)
			usage := object("prompt_tokens", prompt, "completion_tokens", completion, "total_tokens", prompt+completion)
			// Settle before returning. The deferred Finish is idempotent and handles
			// every early-return cancellation before worker final. As in Python,
			// a received final settles even if the final SSE write later fails.
			if err := finish(raw, e.CompletionTokens, false); err != nil {
				terminalErr = "billing_error"
				break
			}
			cancelled = false
			if stream {
				if key.AllowThinking && e.Reasoning != "" && !send(chunk(object("reasoning_content", e.Reasoning), nil)) {
					return
				}
				if visible != "" && !send(chunk(object("content", visible), nil)) {
					return
				}
				if !send(chunk(object(), "stop")) {
					return
				}
				if _, ok := body["stream_options"]; ok {
					if !send(object("id", rid, "object", "chat.completion.chunk", "model", a.Config.Model, "choices", []any{}, "usage", usage)) {
						return
					}
				}
				send("[DONE]")
			} else {
				message := object("role", "assistant", "content", text.String())
				if reasoning.Len() != 0 {
					message.Set("reasoning_content", reasoning.String())
				}
				reply(w, 200, object("id", rid, "object", "chat.completion", "created", created, "model", a.Config.Model, "choices", []any{object("index", 0, "message", message, "finish_reason", "stop")}, "usage", usage))
			}
			return
		}
	}
	if stream {
		send(object("error", object("type", terminalErr)))
		send("[DONE]")
	} else {
		reject(w, 503, object("type", terminalErr))
	}
}
