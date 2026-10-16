package pyjson

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

const hexdigits = "0123456789abcdef"

// writeString mirrors json's ensure_ascii encoder (py_encode_basestring_ascii).
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r >= 0x20 && r < 0x7f {
				b.WriteRune(r)
			} else if r > 0xffff {
				r1, r2 := utf16.EncodeRune(r)
				writeU(b, r1)
				writeU(b, r2)
			} else {
				writeU(b, r)
			}
		}
	}
	b.WriteByte('"')
}

func writeU(b *strings.Builder, r rune) {
	b.WriteString(`\u`)
	for shift := 12; shift >= 0; shift -= 4 {
		b.WriteByte(hexdigits[(r>>uint(shift))&0xf])
	}
}

// FloatRepr returns Python's float.__repr__ as used by json.dumps
// (NaN, Infinity, -Infinity for non-finite values).
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // e.g. -1.2345e+06
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, expS, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expS)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := strconv.Itoa(abs(exp))
		if len(es) < 2 {
			es = "0" + es
		}
		if exp < 0 {
			return sign + m + "e-" + es
		}
		return sign + m + "e+" + es
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}
	return sign + digits[:exp+1] + "." + digits[exp+1:]
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
