package tunnelkeys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PUB/PUB2 from tests/test_gateway.py.
const (
	PUB  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIM+qPbR64P1oIeDurt040Y6ZXJRC2DTnqD2RHVAsq2wu"
	PUB2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEbdGkLkLVGxaPpCchQ65W1S1NJrbVxfk74UyyAFa06L"
)

func H(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// fixture writes the test_tunnel_key_sync registry and authorized_keys into dir.
func fixture(t *testing.T, dir string) (machinesPath, akPath string) {
	t.Helper()
	m := map[string]any{
		"a": map[string]any{"secret_sha256": H("a"), "enabled": true, "tunnel_pubkey": PUB},
		"b": map[string]any{"secret_sha256": H("b"), "enabled": false, "tunnel_pubkey": PUB},
		"c": map[string]any{"secret_sha256": H("c"), "enabled": true, "tunnel_pubkey": "ssh-ed25519 bad"},
	}
	raw, _ := json.Marshal(m)
	machinesPath, akPath = filepath.Join(dir, "machines.json"), filepath.Join(dir, "authorized_keys")
	ak := "ssh-ed25519 AAAAuser me@laptop\n" +
		`restrict,permitopen="127.0.0.1:8701" ssh-ed25519 OLD nocturne-tunnel-a` + "\n" +
		`restrict,permitopen="127.0.0.1:8701" ` + PUB + " nocturne-tunnel-b\n" +
		`restrict,permitopen="127.0.0.1:8701" ` + PUB2 + " nocturne-tunnel-other\n"
	if err := os.WriteFile(machinesPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(akPath, []byte(ak), 0o600); err != nil {
		t.Fatal(err)
	}
	return
}

// Port of test_tunnel_key_sync (tests/test_gateway.py).
func TestTunnelKeySync(t *testing.T) {
	dir := t.TempDir()
	mp, ak := fixture(t, dir)
	for i := 0; i < 2; i++ {
		var out, errb bytes.Buffer
		if code := Main([]string{"--machines", mp, "--authorized-keys", ak}, &out, &errb); code != 0 {
			t.Fatalf("run %d: exit %d stderr=%q", i, code, errb.String())
		}
		if i == 1 && out.Len() != 0 {
			t.Fatalf("second run should be silent, got %q", out.String())
		}
	}
	raw, _ := os.ReadFile(ak)
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if lines[0] != "ssh-ed25519 AAAAuser me@laptop" {
		t.Fatalf("first line %q", lines[0])
	}
	var managed []string
	other := false
	for _, x := range lines {
		if strings.Contains(x, "OLD") || strings.Contains(x, "tunnel-b") || strings.Contains(x, "tunnel-c") {
			t.Fatalf("unexpected line %q", x)
		}
		if strings.HasSuffix(x, "nocturne-tunnel-a") {
			managed = append(managed, x)
		}
		other = other || strings.HasSuffix(x, "nocturne-tunnel-other")
	}
	if len(managed) != 1 || !strings.HasPrefix(managed[0], `restrict,port-forwarding,permitopen="127.0.0.1:8701",permitlisten="none",command=`) {
		t.Fatalf("managed %q", managed)
	}
	if !other || !strings.Contains(managed[0], `permitlisten="none"`) {
		t.Fatal("other line missing or permitlisten absent")
	}
	if fi, _ := os.Stat(ak); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}
