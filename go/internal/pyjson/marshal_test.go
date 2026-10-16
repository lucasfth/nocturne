package pyjson

import (
	"math"
	"testing"
	"time"
)

// Expected strings were produced by CPython 3 json.dumps / repr / round.
func TestMarshal(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{Object{}, `{}`},
		{Object{{"prompt_tokens", 2}, {"completion_tokens", 3}}, `{"prompt_tokens": 2, "completion_tokens": 3}`},
		{Object{{"b", 1}, {"a", []any{1, "x", nil, true, false}}}, `{"b": 1, "a": [1, "x", null, true, false]}`},
		{"æø\"\\\n\x01😀", `"\u00e6\u00f8\"\\\n\u0001\ud83d\ude00"`},
		{map[string]any{"z": 1, "a": 2.5}, `{"a": 2.5, "z": 1}`},
		{[]any{}, `[]`},
	}
	for _, c := range cases {
		got, err := Marshal(c.in)
		if err != nil || got != c.want {
			t.Errorf("Marshal(%#v)=%q,%v want %q", c.in, got, err, c.want)
		}
	}
	if _, err := Marshal(struct{}{}); err == nil {
		t.Error("expected error for unsupported type")
	}
}

func TestFloatRepr(t *testing.T) {
	cases := map[float64]string{
		1: "1.0", 0.1: "0.1", -2.5: "-2.5", 1e16: "1e+16", 1e15: "1000000000000000.0",
		1.5e-5: "1.5e-05", 0.0001: "0.0001", 123456789.125: "123456789.125",
		1760538000.123456: "1760538000.123456", math.Inf(1): "Infinity", math.Inf(-1): "-Infinity",
		math.Copysign(0, -1): "-0.0", 0: "0.0", 1.7976931348623157e308: "1.7976931348623157e+308",
	}
	for in, want := range cases {
		if got := FloatRepr(in); got != want {
			t.Errorf("FloatRepr(%v)=%q want %q", in, got, want)
		}
	}
	if FloatRepr(math.NaN()) != "NaN" {
		t.Error("NaN")
	}
}

func TestRoundHalfEven(t *testing.T) {
	cases := []struct {
		x    float64
		n    int
		want float64
	}{
		{2.675, 2, 2.67}, {0.5, 0, 0}, {1.5, 0, 2}, {2.5, 0, 2}, {-2.5, 0, -2},
		{0.125, 2, 0.12}, {0.375, 2, 0.38}, {1234.5, -1, 1230}, {1250, -2, 1200},
		{1350, -2, 1400}, {0.1 + 0.2, 1, 0.3}, {5e-324, 3, 0},
	}
	for _, c := range cases {
		if got := RoundHalfEven(c.x, c.n); got != c.want {
			t.Errorf("RoundHalfEven(%v,%d)=%v want %v", c.x, c.n, got, c.want)
		}
	}
	if RoundInt(2.5) != 2 || RoundInt(3.5) != 4 || RoundInt(-0.5) != 0 || RoundInt(200.49) != 200 {
		t.Error("RoundInt")
	}
	if !math.Signbit(RoundHalfEven(-0.001, 1)) {
		t.Error("round(-0.001,1) should be -0.0")
	}
}

func TestIsoformat(t *testing.T) {
	cph := time.FixedZone("CEST", 2*3600)
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 15, 14, 41, 0, 0, time.UTC), "2026-10-15T14:41:00+00:00"},
		{time.Date(2026, 10, 15, 14, 41, 0, 123456000, cph), "2026-10-15T14:41:00.123456+02:00"},
		{time.Date(2026, 1, 2, 3, 4, 5, 1500, time.FixedZone("x", -(5*3600+30*60))), "2026-01-02T03:04:05.000002-05:30"},
		{time.Date(2026, 1, 2, 3, 4, 5, 999999900, time.UTC), "2026-01-02T03:04:06+00:00"},
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("s", 3600+7)), "2026-01-02T03:04:05+01:00:07"},
	}
	for _, c := range cases {
		if got := Isoformat(c.t); got != c.want {
			t.Errorf("Isoformat=%q want %q", got, c.want)
		}
	}
}
