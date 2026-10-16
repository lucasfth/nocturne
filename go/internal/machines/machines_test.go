package machines

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// PUB/PUB2 from tests/test_gateway.py.
const (
	PUB  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIM+qPbR64P1oIeDurt040Y6ZXJRC2DTnqD2RHVAsq2wu"
	PUB2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEbdGkLkLVGxaPpCchQ65W1S1NJrbVxfk74UyyAFa06L"
)

func H(s string) string { d := sha256.Sum256([]byte(s)); return hex.EncodeToString(d[:]) }

func TestMigrationImportsTunnelKeysAndRefusesBadIDs(t *testing.T) {
	dir := t.TempDir()
	ak := filepath.Join(dir, "ak")
	os.WriteFile(ak, []byte(`restrict,permitopen="127.0.0.1:8701" `+PUB+" nocturne-tunnel-mac-a\n"), 0o600)
	m, err := Open(dir, Legacy{
		Secrets: map[string]any{"mac-a": H("a")},
		Meta:    map[string]map[string]any{"mac-a": {"owner": "A", "ip": "fe80::1%eth0", "junk": 1}},
		Keys:    LegacyTunnelKeys(ak),
	})
	if err != nil {
		t.Fatal(err)
	}
	it := m.items["mac-a"]
	if it.TunnelPubkey != PUB || len(it.Meta) != 0 {
		t.Fatalf("got %+v", it)
	}
	x := filepath.Join(dir, "x")
	if _, err := Open(x, Legacy{Secrets: map[string]any{"Bad_ID": H("b")}}); err == nil {
		t.Fatal("bad id accepted")
	}
	if _, err := Open(x, Legacy{Secrets: map[string]any{"ok": "ABC"}}); err == nil {
		t.Fatal("bad digest accepted")
	}
	if _, err := os.Stat(filepath.Join(x, "machines.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("machines.json written on failed migration")
	}
	// Bad pubkey becomes null; good meta kept.
	m2, err := Open(filepath.Join(dir, "y"), Legacy{
		Secrets: map[string]any{"b": H("b")},
		Meta:    map[string]map[string]any{"b": {"owner": " Bo ", "user": "bo"}},
		Keys:    map[string]string{"b": "ssh-rsa AAAA"},
	})
	if err != nil || m2.items["b"].TunnelPubkey != "" || !reflect.DeepEqual(m2.items["b"].Meta, map[string]string{"owner": "Bo", "user": "bo"}) {
		t.Fatalf("%v %+v", err, m2.items["b"])
	}
}

func TestMetaRejectsNewlineAndScope(t *testing.T) {
	for _, bad := range []map[string]any{{"user": "anna\n"}, {"host": "a.local\n"}, {"ip": "fe80::1%$(id)"},
		{"ip": "fe80::1%eth0"}, {"host": strings.Repeat("a.", 127)}, {"owner": "x\x7f"}, {"evil": "1"}, {"user": 5}} {
		if _, err := CleanMeta(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	got, err := CleanMeta(map[string]any{"host": "Annas-Air.local.", "ip": "::FFFF:1.2.3.4", "jump": "j@x.y", "owner": ""})
	if err != nil || got["ip"] != "::ffff:1.2.3.4" || got["owner"] != nil {
		t.Fatalf("%v %v", got, err)
	}
}
