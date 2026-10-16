package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func overlayBase(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if e := os.WriteFile(path, []byte(`{"service_name":"Base name","public_limit_concurrency":73,"lightning":{"backend":"fake"},"signup_invite":"fixture-only","admin_tailnet_logins":["fixture"],"extension":{"number":9007199254740993,"password":"fixture-only"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(root, "data"), 0700); e != nil {
		t.Fatal(e)
	}
	return path
}
func mustSettingsPatch(t *testing.T, s *Settings, changes string) map[string]any {
	t.Helper()
	v, e := s.Patch(settingsPatch(t, s, changes))
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestSettingsReadonlyBaseAndSparseOverlay(t *testing.T) {
	base := overlayBase(t)
	before, _ := os.ReadFile(base)
	if e := os.Chmod(base, 0400); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Dir(base), 0500); e != nil {
		t.Fatal(e)
	}
	defer os.Chmod(filepath.Dir(base), 0700)
	s, e := OpenSettings(base, "Base name", 73)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(s.path); !os.IsNotExist(e) {
		t.Fatal("overlay should initially be absent")
	}
	mustSettingsPatch(t, s, `{"service_name":"Portable \ud83d\ude00 \\unicode"}`)
	raw, e := LoadSettingsOverlay(base)
	if e != nil {
		t.Fatal(e)
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil || len(values) != 1 || values["service_name"] != "Portable 😀 \\unicode" {
		t.Fatal("overlay must contain only explicit overrides")
	}
	mustSettingsPatch(t, s, `{"public_limit_concurrency":20}`)
	stale := settingsPatch(t, s, `{"service_name":"Stale"}`)
	restarted, e := OpenSettings(base, "Portable 😀 \\unicode", 20)
	if e != nil {
		t.Fatal(e)
	}
	v, e := restarted.View()
	if e != nil || len(v["restart_required"].([]string)) != 0 {
		t.Fatal("restart did not load overlay")
	}
	if _, e = restarted.Patch(stale); e == nil || e.(*settingsError).status != 409 {
		t.Fatal("pre-restart revision accepted")
	}
	after, _ := os.ReadFile(base)
	if string(before) != string(after) {
		t.Fatal("mutated immutable config")
	}
	for p, mode := range map[string]os.FileMode{base: 0400, filepath.Dir(base): 0500, s.path: 0600, filepath.Dir(s.path): 0700} {
		st, e := os.Stat(p)
		if e != nil || st.Mode().Perm() != mode {
			t.Fatal("incorrect mode")
		}
	}
	raw, _ = os.ReadFile(s.path)
	if strings.Contains(string(raw), "fixture-only") || strings.Contains(string(raw), "extension") || strings.Contains(string(raw), "lightning") {
		t.Fatal("copied immutable fields")
	}
	entries, e := os.ReadDir(filepath.Dir(base))
	if e != nil || len(entries) != 2 {
		t.Fatal("wrote outside data")
	}
}
func TestSettingsOverlayExternalCAS(t *testing.T) {
	for _, edit := range []string{"create", "replace", "delete", "base"} {
		t.Run(edit, func(t *testing.T) {
			base := overlayBase(t)
			s, e := OpenSettings(base, "Base name", 73)
			if e != nil {
				t.Fatal(e)
			}
			if edit != "create" {
				mustSettingsPatch(t, s, `{"service_name":"Saved"}`)
			}
			switch edit {
			case "delete":
				e = os.Remove(s.path)
			case "base":
				e = os.WriteFile(base, []byte(`{"extension":1}`), 0600)
			default:
				e = os.WriteFile(s.path, []byte(`{"service_name":"External"}`), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			before, _ := os.ReadFile(s.path)
			if _, e = s.Patch(settingsPatch(t, s, `{"service_name":"Lost update"}`)); e == nil || e.(*settingsError).status != 409 {
				t.Fatal("external edit not detected")
			}
			after, _ := os.ReadFile(s.path)
			if string(before) != string(after) {
				t.Fatal("external state overwritten")
			}
		})
	}
}
func TestSettingsOverlayInvalidStartup(t *testing.T) {
	cases := []string{
		`null`, `[]`, `{`, `{} {}`, strings.Repeat(" ", 4097), strings.Repeat("[", 1500) + strings.Repeat("]", 1500),
		`{"service_name":"A","service_name":"B"}`, `{"service_name":null}`, `{"service_name":true}`, `{"service_name":{}}`,
		`{"service_name":""}`, `{"service_name":"<script>"}`, `{"service_name":"\ud800"}`, `{"service_name":"\u00a0"}`, "{\"service_name\":\"\xff\"}",
		`{"public_limit_concurrency":true}`, `{"public_limit_concurrency":1.0}`, `{"public_limit_concurrency":1e2}`, `{"public_limit_concurrency":null}`,
		`{"public_limit_concurrency":0}`, `{"public_limit_concurrency":4097}`, `{"public_limit_concurrency":NaN}`,
		`{"lightning":{"backend":"fake"}}`, `{"admin_tailnet_logins":[]}`, `{"extension":{}}`, `{"service_name":"Good","signup_invite":"fixture-only"}`,
	}
	for _, raw := range cases {
		base := overlayBase(t)
		path := settingsPath(base)
		if e := os.Mkdir(filepath.Dir(path), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := LoadSettingsOverlay(base); e == nil || e.Error() != "configuration settings startup failed" {
			t.Fatal("invalid overlay accepted or unredacted error")
		}
		if _, e := OpenSettings(base, "Base name", 73); e == nil || e.Error() != "configuration settings startup failed" {
			t.Fatal("invalid overlay accepted or unredacted error")
		}
		after, _ := os.ReadFile(path)
		if string(after) != raw {
			t.Fatal("invalid state overwritten")
		}
	}
}
func TestSettingsOverlayUnsafeState(t *testing.T) {
	for _, kind := range []string{"directory-mode", "file-mode", "directory-link", "file-link", "data-link", "not-file"} {
		t.Run(kind, func(t *testing.T) {
			base := overlayBase(t)
			path := settingsPath(base)
			if e := os.Mkdir(filepath.Dir(path), 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(path, []byte(`{}`), 0600); e != nil {
				t.Fatal(e)
			}
			var e error
			switch kind {
			case "directory-mode":
				e = os.Chmod(filepath.Dir(path), 0755)
			case "file-mode":
				e = os.Chmod(path, 0644)
			case "directory-link", "data-link":
				dir := filepath.Dir(path)
				if kind == "data-link" {
					dir = filepath.Dir(dir)
				}
				moved := dir + "-other"
				if e = os.Rename(dir, moved); e == nil {
					e = os.Symlink(moved, dir)
				}
			case "file-link":
				if e = os.Remove(path); e == nil {
					e = os.Symlink(base, path)
				}
			case "not-file":
				if e = os.Remove(path); e == nil {
					e = os.Mkdir(path, 0700)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = LoadSettingsOverlay(base); e == nil || e.Error() != "configuration settings startup failed" {
				t.Fatal("unsafe overlay accepted or unredacted error")
			}
			if _, e = OpenSettings(base, "Base name", 73); e == nil || e.Error() != "configuration settings startup failed" {
				t.Fatal("unsafe overlay accepted or unredacted error")
			}
		})
	}
}

func TestSettingsLegacyBaseNameCompatibility(t *testing.T) {
	base := overlayBase(t)
	if e := os.WriteFile(base, []byte(`{"service_name":"Research & Development"}`), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := OpenSettings(base, "Research & Development", 64)
	if e != nil {
		t.Fatal(e)
	}
	mustSettingsPatch(t, s, `{"service_name":"Updated"}`)
	if _, e := LoadSettingsOverlay(base); e != nil {
		t.Fatal(e)
	}
}
