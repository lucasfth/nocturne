package policy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func b(v bool) *bool { return &v }

func base() Worker {
	return Worker{OnAC: true, BatteryPct: 90, ThermalState: "nominal", UserActive: b(false), Seen: 100, Ready: true}
}

// test_admission_refusal
func TestAdmissionRefusal(t *testing.T) {
	for _, s := range []struct {
		ac  bool
		pct float64
	}{{false, 25}, {false, 20}, {true, 3}} {
		w := base()
		w.OnAC, w.BatteryPct = s.ac, s.pct
		if Eligible(w, 100) {
			t.Errorf("eligible with %+v", s)
		}
	}
}

// test_battery_mode_serves_until_20 and test_unplug_cancel_requeue
func TestBatteryHysteresis(t *testing.T) {
	w := base()
	w.OnAC = false
	w.BatteryPct = 26
	if !Eligible(w, 100) {
		t.Fatal("26% should start")
	}
	w.BatteryPct = 22
	if Eligible(w, 100) {
		t.Fatal("22% idle should not start")
	}
	w.Jobs = 1
	if !Eligible(w, 100) || Unsafe(w) {
		t.Fatal("22% busy should keep serving")
	}
	w.BatteryPct = 20
	if Eligible(w, 100) || !Unsafe(w) {
		t.Fatal("20% must stop")
	}
}

func TestOther(t *testing.T) {
	w := base()
	if Eligible(w, 145) || !Eligible(w, 144.9) {
		t.Error("staleness 45s")
	}
	w.UserActive = nil
	if Eligible(w, 100) || !Unsafe(w) {
		t.Error("missing user_active defaults true")
	}
	w = base()
	w.ThermalState = "serious"
	if Eligible(w, 100) || !Unsafe(w) {
		t.Error("serious thermal")
	}
	w.ThermalState = ""
	if Eligible(w, 100) || Unsafe(w) {
		t.Error("missing thermal: ineligible but not unsafe")
	}
}

// pyRun runs Python code from the repo root via uv; skips if uv is missing.
func pyRun(t *testing.T, code string) []byte {
	t.Helper()
	uv, err := exec.LookPath("uv")
	if err != nil {
		home, _ := os.UserHomeDir()
		uv = filepath.Join(home, ".local/bin/uv")
		if _, err := os.Stat(uv); err != nil {
			t.Skip("uv not available")
		}
	}
	cmd := exec.Command(uv, "run", "--frozen", "python", "-c", code)
	cmd.Dir = "../../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python: %v", err)
	}
	return out
}

func TestCrossCheckPython(t *testing.T) {
	var cases []Worker
	var py []map[string]any
	for _, ac := range []bool{true, false} {
		for _, pct := range []float64{3, 5, 20, 20.5, 25, 25.5, 60} {
			for _, jobs := range []int{0, 1} {
				for _, th := range []string{"nominal", "fair", "serious"} {
					for _, seen := range []float64{100, 50} {
						cases = append(cases, Worker{OnAC: ac, BatteryPct: pct, ThermalState: th, UserActive: b(false), Seen: seen, Ready: true, Jobs: jobs})
						py = append(py, map[string]any{"on_ac": ac, "battery_pct": pct, "thermal_state": th, "user_active": false, "seen": seen, "ready": true, "jobs": make([]int, jobs)})
					}
				}
			}
		}
	}
	cases = append(cases, Worker{OnAC: true, BatteryPct: 90, ThermalState: "fair", Ready: true})
	py = append(py, map[string]any{"on_ac": true, "battery_pct": 90, "thermal_state": "fair", "ready": true, "jobs": []int{}})
	in, _ := json.Marshal(py)
	out := pyRun(t, "import json\nfrom gateway.policy import eligible,unsafe\nws=json.loads("+strconvQuote(string(in))+")\nprint(json.dumps([[eligible(w,140.0),unsafe(w)] for w in ws]))")
	var got [][2]bool
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("bad python output %q: %v", out, err)
	}
	for i, w := range cases {
		if g := [2]bool{Eligible(w, 140), Unsafe(w)}; g != got[i] {
			t.Errorf("case %+v: go %v python %v", w, g, got[i])
		}
	}
}

func strconvQuote(s string) string { q, _ := json.Marshal(s); return string(q) }
