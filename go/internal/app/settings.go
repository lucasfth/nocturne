package app

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Settings owns a single-process config snapshot. Every snapshot and live read
// shares this lock; App.Config and the edge listener remain immutable.
type Settings struct {
	mu          sync.Mutex
	path        string
	basePath    string
	baseRaw     []byte
	raw         []byte
	overlay     map[string]json.RawMessage
	saved       map[string]json.RawMessage
	revision    string
	name        string
	concurrency int
	uncertain   bool
	// Injectable fsync for deterministic pre/post-replacement failure tests.
	syncFile func(*os.File) error
}
type settingsError struct {
	status int
	detail string
}

func (e *settingsError) Error() string   { return e.detail }
func se(status int, detail string) error { return &settingsError{status, detail} }
func revision() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("configuration revision entropy unavailable")
	}
	return hex.EncodeToString(b[:])
}

// The overlay format and path are shared with Python. It contains only explicit
// overrides, never credentials, backend configuration or unknown extensions.
// One coordinator owns this state; external edits require restart.
func settingsPath(base string) string {
	return filepath.Join(filepath.Dir(base), "data", "settings", "config.json")
}
func settingsDirectory(path string, private bool) error {
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !st.IsDir() || (private && st.Mode() != os.ModeDir|0700) {
		return errors.New("invalid settings directory")
	}
	return nil
}
func prepareSettingsDirectory(path string) error {
	for _, dir := range []string{filepath.Dir(filepath.Dir(path)), filepath.Dir(path)} {
		e := os.Mkdir(dir, 0700)
		if e == nil {
			parent, err := os.Open(filepath.Dir(dir))
			if err != nil {
				return err
			}
			err = parent.Sync()
			parent.Close()
			if err != nil {
				return err
			}
		} else if !errors.Is(e, os.ErrExist) {
			return e
		}
		if e = settingsDirectory(dir, dir == filepath.Dir(path)); e != nil {
			return e
		}
	}
	return nil
}
func validateSettings(changes map[string]any) error {
	for k, v := range changes {
		switch k {
		case "service_name":
			if !validName(v) {
				return se(400, "invalid setting value")
			}
		case "public_limit_concurrency":
			n, ok := integer(v)
			if !ok || n < 1 || n > 4096 {
				return se(400, "invalid setting value")
			}
		default:
			return se(400, "unsupported setting")
		}
	}
	return nil
}
func readSettingsOverlay(path string) ([]byte, map[string]json.RawMessage, error) {
	empty := map[string]json.RawMessage{}
	for _, dir := range []string{filepath.Dir(filepath.Dir(path)), filepath.Dir(path)} {
		if e := settingsDirectory(dir, dir == filepath.Dir(path)); e != nil {
			if errors.Is(e, os.ErrNotExist) {
				return nil, empty, nil
			}
			return nil, nil, e
		}
	}
	st, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, empty, nil
	}
	if e != nil {
		return nil, nil, e
	}
	if st.Mode() != 0600 {
		return nil, nil, errors.New("invalid settings file")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, nil, e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 4097))
	if e != nil || len(raw) > 4096 {
		return nil, nil, errors.New("invalid settings file")
	}
	v, e := settingsJSON(raw)
	values, ok := v.(map[string]any)
	if e != nil || !ok || validateSettings(values) != nil {
		return nil, nil, errors.New("invalid settings file")
	}
	for k, v := range values {
		empty[k], _ = json.Marshal(v)
	}
	return raw, empty, nil
}

// LoadSettingsOverlay validates state before runtime/listener construction.
// A missing overlay is normal; all other failures are redacted and fail closed.
func LoadSettingsOverlay(basePath string) ([]byte, error) {
	raw, _, e := readSettingsOverlay(settingsPath(basePath))
	if e != nil {
		return nil, errors.New("configuration settings startup failed")
	}
	return raw, nil
}
func OpenSettings(path, name string, concurrency int) (*Settings, error) {
	base, e := os.ReadFile(path)
	if e != nil {
		return nil, errors.New("configuration unavailable")
	}
	v, e := settingsJSON(base)
	config, ok := v.(map[string]any)
	if e != nil || !ok {
		return nil, errors.New("invalid configuration")
	}
	selected := map[string]any{}
	for _, k := range []string{"service_name", "public_limit_concurrency"} {
		if v, ok := config[k]; ok {
			selected[k] = v
		}
	}
	saved := map[string]json.RawMessage{}
	for k, v := range selected {
		saved[k], _ = json.Marshal(v)
	}
	overlayPath := settingsPath(path)
	if prepareSettingsDirectory(overlayPath) != nil {
		return nil, errors.New("configuration settings startup failed")
	}
	raw, overlay, e := readSettingsOverlay(overlayPath)
	if e != nil {
		return nil, errors.New("configuration settings startup failed")
	}
	for k, v := range overlay {
		saved[k] = v
	}
	return &Settings{path: overlayPath, basePath: path, baseRaw: base, raw: raw, overlay: overlay, saved: saved, revision: revision(), name: name, concurrency: concurrency, syncFile: func(f *os.File) error { return f.Sync() }}, nil
}
func (s *Settings) viewLocked() (map[string]any, error) {
	if s.uncertain {
		return nil, se(503, "configuration durability uncertain; restart required")
	}
	values := map[string]any{"service_name": "inference", "public_limit_concurrency": 64}
	for k := range values {
		if v, ok := s.saved[k]; ok {
			var x any
			d := json.NewDecoder(bytes.NewReader(v))
			d.UseNumber()
			_ = d.Decode(&x)
			values[k] = x
		}
	}
	effective := map[string]any{"service_name": s.name, "public_limit_concurrency": s.concurrency}
	restart := []string{}
	for _, k := range []string{"service_name", "public_limit_concurrency"} {
		a, _ := json.Marshal(values[k])
		b, _ := json.Marshal(effective[k])
		if !bytes.Equal(a, b) {
			restart = append(restart, k)
		}
	}
	return map[string]any{"schema_version": 1, "runtime": "go", "revision": s.revision, "fields": map[string]any{
		"service_name":             map[string]any{"type": "string", "apply": "live", "minimum": nil, "maximum": nil, "default": "inference"},
		"public_limit_concurrency": map[string]any{"type": "integer", "apply": "restart", "minimum": 1, "maximum": 4096, "default": 64}}, "values": values, "effective": effective, "restart_required": restart}, nil
}
func (s *Settings) View() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked()
}
func (a *App) serviceName() string {
	if a.Settings == nil {
		return a.Config.ServiceName
	}
	s := a.Settings
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}
func validName(v any) bool {
	x, ok := v.(string)
	if !ok || !utf8.ValidString(x) || utf8.RuneCountInString(x) < 1 || utf8.RuneCountInString(x) > 80 {
		return false
	}
	for _, r := range x {
		if (!unicode.IsPrint(r) || (r != ' ' && unicode.Is(unicode.Zs, r))) || strings.ContainsRune("<>&\"'", r) {
			return false
		}
	}
	return true
}
func (s *Settings) Patch(body map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, e := s.viewLocked(); e != nil {
		return nil, e
	}
	changes, ok := body["changes"].(map[string]any)
	_, hasRevision := body["revision"]
	if len(body) != 2 || !hasRevision || !ok || len(changes) == 0 {
		return nil, se(400, "expected revision and nonempty changes object")
	}
	rev, _ := body["revision"].(string)
	if rev != s.revision {
		return nil, se(409, "configuration changed; reload or restart before editing")
	}
	current, _, e := readSettingsOverlay(s.path)
	base, be := os.ReadFile(s.basePath)
	if e != nil || be != nil {
		return nil, se(503, "configuration persistence failed; reload or restart required")
	}
	if !bytes.Equal(current, s.raw) || !bytes.Equal(base, s.baseRaw) {
		return nil, se(409, "configuration changed; reload or restart before editing")
	}
	if e := validateSettings(changes); e != nil {
		return nil, e
	}
	saved := make(map[string]json.RawMessage, len(s.saved))
	for k, v := range s.saved {
		saved[k] = v
	}
	overlay := make(map[string]json.RawMessage, len(s.overlay))
	for k, v := range s.overlay {
		overlay[k] = v
	}
	for k, v := range changes {
		saved[k], _ = json.Marshal(v)
		overlay[k] = saved[k]
	}
	raw, e := json.MarshalIndent(overlay, "", "  ")
	if e != nil {
		return nil, se(503, "configuration persistence failed; reload or restart required")
	}
	raw = append(raw, '\n')
	renamed, e := s.persist(raw)
	if e != nil {
		s.uncertain = renamed
		return nil, se(503, "configuration persistence failed; reload or restart required")
	}
	s.saved, s.overlay, s.raw, s.revision = saved, overlay, raw, revision()
	if v, ok := changes["service_name"]; ok {
		s.name = v.(string)
	}
	return s.viewLocked()
}
func (s *Settings) persist(raw []byte) (renamed bool, err error) {
	f, e := os.CreateTemp(filepath.Dir(s.path), ".nocturne-settings-")
	if e != nil {
		return false, e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(raw)
	}
	if e == nil {
		e = s.syncFile(f)
	}
	if e == nil {
		e = f.Close()
	}
	if e != nil {
		return false, e
	}
	if e = os.Rename(f.Name(), s.path); e != nil {
		return false, e
	}
	d, e := os.Open(filepath.Dir(s.path))
	if e != nil {
		return true, e
	}
	defer d.Close()
	return true, s.syncFile(d)
}
func settingsBrowserGuard(r *http.Request) bool {
	site, origin := r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Origin")
	if site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin != "" {
		u, e := url.Parse(origin)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return false
		}
	}
	return true
}

// Decode tokens recursively so duplicate keys at any nesting depth are rejected.
// encoding/json substitutes unpaired UTF-16 surrogates with U+FFFD. Python
// retains them and its printable-string validation rejects them; fail closed
// before substitution instead of silently saving a different display name.
func validJSONSurrogates(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func settingsJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) || !validJSONSurrogates(raw) {
		return nil, errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func() (any, error)
	value = func() (any, error) {
		t, e := d.Token()
		if e != nil {
			return nil, e
		}
		switch t {
		case json.Delim('{'):
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				key, ok := k.(string)
				if !ok {
					return nil, errors.New("invalid key")
				}
				if _, ok = m[key]; ok {
					return nil, errors.New("duplicate key")
				}
				v, e := value()
				if e != nil {
					return nil, e
				}
				m[key] = v
			}
			_, e = d.Token()
			return m, e
		case json.Delim('['):
			a := []any{}
			for d.More() {
				v, e := value()
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, e = d.Token()
			return a, e
		default:
			return t, nil
		}
	}
	v, e := value()
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return v, nil
}
func (a *App) adminSettings(w http.ResponseWriter, r *http.Request) {
	if !settingsBrowserGuard(r) {
		reject(w, 403, "cross-origin configuration access denied")
		return
	}
	if a.Settings == nil {
		reject(w, 503, "configuration unavailable")
		return
	}
	var v map[string]any
	var e error
	if r.Method == "GET" {
		v, e = a.Settings.View()
	} else {
		if strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])) != "application/json" {
			reject(w, 415, "application/json required")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if len(raw) > 4096 {
			reject(w, 413, "configuration patch too large")
			return
		}
		if err != nil {
			reject(w, 400, "invalid JSON")
			return
		}
		b, err := settingsJSON(raw)
		if err != nil {
			reject(w, 400, "invalid JSON")
			return
		}
		body, ok := b.(map[string]any)
		if !ok {
			reject(w, 400, "expected object")
			return
		}
		v, e = a.Settings.Patch(body)
	}
	if e != nil {
		var se *settingsError
		if errors.As(e, &se) {
			reject(w, se.status, se.detail)
		} else {
			reject(w, 503, "configuration unavailable")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	adminReply(w, 200, v)
}
