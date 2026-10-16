package machines

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const pyScript = `
import sys, time
from gateway.machines import Machines
d = sys.argv[1]
m = Machines(d, {'mac-a': 'a' * 64, 'mac-b': 'b' * 64},
             {'mac-a': {'owner': '\u00c5nna \U0001F600 "q" \\ x', 'ip': '::FFFF:1.2.3.4', 'host': 'a.local'}})
m.add({'worker_id': 'm3', 'secret_sha256': 'C' * 64, 'tunnel_pubkey': sys.argv[2], 'enabled': False,
       'meta': {'agent': 'Max\u00e9', 'user': 'max', 'jump': 'j@h.example'}})
m.update('mac-b', {'meta': {'model': 'mini'}})
`

func TestPythonWrittenFileRoundTripsByteIdentical(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv not installed")
	}
	root, _ := filepath.Abs("../../..")
	if _, err := os.Stat(filepath.Join(root, "gateway", "machines.py")); err != nil {
		t.Skip("repo root not found")
	}
	dir := t.TempDir()
	cmd := exec.Command(uv, "run", "--frozen", "python", "-c", pyScript, dir, PUB)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	path := filepath.Join(dir, "machines.json")
	want, _ := os.ReadFile(path)
	m, err := Open(dir, Legacy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.save(m.items); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(want) {
		t.Fatalf("mismatch\npython:\n%s\ngo:\n%s", want, got)
	}
	for _, frag := range []string{`\u00c5nna \ud83d\ude00 \"q\" \\ x`, "\n \"mac-a\": {\n  \"added\": ", `"tunnel_pubkey": null`} {
		if !strings.Contains(string(want), frag) {
			t.Errorf("python output lacks %q", frag)
		}
	}
	if !m.CheckSecret("mac-a", repeat('a')) || m.Enabled("m3") {
		t.Fatal("loaded state wrong")
	}
}

func repeat(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
