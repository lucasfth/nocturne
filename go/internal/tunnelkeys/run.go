package tunnelkeys

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Main runs the subcommand and returns the process exit code
// (0 ok, 1 runtime error, 2 usage error).
func Main(args []string, stdout, stderr io.Writer) int {
	o, help, err := Parse(args)
	if help {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	if err != nil {
		return usageFail(stderr, err)
	}
	if err := Run(o, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "tunnel key synchronization failed; check registry and filesystem")
		return 1
	}
	return 0
}

// FalsePath mirrors shutil.which('false') or '/bin/false'.
func FalsePath() string {
	if p, err := exec.LookPath("false"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return "/bin/false"
}

// stableRegistry requires two matching complete snapshots separated by quiet.
// Descriptor and path identity catch atomic replacement even with equal bytes.
// Invalid or continuously changing input never reaches authorized_keys.
func stableRegistry(path string, quiet, timeout time.Duration) (Machines, error) {
	deadline := time.Now().Add(timeout)
	var previous []byte
	var previousInfo os.FileInfo
	for time.Now().Before(deadline) {
		f, err := os.Open(path)
		if err == nil {
			before, e1 := f.Stat()
			raw, e2 := io.ReadAll(f)
			after, e3 := f.Stat()
			f.Close()
			current, e4 := os.Stat(path)
			same := func(a, b os.FileInfo) bool {
				return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime() && sameChangeTime(a, b)
			}
			if e1 == nil && e2 == nil && e3 == nil && e4 == nil && same(before, after) && same(after, current) {
				if previousInfo != nil && same(after, previousInfo) && bytes.Equal(raw, previous) {
					if ms, err := decodeMachines(raw); err == nil {
						return ms, nil
					}
				}
				previous, previousInfo = raw, after
			} else {
				previous, previousInfo = nil, nil
			}
		} else {
			previous, previousInfo = nil, nil
		}
		time.Sleep(quiet)
	}
	return nil, errors.New("machine registry not stable and valid")
}

func decodeMachines(raw []byte) (Machines, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid registry encoding")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return nil, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || top == nil {
		return nil, errors.New("invalid registry")
	}
	ms := Machines{}
	for id, v := range top {
		m, ok := v.(map[string]any)
		if !ok || !idRe.MatchString(id) {
			return nil, errors.New("invalid machine")
		}
		if enabled, present := m["enabled"]; present {
			if _, ok := enabled.(bool); !ok {
				return nil, errors.New("invalid enabled")
			}
		}
		if pub := m["tunnel_pubkey"]; pub != nil {
			if _, ok := pub.(string); !ok {
				return nil, errors.New("invalid key type")
			}
		}
		ms[id] = m
	}
	return ms, nil
}

func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%s: not valid UTF-8", path)
	}
	return universalNewlines(string(b)), nil
}

// Run performs one sync under the .nocturne-tunnel-keys.lock flock.
func Run(o Options, stdout, stderr io.Writer) error {
	time.Sleep(time.Duration(o.SettleSeconds * float64(time.Second)))
	ak := o.AuthorizedKeys
	dir := filepath.Dir(ak)
	// One writer at a time (the path unit can fire while a manual run is going).
	lock, err := os.OpenFile(filepath.Join(dir, ".nocturne-tunnel-keys.lock"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	ms, err := stableRegistry(o.Machines, 200*time.Millisecond, 2*time.Second)
	if err != nil {
		return err
	}
	old, err := readText(ak)
	if err != nil {
		return err
	}
	legacy := AdoptLegacy(old, ms)
	for wid, pub := range legacy {
		m := make(map[string]any, len(ms[wid])+1)
		for k, v := range ms[wid] {
			m[k] = v
		}
		m["tunnel_pubkey"] = pub
		ms[wid] = m
	}
	block, err := Render(ms, o.Port, FalsePath(), stderr)
	if err != nil {
		return err
	}
	// Every registry machine owns its id, keyed or not, so a disabled/removed one is revoked.
	managed := make(map[string]bool, len(ms))
	for id := range ms {
		managed[id] = true
	}
	merged := Merge(old, block, managed)
	if o.DryRun {
		_, err := io.WriteString(stdout, merged)
		return err
	}
	if len(legacy) > 0 {
		ids := make([]string, 0, len(legacy))
		for id := range legacy {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Fprintf(stderr, "adopted legacy tunnel keys: %s; the gateway imports these on first start\n", strings.Join(ids, ", "))
	}
	if merged == old {
		return nil
	}
	if err := writeAtomic(dir, ak, merged); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed %d tunnel keys\n", len(block))
	return nil
}

func writeAtomic(dir, dst, content string) error {
	f, err := os.CreateTemp(dir, ".authorized_keys.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	ok = true
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
