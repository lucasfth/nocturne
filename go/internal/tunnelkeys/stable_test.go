package tunnelkeys

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStableRegistryRejectsInvalid(t *testing.T) {
	for _, raw := range []string{`{`, `null`, `[]`, `{} {}`, `{"Bad":{}}`, `{"a":{"enabled":1}}`, `{"a":{"enabled":null}}`, `{"a":{"tunnel_pubkey":4}}`, `{"a":null}`} {
		t.Run(raw, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "machines.json")
			if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := stableRegistry(p, time.Millisecond, 8*time.Millisecond); err == nil {
				t.Fatal("accepted invalid registry")
			}
		})
	}
}

func TestStableRegistryPartialThenAtomicReplacement(t *testing.T) {
	p := filepath.Join(t.TempDir(), "machines.json")
	if err := os.WriteFile(p, []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		tmp := p + ".new"
		if err := os.WriteFile(tmp, []byte(`{"a":{"enabled":false}}`), 0600); err != nil {
			done <- err
			return
		}
		done <- os.Rename(tmp, p)
	}()
	ms, err := stableRegistry(p, 5*time.Millisecond, time.Second)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if err != nil || ms["a"]["enabled"] != false {
		t.Fatalf("snapshot=%v err=%v", ms, err)
	}
}

func TestSyncInvalidPreservesKeysAndRedacts(t *testing.T) {
	dir := t.TempDir()
	mp, ak := fixture(t, dir)
	old, err := os.ReadFile(ak)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp, []byte(`{"a":{"enabled":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if Main([]string{"--machines", mp, "--authorized-keys", ak}, &out, &stderr) != 1 {
		t.Fatal("invalid registry succeeded")
	}
	got, _ := os.ReadFile(ak)
	if !bytes.Equal(old, got) || out.Len() != 0 {
		t.Fatal("changed keys on failure")
	}
	if strings.Contains(stderr.String(), dir) || stderr.String() != "tunnel key synchronization failed; check registry and filesystem\n" {
		t.Fatal("unredacted error")
	}
	temps, _ := filepath.Glob(filepath.Join(dir, ".authorized_keys.*"))
	if len(temps) != 0 {
		t.Fatal("temporary files retained")
	}
}

func TestSettleFlagAndDelay(t *testing.T) {
	for _, v := range []string{"-1", "31", "NaN", "Inf", "oops"} {
		if _, _, err := Parse([]string{"--settle-seconds=" + v}); err == nil {
			t.Fatalf("accepted %s", v)
		}
	}
	dir := t.TempDir()
	mp, ak := fixture(t, dir)
	o, _, err := Parse([]string{"--machines", mp, "--authorized-keys", ak, "--settle-seconds=0.025"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := Run(o, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 225*time.Millisecond {
		t.Fatal("missing coalescing and stable-snapshot delay")
	}
	temps, _ := filepath.Glob(filepath.Join(dir, ".authorized_keys.*"))
	if len(temps) != 0 {
		t.Fatal("temporary files retained")
	}
}

func TestStableRegistryRejectsContinuousEqualByteReplacements(t *testing.T) {
	p := filepath.Join(t.TempDir(), "machines.json")
	if err := os.WriteFile(p, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			tmp := p + ".new"
			if err := os.WriteFile(tmp, []byte(`{}`), 0600); err != nil {
				done <- err
				return
			}
			if err := os.Rename(tmp, p); err != nil {
				done <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_, err := stableRegistry(p, 20*time.Millisecond, 100*time.Millisecond)
	close(stop)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if err == nil {
		t.Fatal("accepted continuously replaced registry")
	}
}

func TestAtomicWriteFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "directory")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(dir, destination, "keys"); err == nil {
		t.Fatal("replaced directory")
	}
	temps, _ := filepath.Glob(filepath.Join(dir, ".authorized_keys.*"))
	if len(temps) != 0 {
		t.Fatal("temporary file leaked")
	}
}

// encoding/json repairs invalid UTF-8; a registry must instead fail closed like
// the Python reference, even when the invalid byte is in unused metadata.
func TestSyncInvalidUTF8PreservesKeysAndInode(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("{\"a\":{\"note\":\"\xff\"}}"),
		[]byte("{\"a\":{\"tunnel_pubkey\":\"ssh-ed25519 \xff\"}}"),
	} {
		t.Run(string(raw), func(t *testing.T) {
			dir := t.TempDir()
			mp, ak := fixture(t, dir)
			old, err := os.ReadFile(ak)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(ak)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mp, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			if code := Main([]string{"--machines", mp, "--authorized-keys", ak}, &out, &stderr); code != 1 {
				t.Fatalf("exit=%d", code)
			}
			after, err := os.Stat(ak)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(ak)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(old, got) || !os.SameFile(before, after) {
				t.Fatal("changed authorized_keys bytes or inode")
			}
			if out.Len() != 0 || stderr.String() != "tunnel key synchronization failed; check registry and filesystem\n" {
				t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".authorized_keys.*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("temporary files=%v err=%v", temps, err)
			}
		})
	}
}
