package tunnelkeys

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// pyIsSpace mirrors Python's str.isspace / unicode re \s.
func pyIsSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0,
		0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// pySpace is a regexp class equivalent to Python's unicode \s.
const pySpace = `[\t\n\v\f\r \x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var (
	// Deviation: Python's ID.match also accepts one trailing "\n" before $;
	// Go's $ (without (?m)) only matches at end of text, so such ids are
	// rejected here. Rejecting is the safer behaviour.
	idRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	legacyRe = regexp.MustCompile(`(ssh-ed25519 [A-Za-z0-9+/=]+)` + pySpace + `+nocturne-tunnel-([a-z0-9-]+)` + pySpace + `*$`)
	mergeRe  = regexp.MustCompile(pySpace + `nocturne-tunnel-([a-z0-9-]+)` + pySpace + `*$`)
)

func pyFields(s string) []string { return strings.FieldsFunc(s, pyIsSpace) }

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pySplitlines mirrors str.splitlines() without keepends.
func pySplitlines(s string) []string {
	var out []string
	rs := []rune(s)
	start := 0
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(rs[start:i]))
			start = i + 1
		case '\r':
			out = append(out, string(rs[start:i]))
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		out = append(out, string(rs[start:]))
	}
	return out
}

// universalNewlines mirrors Path.read_text() newline translation.
func universalNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="

// Valid reports whether pub is a well-formed "ssh-ed25519 <base64>" key.
func Valid(pub string) bool {
	p := pyFields(pub)
	if len(p) != 2 || p[0] != "ssh-ed25519" {
		return false
	}
	if strings.Trim(p[1], b64Alphabet) != "" {
		return false
	}
	blob, err := base64.StdEncoding.DecodeString(p[1])
	if err != nil {
		return false
	}
	return len(blob) == 51 && string(blob[:15]) == "\x00\x00\x00\x0bssh-ed25519"
}
