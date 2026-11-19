package canonical_test

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// formatFloatOf parses a decimal literal and renders it with
// FormatFloat, so the tables below read exactly like Python repr
// inputs.
func formatFloatOf(t *testing.T, literal string) string {
	t.Helper()
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		t.Fatalf("ParseFloat(%s): %v", literal, err)
	}
	got, err := canonical.FormatFloat(f)
	if err != nil {
		t.Fatalf("FormatFloat(%s): %v", literal, err)
	}
	return got
}

// The nine float cases of testdata/corpus/numbers.json. Expectations
// are the corpus canonical_text values, i.e. Python json.dumps output.
func TestNumberFormatFloatCorpusCases(t *testing.T) {
	cases := []struct{ literal, want string }{
		{"-0.0", "-0.0"},
		{"0.1", "0.1"},
		{"1.0", "1.0"},
		{"1.7976931348623157e308", "1.7976931348623157e+308"},
		{"123456789.123456789", "123456789.12345679"},
		{"1e-7", "1e-07"},
		{"1e20", "1e+20"},
		{"1e21", "1e+21"},
		{"5e-324", "5e-324"},
	}
	for _, c := range cases {
		if got := formatFloatOf(t, c.literal); got != c.want {
			t.Errorf("FormatFloat(%s) = %q, want %q", c.literal, got, c.want)
		}
	}
}

// Boundary pins where the corpus is silent. Every expectation was
// verified against python3 repr() (which json.dumps matches) before
// being pinned here:
//
//	python3 -c "for v in [1e15, 1e16, 0.0001, 0.00001, 2.5, -123.456, 100.0, 0.0, 1.5e21]: print(repr(v))"
func TestNumberFormatFloatBoundaries(t *testing.T) {
	cases := []struct{ literal, want string }{
		{"1e15", "1000000000000000.0"}, // exp 15 < 16: fixed, integral -> ".0"
		{"1e16", "1e+16"},              // exp 16: Python switches to exponent form
		{"0.0001", "0.0001"},           // exp -4: still fixed
		{"0.00001", "1e-05"},           // exp -5: exponent form, zero-padded
		{"2.5", "2.5"},
		{"-123.456", "-123.456"},
		{"100.0", "100.0"},
		{"0.0", "0.0"},
		{"1.5e21", "1.5e+21"}, // fractional digits survive in exponent form
	}
	for _, c := range cases {
		if got := formatFloatOf(t, c.literal); got != c.want {
			t.Errorf("FormatFloat(%s) = %q, want %q", c.literal, got, c.want)
		}
	}
}

func TestNumberFormatFloatNonFinite(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := canonical.FormatFloat(f); err == nil {
			t.Errorf("FormatFloat(%v) accepted a non-finite float", f)
		}
	}
}

// A >uint64 integer parsed from JSON must encode digit-exact: it can
// only do so if it never passes through float64 (2^64+1 is not
// representable as a float64; a float round-trip would print
// 18446744073709551616 or exponent form).
func TestNumberBigIntNeverPassesThroughFloat64(t *testing.T) {
	v := mustFromJSON(t, `{"value":18446744073709551617}`)
	if v.Pairs[0].Value.Kind != canonical.KindInt {
		t.Fatalf("kind = %v, want KindInt", v.Pairs[0].Value.Kind)
	}
	got, err := canonical.MarshalKernel(v)
	if err != nil {
		t.Fatalf("MarshalKernel: %v", err)
	}
	if !strings.Contains(string(got), `{"value":18446744073709551617}`) {
		t.Fatalf("MarshalKernel = %q, want digit-exact 18446744073709551617", got)
	}
	d, err := canonical.DigestKernel(v)
	if err != nil {
		t.Fatalf("DigestKernel: %v", err)
	}
	want, err := canonical.DigestKernel(mustFromJSON(t, `{"value":18446744073709551617}`))
	if err != nil {
		t.Fatalf("DigestKernel (reparse): %v", err)
	}
	if d != want {
		t.Fatalf("digest unstable across parses: %s != %s", d, want)
	}
}
