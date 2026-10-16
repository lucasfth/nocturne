package pyjson

import (
	"sort"
	"strings"
)

// MarshalIndentSorted mirrors json.dumps(v, indent=indent, sort_keys=True):
// separators (',', ': '), ensure_ascii=True, no trailing newline.
// Object keys and map keys are both emitted in sorted order.
func MarshalIndentSorted(v any, indent int) (string, error) {
	var b strings.Builder
	if err := writeIndent(&b, v, strings.Repeat(" ", indent), 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeIndent(b *strings.Builder, v any, unit string, depth int) error {
	nl := func(d int) {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(unit, d))
	}
	switch x := v.(type) {
	case Object:
		m := make(map[string]any, len(x))
		for _, p := range x {
			m[p.Key] = p.Val
		}
		return writeIndent(b, m, unit, depth)
	case map[string]any:
		if len(x) == 0 {
			b.WriteString("{}")
			return nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // Python sorts str by code point; byte order of UTF-8 agrees
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			nl(depth + 1)
			writeString(b, k)
			b.WriteString(": ")
			if err := writeIndent(b, x[k], unit, depth+1); err != nil {
				return err
			}
		}
		nl(depth)
		b.WriteByte('}')
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			nl(depth + 1)
			if err := writeIndent(b, e, unit, depth+1); err != nil {
				return err
			}
		}
		nl(depth)
		b.WriteByte(']')
	default:
		return write(b, v)
	}
	return nil
}
