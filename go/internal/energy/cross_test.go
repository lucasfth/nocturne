package energy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lucasfth/nocturne/go/internal/pyjson"
)

const pyScript = `
import asyncio, json, sys
from datetime import datetime, timezone
from gateway.energy import Energy
from fastapi import HTTPException
out = []
for c in json.loads(sys.stdin.read()):
    cfg = dict(schedule=dict(tz='Europe/Copenhagen', start=c['start'], end=c['end']), energy=dict(price_area='DK2', tariff_dkk_per_kwh=1.2), dkk_per_btc=100000)
    now = datetime.fromtimestamp(c['t'], timezone.utc)
    async def prov(area, c=c): return [dict(time=r[0], spot_dkk_mwh=r[1], dataset=r[2]) for r in c['rows']]
    e = Energy(cfg, provider=prov, clock=lambda now=now: now)
    asyncio.run(e.refresh())
    o, n, end = e.window()
    st = e.state({})[0]
    try: e.admit({}, c['msat'], 1000, 1000); a = None
    except HTTPException as x: a = [x.status_code, x.detail, x.headers]
    out.append([o, n.isoformat(), end.isoformat(), st, a])
print(json.dumps(out))
`

func uvPath(t *testing.T) string {
	if p, err := exec.LookPath("uv"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".local/bin/uv")
	if _, err := os.Stat(p); err != nil {
		t.Skip("uv not available")
	}
	return p
}

func TestCrossCheckPython(t *testing.T) {
	uv := uvPath(t)
	rows := [][3]any{{"2026-10-10T21:00:00", 2200, "Elspotprices"}, {"2026-10-25T02:00:00", 500, "Elspotprices"},
		{"2026-03-29T01:45:00", 6000, "DayAheadPrices"}, {"2026-10-11T07:00:00+02:00", 100.5, "DayAheadPrices"}}
	type tc struct {
		T    float64  `json:"t"`
		S    string   `json:"start"`
		E    string   `json:"end"`
		Rows [][3]any `json:"rows"`
		Msat int64    `json:"msat"`
	}
	var cases []tc
	times := []time.Time{time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 10, 19, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC),
		time.Date(2026, 3, 29, 0, 50, 0, 0, time.UTC), time.Date(2026, 3, 28, 16, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 11, 5, 5, 30, 250000000, time.UTC), time.Date(2026, 10, 24, 23, 59, 59, 0, time.UTC)}
	for _, at := range times {
		for _, se := range [][2]string{{"17:30", "08:00"}, {"02:30", "06:00"}, {"09:00", "09:00"}, {"00:00", "02:15"}} {
			for _, m := range []int64{1, 50000} {
				cases = append(cases, tc{T: float64(at.UnixNano()) / 1e9, S: se[0], E: se[1], Rows: rows, Msat: m})
			}
		}
	}
	in, _ := json.Marshal(cases)
	cmd := exec.Command(uv, "run", "--frozen", "python", "-c", pyScript)
	cmd.Dir, cmd.Stdin = "../../..", strings.NewReader(string(in))
	want, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	for _, c := range cases {
		cf := cfg()
		cf.Schedule = &Schedule{TZ: "Europe/Copenhagen", Start: c.S, End: c.E}
		cf.Energy = &EnergyConfig{PriceArea: s("DK2"), TariffDKKPerKWh: f(1.2)}
		var rs []Row
		for _, r := range c.Rows {
			sp := r[1]
			if i, ok := sp.(int); ok {
				sp = int64(i)
			}
			rs = append(rs, Row{r[0].(string), sp, r[2].(string)})
		}
		now := time.Unix(0, int64(c.T*1e9)).UTC()
		e := New(cf, func(context.Context, string) ([]Row, error) { return rs, nil }, func() time.Time { return now })
		e.Refresh(context.Background())
		o, n, end := e.Window()
		st, _ := e.State(nil)
		var a any
		if err := e.Admit(nil, c.Msat, 1000, 1000, false); err != nil {
			h := pyjson.Object{}
			for k, v := range err.Headers {
				h.Set(k, v)
			}
			a = []any{err.Status, err.Detail, h}
		}
		got = append(got, []any{o, pyjson.Isoformat(n), pyjson.Isoformat(end), st.Object(), a})
	}
	g := pyjson.MustMarshal(got)
	if w := strings.TrimSpace(string(want)); g != w {
		gs, ws := strings.Split(g, "], ["), strings.Split(w, "], [")
		for i := range gs {
			if i < len(ws) && gs[i] != ws[i] {
				t.Errorf("case %d (%s):\n go %s\n py %s", i, fmt.Sprint(cases[i].S, "-", cases[i].E, " t=", cases[i].T), gs[i], ws[i])
			}
		}
		t.Fatalf("mismatch")
	}
}
