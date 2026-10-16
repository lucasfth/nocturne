package machines

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestMachineRegistryCRUD(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir, Legacy{Secrets: map[string]any{"w": H("worker")}})
	if err != nil {
		t.Fatal(err)
	}
	if l := m.List(); len(l) != 1 || l[0].WorkerID != "w" {
		t.Fatalf("migrated list %+v", l)
	}
	for _, bad := range []map[string]any{
		{"worker_id": "Bad!", "secret_sha256": H("x")}, {"worker_id": "m2", "secret_sha256": "zz"},
		{"worker_id": "m2", "secret_sha256": H("x"), "tunnel_pubkey": "ssh-rsa AAAA"},
		{"worker_id": "m2", "secret_sha256": H("x"), "meta": map[string]any{"host": "a;rm -rf /"}},
		{"worker_id": "m2", "secret_sha256": H("x"), "meta": map[string]any{"evil": "1"}},
		{"worker_id": "m2", "secret_sha256": H("worker")}, // secret reuse
		{"worker_id": "m2", "secret_sha256": H("x"), "enabled": "yes"},
		{"worker_id": "m2", "secret_sha256": H("x"), "extra": 1},
	} {
		if _, err := m.Add(bad); err == nil || errors.Is(err, ErrExists) {
			t.Errorf("Add(%v) = %v", bad, err)
		}
	}
	p, err := m.Add(map[string]any{"worker_id": "m2", "secret_sha256": strings.ToUpper(H("s2")), "tunnel_pubkey": PUB + " comment",
		"meta": map[string]any{"owner": "Anna", "host": "mac-example.local", "ip": "192.0.2.9", "user": "example"}})
	if err != nil || p.TunnelKeyFingerprint == nil || !strings.HasPrefix(*p.TunnelKeyFingerprint, "SHA256:") || p.Meta["owner"] != "Anna" {
		t.Fatalf("%+v %v", p, err)
	}
	if !m.CheckSecret("m2", H("s2")) || m.CheckSecret("m2", H("x")) || m.CheckSecret("nope", strings.Repeat("0", 64)) {
		t.Fatal("CheckSecret (digest must be lowercased on add)")
	}
	if _, err := m.Add(map[string]any{"worker_id": "m2", "secret_sha256": H("s3")}); !errors.Is(err, ErrExists) {
		t.Fatalf("dup id: %v", err)
	}
	if _, err := m.Add(map[string]any{"worker_id": "m3", "secret_sha256": H("s3"), "tunnel_pubkey": PUB}); err == nil {
		t.Fatal("dup tunnel key accepted")
	}
	if _, err := m.Update("w", map[string]any{"tunnel_pubkey": PUB2}); err != nil {
		t.Fatal(err)
	}
	if p, _ := m.Update("m2", map[string]any{"enabled": false}); p.Enabled || m.CheckSecret("m2", H("s2")) {
		t.Fatal("disable")
	}
	p, err = m.Update("m2", map[string]any{"enabled": true, "meta": map[string]any{"owner": nil, "agent": "Max"}})
	want := map[string]string{"host": "mac-example.local", "ip": "192.0.2.9", "user": "example", "agent": "Max"}
	if err != nil || !reflect.DeepEqual(p.Meta, want) {
		t.Fatalf("meta merge %+v %v", p.Meta, err)
	}
	if _, err := m.Update("m2", map[string]any{}); err == nil {
		t.Fatal("empty update")
	}
	if _, err := m.Update("zz", map[string]any{"enabled": true}); !errors.Is(err, ErrNotFound) {
		t.Fatal("update unknown")
	}
	if err := m.Delete("m2"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("m2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if m.CheckSecret("m2", H("s2")) {
		t.Fatal("deleted machine still authenticates")
	}
	fi, err := os.Stat(m.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
	raw, _ := os.ReadFile(m.Path)
	if strings.Contains(string(raw), "s2") {
		t.Fatal("secret in file")
	}
	m2, err := Open(dir, Legacy{})
	if err != nil || len(m2.List()) != 1 || m2.List()[0].WorkerID != "w" || m2.List()[0].TunnelKeyFingerprint == nil {
		t.Fatalf("reload %v", err)
	}
}
