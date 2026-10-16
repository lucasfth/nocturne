package benchmark

import (
	"strings"
	"testing"
)

func TestSummaryRejectsComputedOverflow(t *testing.T) {
	o := Options{ModelSHA256: strings.Repeat("a", 64), BuildDigest: strings.Repeat("b", 64)}
	o, _ = o.Normalize()
	s := Summary{Quality: QualityPass, Context: o.Context, Parallel: o.Parallel, PromptTokens: o.Context - 100, OutputTokens: 100, DurationSeconds: 1e-308, TokensPerSecond: 50, ModelVerified: true, ModelSHA256: o.ModelSHA256, BuildDigest: o.BuildDigest}
	if s.valid(o) {
		t.Fatal("overflow accepted")
	}
}
