package fx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

// steps: (clock, provider rate); 0 rate means provider error.
var steps = [][2]float64{{1000, 500000}, {1600, 700000}, {1700, 300000}, {2300, 520000.25}, {2900, 0},
	{3500, 9e7}, {7200.5, 9e7}, {7800, 2e8}, {8400, 6e7}, {9000.75, 61000000}}

const pyScript = `
import asyncio, json, sys, tempfile
from gateway.fx import Fx
steps = json.loads(sys.argv[1]); now = [0]
d = tempfile.mkdtemp(); fx = Fx({'dkk_per_btc': 100000}, d, provider=None, clock=lambda: now[0])
out = []
for i, (t, rate) in enumerate(steps):
    now[0] = t
    async def p(i=i):
        if steps[i][1] == 0: raise RuntimeError('x')
        return steps[i][1]
    fx.provider = p
    ok = asyncio.run(fx.refresh())
    now[0] = t + 2.5
    out.append([ok, fx.status(), open(d + '/fx.json').read() if fx.updated else None])
out.append(Fx({'dkk_per_btc': 1}, d).rate)
print(json.dumps(out))
`

func TestCrossCheckPython(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		home, _ := os.UserHomeDir()
		uv = filepath.Join(home, ".local/bin/uv")
		if _, err := os.Stat(uv); err != nil {
			t.Skip("uv not available")
		}
	}
	cmd := exec.Command(uv, "run", "--frozen", "python", "-c", pyScript, pyjson.MustMarshal(stepsAny()))
	cmd.Dir = "../../.."
	want, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now, i := 0.0, 0
	f := New(100000, dir, func(context.Context) (float64, error) {
		if steps[i][1] == 0 {
			return 0, os.ErrNotExist
		}
		return steps[i][1], nil
	}, func() float64 { return now })
	var out []any
	for i = range steps {
		now = steps[i][0]
		ok, err := f.Refresh(context.Background(), 600)
		if err != nil {
			t.Fatal(err)
		}
		now += 2.5
		var file any
		if f.updated != nil {
			b, _ := os.ReadFile(filepath.Join(dir, "fx.json"))
			file = string(b)
		}
		out = append(out, []any{ok, f.Status(), file})
	}
	out = append(out, New(1, dir, nil, nil).rate)
	if g, w := pyjson.MustMarshal(out), strings.TrimSpace(string(want)); g != w {
		t.Fatalf("\ngo %s\npy %s", g, w)
	}
}

func stepsAny() []any {
	var a []any
	for _, s := range steps {
		a = append(a, []any{s[0], s[1]})
	}
	return a
}
