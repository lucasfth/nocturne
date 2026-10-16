package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func settingsFixture(t *testing.T) (*App, AdminConfig, *Settings) {
	t.Helper()
	a, _, c := adminFixture(t)
	p := filepath.Join(t.TempDir(), "config.json")
	if e := os.WriteFile(p, []byte(`{"signup_invite":"private-value","extension":{"password":"hidden-value","number":9007199254740993}}`), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := OpenSettings(p, "inference", 64)
	if e != nil {
		t.Fatal(e)
	}
	a.Settings = s
	return a, c, s
}
func settingsRequest(a *App, c AdminConfig, method, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://test/admin/config", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer admin")
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.Admin(c).ServeHTTP(w, r)
	return w
}
func settingsPatch(t *testing.T, s *Settings, changes string) map[string]any {
	t.Helper()
	v, e := settingsJSON([]byte(fmt.Sprintf(`{"revision":%q,"changes":%s}`, s.revision, changes)))
	if e != nil {
		t.Fatal(e)
	}
	return v.(map[string]any)
}
func TestSettingsHTTPValidationAndEffects(t *testing.T) {
	a, c, s := settingsFixture(t)
	r := settingsRequest(a, c, "GET", "", nil)
	if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" || strings.Contains(r.Body.String(), "private-value") || strings.Contains(r.Body.String(), "hidden-value") {
		t.Fatal(r.Code, r.Body)
	}
	patch := fmt.Sprintf(`{"revision":%q,"changes":{"service_name":"New inference","public_limit_concurrency":20}}`, s.revision)
	for _, h := range []map[string]string{{"Authorization": ""}, {"Origin": "https://evil.example"}, {"Origin": "http://test/"}, {"Sec-Fetch-Site": "cross-site"}, {"Tailscale-User-Login": "outsider"}, {"Content-Type": "text/plain"}} {
		r = settingsRequest(a, c, "PATCH", patch, h)
		if r.Code != 401 && r.Code != 403 && r.Code != 415 {
			t.Fatal(r.Code, r.Body)
		}
	}
	baseBefore, _ := os.ReadFile(s.basePath)
	before, _ := os.ReadFile(s.path)
	invalid := []string{`null`, `[]`, `{`, `{"revision":"a","revision":"b"}`, `{"revision":{},"changes":{"service_name":"Changed"}}`, strings.Repeat(" ", 4097)}
	for _, ch := range []string{`{"service_name":null}`, `{"service_name":"\ud800"}`, `{"service_name":"\udc00"}`, `{"service_name":"<script>"}`, `{"service_name":"\u00a0"}`, `{"service_name":""}`, `{"service_name":true}`, `{"service_name":"a","public_limit_concurrency":true}`, `{"public_limit_concurrency":1.0}`, `{"public_limit_concurrency":4097}`, `{"lightning":{}}`, `{}`, `{"service_name":"A","service_name":"B"}`} {
		invalid = append(invalid, fmt.Sprintf(`{"revision":%q,"changes":%s}`, s.revision, ch))
	}
	for _, b := range invalid {
		r = settingsRequest(a, c, "PATCH", b, nil)
		if r.Code != 400 && r.Code != 409 && r.Code != 413 {
			t.Fatal(b, r.Code, r.Body)
		}
		after, _ := os.ReadFile(s.path)
		if string(before) != string(after) || a.serviceName() != "inference" {
			t.Fatal("invalid mutated state")
		}
	}
	r = settingsRequest(a, c, "PATCH", patch, nil)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	v := parsed(t, r.Body.String())
	if v["effective"].(map[string]any)["service_name"] != "New inference" || v["effective"].(map[string]any)["public_limit_concurrency"] != float64(64) || fmt.Sprint(v["restart_required"]) != "[public_limit_concurrency]" {
		t.Fatal(v)
	}
	if r = settingsRequest(a, c, "PATCH", patch, nil); r.Code != 409 {
		t.Fatal(r.Code)
	}
	raw, _ := os.ReadFile(s.path)
	baseAfter, _ := os.ReadFile(s.basePath)
	if string(baseBefore) != string(baseAfter) {
		t.Fatal("mutated immutable base")
	}
	if strings.Contains(string(raw), "hidden-value") || strings.Contains(string(raw), "9007199254740993") {
		t.Fatal("copied unknown fields into overlay")
	}
	st, _ := os.Stat(s.path)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	restarted, e := OpenSettings(s.basePath, "New inference", 20)
	if e != nil {
		t.Fatal(e)
	}
	v, e = restarted.View()
	if e != nil || len(v["restart_required"].([]string)) != 0 {
		t.Fatal(v, e)
	}
}
func TestSettingsCASConcurrentSnapshots(t *testing.T) {
	a, c, s := settingsFixture(t)
	body := settingsPatch(t, s, `{"service_name":"Concurrent"}`)
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Patch(body)
			code := 200
			if e != nil {
				code = e.(*settingsError).status
			}
			codes <- code
			for j := 0; j < 20; j++ {
				_, _ = s.View()
				_ = a.serviceName()
				_ = settingsRequest(a, c, "GET", "", nil)
				_ = request(a, "GET", "/", "", "")
			}
		}()
	}
	wg.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code != 409 {
			t.Fatal(code)
		}
	}
	if success != 1 {
		t.Fatal(success)
	}
}
func TestSettingsAtomicFailuresAndExternalEdits(t *testing.T) {
	for _, stage := range []string{"file", "directory", "external", "read", "rename"} {
		t.Run(stage, func(t *testing.T) {
			_, _, s := settingsFixture(t)
			before, _ := os.ReadFile(s.path)
			body := settingsPatch(t, s, `{"service_name":"Changed"}`)
			calls := []bool{}
			s.syncFile = func(f *os.File) error {
				st, e := f.Stat()
				if e != nil {
					return e
				}
				dir := st.IsDir()
				calls = append(calls, dir)
				if (stage == "file" && !dir) || (stage == "directory" && dir) {
					return errors.New("private disk details")
				}
				return f.Sync()
			}
			if stage == "external" {
				os.WriteFile(s.path, []byte(`{"service_name":"External"}`), 0600)
			}
			if stage == "read" {
				os.Mkdir(s.path, 0700)
			}
			if stage == "rename" {
				s.syncFile = func(f *os.File) error {
					if e := f.Sync(); e != nil {
						return e
					}
					os.Remove(s.path)
					return os.Mkdir(s.path, 0700)
				}
			}
			_, e := s.Patch(body)
			if e == nil || strings.Contains(e.Error(), "private") {
				t.Fatal(e)
			}
			status := e.(*settingsError).status
			if stage == "external" && status != 409 || stage != "external" && status != 503 {
				t.Fatal(status)
			}
			if s.name != "inference" {
				t.Fatal("applied failed write")
			}
			if stage == "file" {
				after, _ := os.ReadFile(s.path)
				if string(before) != string(after) {
					t.Fatal("changed failed file")
				}
			}
			if stage == "directory" {
				if len(calls) != 2 || calls[0] || !calls[1] {
					t.Fatal(calls)
				}
				if _, e = s.View(); e == nil {
					t.Fatal("uncertain store usable")
				}
				raw, _ := os.ReadFile(s.path)
				var saved map[string]any
				json.Unmarshal(raw, &saved)
				if saved["service_name"] != "Changed" {
					t.Fatal("replacement not reached")
				}
			}
			temps, _ := filepath.Glob(filepath.Join(filepath.Dir(s.path), ".nocturne-settings-*"))
			if len(temps) != 0 {
				t.Fatal(temps)
			}
		})
	}
}

func TestSettingsDurabilitySuccessAndJSONBoundary(t *testing.T) {
	a, c, s := settingsFixture(t)
	calls := []bool{}
	s.syncFile = func(f *os.File) error {
		st, e := f.Stat()
		if e != nil {
			return e
		}
		calls = append(calls, st.IsDir())
		return f.Sync()
	}
	old := s.revision
	body := fmt.Sprintf(`{"revision":%q,"changes":{"service_name":"Portable \ud83d\ude00 \\unicode"}}`, old)
	body += strings.Repeat(" ", 4096-len(body))
	r := settingsRequest(a, c, "PATCH", body, map[string]string{"Origin": "https://test", "Sec-Fetch-Site": "same-origin"})
	if r.Code != 200 || len(calls) != 2 || calls[0] || !calls[1] || s.revision == old {
		t.Fatal(r.Code, r.Body, calls)
	}
	if a.serviceName() != "Portable 😀 \\unicode" {
		t.Fatal(a.serviceName())
	}
	v, e := s.View()
	if e != nil || len(v["restart_required"].([]string)) != 0 {
		t.Fatal(v, e)
	}
}
