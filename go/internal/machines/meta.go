package machines

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var (
	idRe  = regexp.MustCompile(`\A[a-z0-9][a-z0-9-]{0,31}\z`)
	hexRe = regexp.MustCompile(`\A[0-9a-f]{64}\z`)
	// hostRe is the STRICT host pattern minus its (?=.{1,253}$) lookahead,
	// which validHost implements as an explicit length check.
	hostRe = regexp.MustCompile(`\A[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*\.?\z`)
	userRe = regexp.MustCompile(`\A[a-z_][a-z0-9_.-]{0,31}\z`)
	jumpRe = regexp.MustCompile(`\A[a-z_][a-z0-9_.-]{0,31}@[A-Za-z0-9.-]{1,253}\z`)
)

// IDPattern is the Python ID.pattern, used in error messages.
const IDPattern = `^[a-z0-9][a-z0-9-]{0,31}$`

func validHost(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= 253 && !strings.Contains(s, "\n") && hostRe.MatchString(s)
}

var strict = map[string]func(string) bool{
	"user":       userRe.MatchString,
	"admin_user": userRe.MatchString,
	"host":       validHost,
	"jump":       jumpRe.MatchString,
}

var free = map[string]bool{"owner": true, "agent": true, "model": true}

// MetaKeys is the sorted META set.
var MetaKeys = []string{"admin_user", "agent", "host", "ip", "jump", "model", "owner", "user"}

func isMetaKey(k string) bool {
	i := sort.SearchStrings(MetaKeys, k)
	return i < len(MetaKeys) && MetaKeys[i] == k
}

// ValidID reports whether wid matches the worker id pattern.
func ValidID(wid string) bool { return idRe.MatchString(wid) }

func cleanFree(v any) (string, error) {
	s, ok := v.(string)
	if !ok || utf8.RuneCountInString(s) > 128 {
		return "", errors.New("invalid text")
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return "", errors.New("invalid text")
		}
	}
	return strings.TrimSpace(s), nil
}

// CleanMeta ports clean_meta. Values in the result are string or nil (nil
// means "remove this key"). meta must be a map[string]any.
func CleanMeta(meta any) (map[string]any, error) {
	m, ok := meta.(map[string]any)
	if ok {
		for k := range m {
			if !isMetaKey(k) {
				ok = false
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("meta keys allowed: %s", strings.Join(MetaKeys, ", "))
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if v == nil || v == "" {
			out[k] = nil
			continue
		}
		switch {
		case free[k]:
			s, err := cleanFree(v)
			if err != nil {
				return nil, err
			}
			out[k] = s
		case k == "ip":
			s, ok := v.(string)
			if !ok {
				return nil, errors.New("invalid ip")
			}
			ip, err := netip.ParseAddr(s)
			if err != nil {
				return nil, errors.New("invalid ip")
			}
			if ip.Zone() != "" {
				return nil, errors.New("invalid ip (no scope ids)")
			}
			out[k] = ip.String()
		default:
			s, ok := v.(string)
			if !ok || !strict[k](s) {
				return nil, fmt.Errorf("invalid %s", k)
			}
			out[k] = s
		}
	}
	return out, nil
}
