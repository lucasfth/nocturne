package machines

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"regexp"
	"strings"
)

// ParsePubkey ports parse_pubkey: accept only ssh-ed25519 keys and return
// the normalized "ssh-ed25519 <base64>" (comment dropped).
func ParsePubkey(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", errors.New("invalid tunnel_pubkey")
	}
	parts := strings.Fields(s)
	if len(parts) < 2 || parts[0] != "ssh-ed25519" || len(parts[1]) > 128 {
		return "", errors.New("tunnel_pubkey must be ssh-ed25519")
	}
	blob, err := b64decodeStrict(parts[1])
	if err != nil {
		return "", errors.New("invalid tunnel_pubkey")
	}
	bad := errors.New("invalid tunnel_pubkey")
	if len(blob) < 4 {
		return "", bad
	}
	n := int(binary.BigEndian.Uint32(blob[:4]))
	if n != len("ssh-ed25519") || len(blob) != 8+n+32 ||
		!bytes.Equal(blob[4:4+n], []byte("ssh-ed25519")) ||
		binary.BigEndian.Uint32(blob[4+n:8+n]) != 32 {
		return "", bad
	}
	return "ssh-ed25519 " + parts[1], nil
}

// b64decodeStrict approximates base64.b64decode(s, validate=True): standard
// alphabet, padding required to a multiple of 4. Python ignores non-zero
// trailing bits; Go's Strict mode would reject them, so use the lenient form.
func b64decodeStrict(s string) ([]byte, error) {
	if len(s)%4 != 0 {
		return nil, errors.New("bad padding")
	}
	return base64.StdEncoding.DecodeString(s)
}

// Fingerprint ports fingerprint: OpenSSH-style SHA256 fingerprint.
func Fingerprint(pub string) string {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return ""
	}
	blob, _ := base64.StdEncoding.DecodeString(f[1])
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

var tunnelLine = regexp.MustCompile(`(ssh-ed25519 [A-Za-z0-9+/=]+)\s+nocturne-tunnel-([a-z0-9-]+)\s*$`)

// LegacyTunnelKeys ports legacy_tunnel_keys: {worker_id: "ssh-ed25519 ..."}
// from "... nocturne-tunnel-<id>" lines carrying permitopen= in authorized_keys.
func LegacyTunnelKeys(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if m := tunnelLine.FindStringSubmatch(line); m != nil && strings.Contains(line, "permitopen=") {
			out[m[2]] = m[1]
		}
	}
	return out
}
