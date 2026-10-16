package tunnelkeys

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Prog is the program name used in usage/error text.
const Prog = "nocturne sync-tunnel-keys"

const usageLine = "usage: " + Prog + " [-h] [--machines MACHINES]\n" +
	"       [--authorized-keys AUTHORIZED_KEYS] [--port PORT] [--dry-run] [--settle-seconds SECONDS]\n"

const helpText = usageLine + `
options:
  -h, --help            show this help message and exit
  --machines MACHINES
  --authorized-keys AUTHORIZED_KEYS
  --port PORT
  --dry-run
  --settle-seconds SECONDS  minimum coalescing delay (0..30 seconds)
`

// Options are the parsed command-line flags.
type Options struct {
	Machines       string
	AuthorizedKeys string
	Port           int
	DryRun         bool
	SettleSeconds  float64
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

var longOpts = []string{"--machines", "--authorized-keys", "--port", "--dry-run", "--help", "--settle-seconds"}

// resolve mirrors argparse's exact-or-unique-prefix matching of long options.
func resolve(name string) (string, error) {
	var hits []string
	for _, o := range longOpts {
		if o == name {
			return o, nil
		}
		if strings.HasPrefix(o, name) {
			hits = append(hits, o)
		}
	}
	switch len(hits) {
	case 0:
		return "", nil
	case 1:
		return hits[0], nil
	}
	return "", usageError{fmt.Sprintf("ambiguous option: %s could match %s", name, strings.Join(hits, ", "))}
}

func defaults() Options {
	home, _ := os.UserHomeDir()
	return Options{
		Machines:       filepath.Join(home, "nocturne/data/machines.json"),
		AuthorizedKeys: filepath.Join(home, ".ssh/authorized_keys"),
		Port:           8701,
	}
}

// Parse parses args like the Python tool's argparse setup. help=true means -h was given.
func Parse(args []string) (o Options, help bool, err error) {
	o = defaults()
	var unknown []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" {
			return o, true, nil
		}
		if !strings.HasPrefix(a, "--") || a == "--" {
			unknown = append(unknown, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		opt, err := resolve(name)
		if err != nil {
			return o, false, err
		}
		switch opt {
		case "":
			unknown = append(unknown, a)
			continue
		case "--help":
			return o, true, nil
		case "--dry-run":
			if hasVal {
				return o, false, usageError{"argument --dry-run: ignored explicit argument '" + val + "'"}
			}
			o.DryRun = true
			continue
		}
		if !hasVal {
			if i+1 >= len(args) || (strings.HasPrefix(args[i+1], "-") && len(args[i+1]) > 1) {
				return o, false, usageError{"argument " + opt + ": expected one argument"}
			}
			i++
			val = args[i]
		}
		switch opt {
		case "--machines":
			o.Machines = val
		case "--authorized-keys":
			o.AuthorizedKeys = val
		case "--settle-seconds":
			n, perr := strconv.ParseFloat(pyStrip(val), 64)
			if perr != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 30 {
				return o, false, usageError{"settle-seconds must be 0..30"}
			}
			o.SettleSeconds = n
		case "--port":
			n, perr := strconv.Atoi(pyStrip(val))
			if perr != nil {
				return o, false, usageError{"argument --port: invalid int value: '" + val + "'"}
			}
			o.Port = n
		}
	}
	if len(unknown) > 0 {
		return o, false, usageError{"unrecognized arguments: " + strings.Join(unknown, " ")}
	}
	return o, false, nil
}

func usageFail(stderr io.Writer, err error) int {
	fmt.Fprint(stderr, usageLine)
	fmt.Fprintf(stderr, "%s: error: %s\n", Prog, err)
	return 2
}
