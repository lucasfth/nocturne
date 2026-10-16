// Package tunnelkeys is a port of tools/sync-tunnel-keys.py: it installs worker
// tunnel keys from data/machines.json into ~/.ssh/authorized_keys inside a
// managed block, each with a fixed port-forwarding-only restriction.
package tunnelkeys

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	Begin = "# >>> nocturne tunnel keys (managed, do not edit) >>>"
	End   = "# <<< nocturne tunnel keys <<<"
)

// Machines is the decoded registry: id -> machine object.
type Machines map[string]map[string]any

// truthy mirrors Python truthiness for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func sortedIDs(ms Machines) []string {
	ids := make([]string, 0, len(ms))
	for id := range ms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Render returns the managed authorized_keys lines; skipped invalid keys are reported on stderr.
func Render(ms Machines, port int, falsePath string, stderr io.Writer) ([]string, error) {
	var out []string
	for _, wid := range sortedIDs(ms) {
		m := ms[wid]
		pubV := m["tunnel_pubkey"]
		enabled, ok := m["enabled"]
		if !ok {
			enabled = true
		}
		if !idRe.MatchString(wid) || !truthy(enabled) || !truthy(pubV) {
			continue
		}
		pub, isStr := pubV.(string)
		if !isStr {
			return nil, fmt.Errorf("machine %s: tunnel_pubkey is not a string", wid)
		}
		if !Valid(pub) {
			fmt.Fprintf(stderr, "skip %s: invalid key\n", wid)
			continue
		}
		out = append(out, fmt.Sprintf(`restrict,port-forwarding,permitopen="127.0.0.1:%d",permitlisten="none",command="%s" %s nocturne-tunnel-%s`, port, falsePath, pub, wid))
	}
	return out, nil
}

// AdoptLegacy returns {id: pubkey} for old unmanaged tunnel lines whose machine has no key yet.
func AdoptLegacy(text string, ms Machines) map[string]string {
	found := map[string]string{}
	for _, line := range pySplitlines(text) {
		g := legacyRe.FindStringSubmatch(line)
		if g == nil || !strings.Contains(line, "permitopen=") {
			continue
		}
		m, ok := ms[g[2]]
		if ok && !truthy(m["tunnel_pubkey"]) && Valid(g[1]) {
			found[g[2]] = g[1]
		}
	}
	return found
}

// Merge replaces the managed block in text and drops legacy lines for managed ids.
func Merge(text string, block []string, managed map[string]bool) string {
	var keep []string
	inside := false
	for _, line := range pySplitlines(text) {
		switch {
		case line == Begin:
			inside = true
			continue
		case line == End:
			inside = false
			continue
		case inside:
			continue
		}
		if g := mergeRe.FindStringSubmatch(line); g != nil && managed[g[1]] && strings.Contains(line, "permitopen=") {
			continue
		}
		keep = append(keep, line)
	}
	for len(keep) > 0 && pyStrip(keep[len(keep)-1]) == "" {
		keep = keep[:len(keep)-1]
	}
	lines := keep
	if len(keep) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, Begin)
	lines = append(lines, block...)
	lines = append(lines, End)
	return strings.Join(lines, "\n") + "\n"
}
