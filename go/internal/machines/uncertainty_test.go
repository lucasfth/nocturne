package machines

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestMutationDurabilityFailures(t *testing.T) {
	mutations := map[string]func(*Machines) error{
		"disable":  func(m *Machines) error { _, e := m.Update("w", map[string]any{"enabled": false}); return e },
		"delete":   func(m *Machines) error { return m.Delete("w") },
		"rotation": func(m *Machines) error { _, e := m.Update("w", map[string]any{"secret_sha256": H("new")}); return e },
		"add": func(m *Machines) error {
			_, e := m.Add(map[string]any{"worker_id": "new", "secret_sha256": H("new")})
			return e
		},
	}
	for name, mutate := range mutations {
		for _, afterRename := range []bool{false, true} {
			stage := "before-rename"
			if afterRename {
				stage = "directory-sync"
			}
			t.Run(name+"/"+stage, func(t *testing.T) {
				dir := t.TempDir()
				m, err := Open(dir, Legacy{Secrets: map[string]any{"w": H("old"), "other": H("other")}})
				if err != nil {
					t.Fatal(err)
				}
				originalPath := m.Path
				before, err := os.ReadFile(originalPath)
				if err != nil {
					t.Fatal(err)
				}
				snapshot := m.List()
				syncCalled := false
				m.syncDir = func(string) error { syncCalled = true; return errors.New("private path and secret must not escape") }
				if !afterRename {
					// A nonempty directory forces rename to fail after the temp file was synced.
					m.Path = filepath.Join(dir, "blocked")
					if err := os.Mkdir(m.Path, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(m.Path, "entry"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				woke := make(chan struct{})
				stop := make(chan struct{})
				go func() {
					defer close(woke)
					select {
					case <-m.Failed():
					case <-stop:
					}
				}()
				defer close(stop)
				err = mutate(m)
				if err == nil {
					t.Fatal("mutation unexpectedly succeeded")
				}
				if !reflect.DeepEqual(snapshot, m.List()) {
					t.Fatal("failed mutation published candidate")
				}
				disk, readErr := os.ReadFile(originalPath)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !afterRename {
					if syncCalled || errors.Is(err, ErrUnavailable) || !m.Available() || !m.CheckSecret("w", H("old")) || !m.Enabled("w") {
						t.Fatal("pre-rename failure poisoned registry")
					}
					if string(before) != string(disk) {
						t.Fatal("pre-rename failure changed disk")
					}
					select {
					case <-m.Failed():
						t.Fatal("pre-rename failure signaled uncertainty")
					default:
					}
					m.Path = originalPath
					m.syncDir = syncDirectory
					if err := mutate(m); err != nil {
						t.Fatal(err)
					}
					return
				}
				if !syncCalled || err != ErrUnavailable || m.Available() {
					t.Fatalf("not sanitized/unavailable: %v", err)
				}
				if string(before) == string(disk) {
					t.Fatal("fault did not occur after rename")
				}
				select {
				case <-woke:
				case <-time.After(time.Second):
					t.Fatal("waiting lease not notified")
				}
				// All identities fail closed, not just the mutation's target; concurrent
				// reads and rejected writes must not race or revive credentials.
				var wg sync.WaitGroup
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if m.Secret("w") != "" || m.Enabled("w") || m.CheckSecret("w", H("old")) || m.CheckSecret("w", H("new")) || m.CheckSecret("other", H("other")) {
							t.Error("uncertain registry authorized worker")
						}
						if _, e := m.Add(nil); e != ErrUnavailable {
							t.Error("add not blocked")
						}
						if _, e := m.Update("missing", nil); e != ErrUnavailable {
							t.Error("update not blocked")
						}
						if e := m.Delete("missing"); e != ErrUnavailable {
							t.Error("delete not blocked")
						}
					}()
				}
				wg.Wait()
				unchanged, e := os.ReadFile(originalPath)
				if e != nil || string(unchanged) != string(disk) {
					t.Fatal("poisoned mutation wrote disk")
				}
				reopened, e := Open(dir, Legacy{})
				if e != nil || !reopened.Available() {
					t.Fatal("restart failed", e)
				}
				switch name {
				case "disable", "delete":
					if reopened.CheckSecret("w", H("old")) {
						t.Fatal("restart revived revoked secret")
					}
				case "rotation":
					if reopened.CheckSecret("w", H("old")) || !reopened.CheckSecret("w", H("new")) {
						t.Fatal("restart did not load rotation")
					}
				}
			})
		}
	}
}

func TestInvalidPersistedIDCannotAuthenticate(t *testing.T) {
	m, err := Open(t.TempDir(), Legacy{})
	if err != nil {
		t.Fatal(err)
	}
	m.items["invalid/id"] = Machine{Enabled: true, SecretSHA256: H("secret")}
	m.index()
	if m.CheckSecret("invalid/id", H("secret")) || m.Enabled("invalid/id") {
		t.Fatal("invalid persisted ID authorized")
	}
}

func TestKnownSecretPoisonedRegistry(t *testing.T) {
	m := &Machines{unavailable: true, secretDigests: map[string]bool{H("known"): true}}
	for _, d := range []string{H("known"), H("unknown")} {
		if ok, err := m.KnownSecret(d); ok || err != ErrUnavailable {
			t.Fatal("poisoned registry admitted credential")
		}
	}
}
