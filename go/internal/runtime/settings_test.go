package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lucasfth/nocturne/go/internal/app"
)

func TestSettingsOverlayRestartWiring(t *testing.T) {
	root := fixture(t) // Disposable ledger and explicit fake payment backend.
	base := filepath.Join(root, "config.json")
	before, _ := os.ReadFile(base)
	if e := os.Chmod(base, 0400); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(root, 0500); e != nil {
		t.Fatal(e)
	}
	defer os.Chmod(root, 0700)
	r, e := New(root, offline())
	if e != nil {
		t.Fatal(e)
	}
	v, e := r.App.Settings.View()
	if e != nil {
		r.Close()
		t.Fatal(e)
	}
	_, e = r.App.Settings.Patch(map[string]any{"revision": v["revision"], "changes": map[string]any{
		"service_name": "Portable 😀 \\unicode", "public_limit_concurrency": json.Number("20"),
	}})
	r.Close()
	if e != nil {
		t.Fatal(e)
	}
	c, e := Load(root)
	if e != nil {
		t.Fatal(e)
	}
	if c.ServiceName != "Portable 😀 \\unicode" || c.Concurrency != 20 || c.edgeConfig().PublicConcurrency != 20 || c.Lightning.Backend != "fake" || len(c.Tailnet) != 1 || c.Tailnet[0] != "allowed" {
		t.Fatal("settings overlay not applied before runtime wiring or changed immutable config")
	}
	restarted, e := New(root, offline())
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	v, e = restarted.App.Settings.View()
	if e != nil || len(v["restart_required"].([]string)) != 0 || restarted.App.Config.ServiceName != "Portable 😀 \\unicode" {
		t.Fatal("restart did not make overlay effective")
	}
	after, _ := os.ReadFile(base)
	if string(before) != string(after) {
		t.Fatal("immutable base was changed")
	}
}

func TestSettingsOverlayLoadBeforeStartup(t *testing.T) {
	root := fixture(t)
	// Encoding emitted by Python (ASCII escaped Unicode) is accepted by Go.
	put(t, root, "data/settings/config.json", `{"service_name":"Portable \ud83d\ude00 \\unicode","public_limit_concurrency":20}`)
	c, e := Load(root)
	if e != nil || c.ServiceName != "Portable 😀 \\unicode" || c.Concurrency != 20 {
		t.Fatal("Python-compatible overlay not loaded")
	}
	for _, raw := range []string{`{"lightning":{"backend":"fake"}}`, `{"public_limit_concurrency":true}`, `{"service_name":null}`, `{"service_name":"A","service_name":"B"}`} {
		put(t, root, "data/settings/config.json", raw)
		if _, e = Load(root); e == nil || e.Error() != "configuration settings startup failed" {
			t.Fatal("invalid settings did not fail config load")
		}
		if r, e := New(root, offline()); e == nil {
			r.Close()
			t.Fatal("invalid settings did not block startup")
		}
	}
	// The overlay is optional for existing installations.
	if e = os.Remove(filepath.Join(root, "data/settings/config.json")); e != nil {
		t.Fatal(e)
	}
	c, e = Load(root)
	if e != nil || c.ServiceName != "inference" || c.Concurrency != 64 {
		t.Fatal("base-only compatibility lost")
	}
	if _, e = app.LoadSettingsOverlay(filepath.Join(root, "config.json")); e != nil {
		t.Fatal(e)
	}
}
