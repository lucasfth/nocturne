package pyjson

import (
	"fmt"
	"math"
	"math/big"
	"time"
)

// RoundHalfEven mirrors Python's round(x, ndigits) for floats: the exact
// binary value of x is rounded to ndigits decimal places (ties to even) and
// the decimal result is converted back to the nearest float64.
func RoundHalfEven(x float64, ndigits int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x == 0 {
		return x
	}
	r := new(big.Rat).SetFloat64(x) // exact binary value, as CPython does
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt(ndigits))), nil))
	if ndigits >= 0 {
		r.Mul(r, scale)
	} else {
		r.Quo(r, scale)
	}
	n := roundRatHalfEven(r)
	out := new(big.Rat).SetInt(n)
	if ndigits >= 0 {
		out.Quo(out, scale)
	} else {
		out.Mul(out, scale)
	}
	f, _ := out.Float64()
	if f == 0 && math.Signbit(x) {
		return math.Copysign(0, -1)
	}
	return f
}

// RoundInt mirrors Python's round(x) (no ndigits): nearest int, ties to even.
func RoundInt(x float64) int64 { return int64(math.RoundToEven(x)) }

func roundRatHalfEven(r *big.Rat) *big.Int {
	num, den := r.Num(), r.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int)) // truncated toward zero
	twice := new(big.Int).Abs(m)
	twice.Lsh(twice, 1)
	switch twice.Cmp(den) {
	case 1:
		q.Add(q, big.NewInt(int64(r.Sign())))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(int64(r.Sign())))
		}
	}
	return q
}

func absInt(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

// Isoformat mirrors Python datetime.isoformat() for an aware datetime:
// YYYY-MM-DDTHH:MM:SS[.ffffff]+HH:MM[:SS]. Nanoseconds are rounded to
// microseconds half-even, like datetime.fromtimestamp.
func Isoformat(t time.Time) string {
	ns := t.Nanosecond()
	us, rem := ns/1000, ns%1000
	if rem > 500 || (rem == 500 && us%2 == 1) {
		t = t.Add(time.Duration(1000 - rem))
	} else {
		t = t.Add(-time.Duration(rem))
	}
	us = t.Nanosecond() / 1000
	s := t.Format("2006-01-02T15:04:05")
	if us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	s += fmt.Sprintf("%s%02d:%02d", sign, off/3600, off%3600/60)
	if off%60 != 0 {
		s += fmt.Sprintf(":%02d", off%60)
	}
	return s
}
