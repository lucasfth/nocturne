package app

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"unicode/utf8"
)

func decode(r *http.Request) (any, error) {
	d := json.NewDecoder(r.Body)
	d.UseNumber()
	var b any
	err := d.Decode(&b)
	if err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
	}
	return b, err
}
func integer(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, e := n.Int64()
	return i, e == nil
}
func numeric(v any) (float64, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, e := n.Float64()
	return f, e == nil
}

var efforts = map[string]int64{"none": 0, "minimal": 0, "low": 256, "medium": 1024, "high": 4096}

func (a *App) validate(v any) (map[string]any, int64, int64, string) {
	b, ok := v.(map[string]any)
	if !ok {
		return nil, 0, 0, "unsupported parameters"
	}
	allowed := map[string]bool{}
	for _, k := range []string{"model", "messages", "max_tokens", "stream", "temperature", "top_p", "stop", "presence_penalty", "frequency_penalty", "n", "stream_options", "reasoning_effort"} {
		allowed[k] = true
	}
	for k := range b {
		if !allowed[k] {
			return nil, 0, 0, "unsupported parameters"
		}
	}
	n := float64(1)
	if x, has := b["n"]; has {
		n, _ = numeric(x)
	}
	if b["model"] != a.Config.Model || n != 1 {
		return nil, 0, 0, "unsupported model or n"
	}
	if x, has := b["stream"]; has {
		if _, ok := x.(bool); !ok {
			return nil, 0, 0, "invalid stream"
		}
	}
	msgs, ok := b["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return nil, 0, 0, "messages required"
	}
	var chars int64
	for _, v := range msgs {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, 0, 0, "text messages only"
		}
		for k := range m {
			if k != "role" && k != "content" {
				return nil, 0, 0, "text messages only"
			}
		}
		role, _ := m["role"].(string)
		s, ok := m["content"].(string)
		if !ok || (role != "system" && role != "user" && role != "assistant") {
			return nil, 0, 0, "text messages only"
		}
		chars += int64(utf8.RuneCountInString(s))
	}
	mt := a.Config.MaxTokens
	if x, has := b["max_tokens"]; has {
		var ok bool
		mt, ok = integer(x)
		if !ok {
			return nil, 0, 0, "input or token limit exceeded"
		}
	}
	if mt < 1 || mt > a.Config.MaxTokens || chars > a.Config.MaxInputChars || (chars+3)/4+mt > a.Config.MaxContext {
		return nil, 0, 0, "input or token limit exceeded"
	}
	for _, f := range []struct {
		k      string
		lo, hi float64
	}{{"temperature", 0, 2}, {"top_p", 0, 1}, {"presence_penalty", -2, 2}, {"frequency_penalty", -2, 2}} {
		if x, has := b[f.k]; has {
			v, ok := numeric(x)
			_, boolean := x.(bool)
			if !ok || boolean || math.IsNaN(v) || v < f.lo || v > f.hi {
				return nil, 0, 0, "invalid " + f.k
			}
		}
	}
	if x, has := b["reasoning_effort"]; has {
		s, ok := x.(string)
		if _, found := efforts[s]; !ok || !found {
			return nil, 0, 0, "invalid reasoning_effort"
		}
	}
	if x, has := b["stream_options"]; has {
		m, ok := x.(map[string]any)
		if !ok || len(m) != 1 || m["include_usage"] != true {
			return nil, 0, 0, "unsupported stream_options"
		}
	}
	if x, has := b["stop"]; has {
		if _, ok := x.(string); !ok {
			l, ok := x.([]any)
			if !ok || len(l) > 4 {
				return nil, 0, 0, "invalid stop"
			}
			for _, s := range l {
				if _, ok := s.(string); !ok {
					return nil, 0, 0, "invalid stop"
				}
			}
		}
	}
	return b, chars, mt, ""
}
func (a *App) backendBody(b map[string]any, mt int64) map[string]any {
	out := map[string]any{}
	for k, v := range b {
		if k != "reasoning_effort" {
			out[k] = v
		}
	}
	out["max_tokens"] = mt
	budget := int64(512)
	if a.Config.DefaultThinkingBudget != nil {
		budget = *a.Config.DefaultThinkingBudget
	}
	if s, ok := b["reasoning_effort"].(string); ok {
		budget = efforts[s]
	}
	budget = min(budget, mt/2)
	out["chat_template_kwargs"] = map[string]any{"enable_thinking": budget >= 32}
	if budget >= 32 {
		out["thinking_budget_tokens"] = budget
	}
	return out
}
