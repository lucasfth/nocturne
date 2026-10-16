package app

import (
	"encoding/json"
	"github.com/lucasfth/nocturne/go/internal/pyjson"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const pythonPublic = `
import asyncio,json,sys
from pathlib import Path
from tempfile import TemporaryDirectory
from fastapi import HTTPException
from gateway.app import Coordinator,ThinkStrip
out=[]
with TemporaryDirectory() as root:
 p=Path(root);(p/'data').mkdir()
 (p/'config.json').write_text(json.dumps(dict(model='Ornith-1.5-9B',max_tokens=100,max_input_chars=1000,max_context=1000,queue_depth=2)))
 (p/'data/prices.json').write_text('{"price_per_token_msat":1}')
 (p/'data/admin-secret.sha256').write_text('0'*64)
 c=Coordinator(p);c.fx.rate=500000
 for b in json.loads(sys.stdin.read()):
  try:
   chars,mt=c.validate(b);out.append(dict(chars=chars,max_tokens=mt,body=c.backend_body(b,mt)))
  except HTTPException as e:out.append(dict(detail=e.detail))
 s=ThinkStrip();out.append([s.feed('Hello <thi'),s.feed('nk>secret</th'),s.feed('ink>world',True)])
 out.append(c.price_info())
 print(json.dumps(out))
`

func TestParsedPythonComparison(t *testing.T) {
	a, _, _, _ := fixture(t)
	cases := []string{chatBody, strings.TrimSuffix(chatBody, "}") + `,"reasoning_effort":"none"}`, strings.TrimSuffix(chatBody, "}") + `,"max_tokens":100,"reasoning_effort":"high"}`, strings.Replace(chatBody, "hello", "😀é世界", 1), `null`, `{}`, strings.TrimSuffix(chatBody, "}") + `,"tools":[]}`, strings.TrimSuffix(chatBody, "}") + `,"n":true}`, strings.TrimSuffix(chatBody, "}") + `,"max_tokens":1.0}`}
	var input []any
	var got []any
	for _, s := range cases {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var v any
		if e := d.Decode(&v); e != nil {
			continue
		}
		input = append(input, v)
		b, chars, mt, detail := a.validate(v)
		if detail != "" {
			got = append(got, map[string]any{"detail": detail})
		} else {
			got = append(got, map[string]any{"chars": chars, "max_tokens": mt, "body": a.backendBody(b, mt)})
		}
	}
	strip := ThinkStrip{}
	got = append(got, []string{strip.Feed("Hello <thi", false), strip.Feed("nk>secret</th", false), strip.Feed("ink>world", true)}, a.pricing())
	raw, _ := json.Marshal(input)
	home, _ := os.UserHomeDir()
	cmd := exec.Command(filepath.Join(home, ".local/bin/uv"), "run", "--frozen", "python", "-c", pythonPublic)
	cmd.Dir = "../../.."
	cmd.Stdin = strings.NewReader(string(raw))
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("Python comparison: %v %s", err, want)
	}
	// pyjson objects must serialize as ordered objects, not Go slice structs.
	rendered := make([]any, 0, len(got))
	for i, v := range got {
		if i == len(got)-1 {
			rendered = append(rendered, parsed(t, pyjson.MustMarshal(v)))
		} else {
			rendered = append(rendered, v)
		}
	}
	g, _ := json.Marshal(rendered)
	var gv, wv any
	if e := json.Unmarshal(g, &gv); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(want, &wv); e != nil {
		t.Fatal(e, string(want))
	}
	if !reflect.DeepEqual(gv, wv) {
		t.Fatalf("Go %s\nPython %s", g, want)
	}
}
