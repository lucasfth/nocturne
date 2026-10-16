package tunnelkeys

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type scenario struct {
	name     string
	machines string // "" = keep the test_tunnel_key_sync fixture
	ak       *string
	args     []string
}

func strp(s string) *string { return &s }

var scenarios = []scenario{
	{name: "basic"},
	{name: "basic-dry", args: []string{"--dry-run"}},
	{name: "port", args: []string{"--port", "9001"}},
	{name: "legacy", machines: `{"d": {"enabled": true}, "e": {"tunnel_pubkey": "` + PUB + `"}}`,
		ak: strp("# hi\r\n\n" + `restrict,permitopen="127.0.0.1:8701" ` + PUB2 + " nocturne-tunnel-d  \n\n\n")},
	{name: "legacy-dry", machines: `{"d": {}}`, args: []string{"--dry-run"},
		ak: strp(`x,permitopen="1" ` + PUB2 + " nocturne-tunnel-d\n")},
	{name: "no-ak", machines: `{"a": {"tunnel_pubkey": "` + PUB + `", "enabled": 1}, "Bad": {"tunnel_pubkey": "` + PUB + `"}}`},
	{name: "unchanged", machines: `{}`, ak: strp(Begin + "\n" + End + "\n")},
	{name: "missing-machines", machines: "-"},
	{name: "bad-port", args: []string{"--port", "x"}},
	{name: "unknown-flag", args: []string{"--nope"}},
}

func setup(t *testing.T, sc scenario) (dir string, args []string) {
	dir = t.TempDir()
	mp, ak := fixture(t, dir)
	switch sc.machines {
	case "":
	case "-":
		os.Remove(mp)
	default:
		os.WriteFile(mp, []byte(sc.machines), 0o644)
	}
	if sc.name == "no-ak" {
		os.Remove(ak)
	} else if sc.ak != nil {
		os.WriteFile(ak, []byte(*sc.ak), 0o600)
	}
	return dir, append([]string{"--machines", mp, "--authorized-keys", ak}, sc.args...)
}

func readAK(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "authorized_keys"))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// TestCrossCheckPython runs tools/sync-tunnel-keys.py and the Go port on identical
// fixtures and requires identical exit codes, stdout and authorized_keys bytes.
func TestCrossCheckPython(t *testing.T) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		home, _ := os.UserHomeDir()
		uv = filepath.Join(home, ".local", "bin", "uv")
		if _, err := os.Stat(uv); err != nil {
			t.Skip("uv not installed")
		}
	}
	root, _ := filepath.Abs("../../..")
	tool := filepath.Join(root, "tools", "sync-tunnel-keys.py")
	if _, err := os.Stat(tool); err != nil {
		t.Skip("repo root not found")
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			pyDir, pyArgs := setup(t, sc)
			goDir, goArgs := setup(t, sc)
			cmd := exec.Command(uv, append([]string{"run", "--frozen", "python", tool}, pyArgs...)...)
			cmd.Dir = root
			var pyOut, pyErr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &pyOut, &pyErr
			pyCode := 0
			if err := cmd.Run(); err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatal(err)
				}
				pyCode = ee.ExitCode()
			}
			var goOut, goErr bytes.Buffer
			goCode := Main(goArgs, &goOut, &goErr)
			if pyCode != goCode {
				t.Fatalf("exit py=%d go=%d\npy stderr: %s\ngo stderr: %s", pyCode, goCode, pyErr.String(), goErr.String())
			}
			if p, g := pyOut.String(), goOut.String(); p != g {
				t.Fatalf("stdout differs\npy: %q\ngo: %q", p, g)
			}
			if p, g := readAK(pyDir), readAK(goDir); p != g {
				t.Fatalf("authorized_keys differs\npy: %q\ngo: %q", p, g)
			}
			if pyCode == 0 && pyErr.String() != goErr.String() {
				t.Fatalf("stderr differs\npy: %q\ngo: %q", pyErr.String(), goErr.String())
			}
			if pyCode == 2 && !strings.Contains(goErr.String(), "error: ") {
				t.Fatalf("go usage error text %q", goErr.String())
			}
		})
	}
}
