// Package pyjson reproduces the byte output of Python's json.dumps (default
// arguments: ensure_ascii=True, separators (', ', ': ')), plus small numeric
// and datetime helpers whose results must match CPython exactly.
package pyjson

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Pair is one key/value entry of an Object.
type Pair struct {
	Key string
	Val any
}

// Object is an insertion-ordered JSON object (a Python dict).
type Object []Pair

// Get returns the value for key and whether it was present.
func (o Object) Get(key string) (any, bool) {
	for _, p := range o {
		if p.Key == key {
			return p.Val, true
		}
	}
	return nil, false
}

// Set replaces an existing key in place or appends it (dict assignment semantics).
func (o *Object) Set(key string, val any) {
	for i := range *o {
		if (*o)[i].Key == key {
			(*o)[i].Val = val
			return
		}
	}
	*o = append(*o, Pair{key, val})
}

// Marshal renders v exactly as Python json.dumps(v) would.
func Marshal(v any) (string, error) {
	var b strings.Builder
	if err := write(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// MustMarshal is Marshal that panics on unsupported types.
func MustMarshal(v any) string {
	s, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return s
}

func write(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case float64:
		b.WriteString(FloatRepr(x))
	case float32:
		b.WriteString(FloatRepr(float64(x)))
	case string:
		writeString(b, x)
	case Object:
		b.WriteByte('{')
		for i, p := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeString(b, p.Key)
			b.WriteString(": ")
			if err := write(b, p.Val); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case map[string]any: // unordered: sorted keys (Go maps have no insertion order)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := make(Object, 0, len(x))
		for _, k := range keys {
			o = append(o, Pair{k, x[k]})
		}
		return write(b, o)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := write(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case []Object:
		a := make([]any, len(x))
		for i := range x {
			a[i] = x[i]
		}
		return write(b, a)
	default:
		return fmt.Errorf("pyjson: unsupported type %T", v)
	}
	return nil
}
