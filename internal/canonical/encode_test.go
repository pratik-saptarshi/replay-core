package canonical_test

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// mustFromJSON parses data through the value model or fails the test.
func mustFromJSON(t *testing.T, data string) canonical.Value {
	t.Helper()
	v, err := canonical.FromJSON([]byte(data))
	if err != nil {
		t.Fatalf("FromJSON(%s): %v", data, err)
	}
	return v
}

func TestEncodeKeyOrderIndependence(t *testing.T) {
	a := mustFromJSON(t, `{"z":1,"a":{"y":2,"b":3}}`)
	b := mustFromJSON(t, `{"a":{"b":3,"y":2},"z":1}`)
	ka, err := canonical.MarshalKernel(a)
	if err != nil {
		t.Fatalf("MarshalKernel(a): %v", err)
	}
	kb, err := canonical.MarshalKernel(b)
	if err != nil {
		t.Fatalf("MarshalKernel(b): %v", err)
	}
	if string(ka) != string(kb) {
		t.Fatalf("key order changed output:\n%s\n%s", ka, kb)
	}
	da, err := canonical.DigestKernel(a)
	if err != nil {
		t.Fatalf("DigestKernel(a): %v", err)
	}
	db, err := canonical.DigestKernel(b)
	if err != nil {
		t.Fatalf("DigestKernel(b): %v", err)
	}
	if da != db {
		t.Fatalf("key order changed digest: %s != %s", da, db)
	}
}

func TestEncodeSnowmanBytes(t *testing.T) {
	v := mustFromJSON(t, `{"a":{"snowman":"snowman-☃"},"z":1}`)
	got, err := canonical.MarshalKernel(v)
	if err != nil {
		t.Fatalf("MarshalKernel: %v", err)
	}
	// Law: kernel profile emits raw UTF-8; the snowman is bytes E2 98 83.
	want := "replaycore.kernel.canonical.v1\n{\"a\":{\"snowman\":\"snowman-\xe2\x98\x83\"},\"z\":1}"
	if string(got) != want {
		t.Fatalf("MarshalKernel =\n%q\nwant\n%q", got, want)
	}
}

func TestEncodeCommonsUnicodeEscaped(t *testing.T) {
	v := mustFromJSON(t, `{"text":"café ☕"}`)
	got, err := canonical.MarshalCommons(v)
	if err != nil {
		t.Fatalf("MarshalCommons: %v", err)
	}
	// Law: commons profile escapes every non-ASCII rune as lowercase \uXXXX.
	want := "commons.canonical.v1\n{\"text\":\"caf\\u00e9 \\u2615\"}"
	if string(got) != want {
		t.Fatalf("MarshalCommons =\n%q\nwant\n%q", got, want)
	}
}

func TestEncodeAstralKeySorting(t *testing.T) {
	// Law: object keys sort by Unicode code point, not by UTF-16 unit.
	// U+E000 < U+10000 by code point; UTF-16 unit order (D800 < E000)
	// would reverse them.
	v := mustFromJSON(t, `{"\ud800\udc00":1,"\ue000":2}`)
	got, err := canonical.MarshalKernel(v)
	if err != nil {
		t.Fatalf("MarshalKernel: %v", err)
	}
	want := "replaycore.kernel.canonical.v1\n{\"\ue000\":2,\"\U00010000\":1}"
	if string(got) != want {
		t.Fatalf("MarshalKernel =\n%q\nwant\n%q", got, want)
	}

	// U+FB00 < U+1F600 by code point.
	v2 := mustFromJSON(t, `{"\ud83d\ude00":1,"\ufb00":2}`)
	got2, err := canonical.MarshalKernel(v2)
	if err != nil {
		t.Fatalf("MarshalKernel: %v", err)
	}
	want2 := "replaycore.kernel.canonical.v1\n{\"\ufb00\":2,\"\U0001f600\":1}"
	if string(got2) != want2 {
		t.Fatalf("MarshalKernel =\n%q\nwant\n%q", got2, want2)
	}
}

func TestEncodeDuplicateKeyRejected(t *testing.T) {
	if _, err := canonical.FromJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("FromJSON accepted duplicate object keys")
	} else if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate-key error does not say so: %v", err)
	}
	if _, err := canonical.FromJSON([]byte(`{"a":{"b":1,"b":2}}`)); err == nil {
		t.Fatal("FromJSON accepted nested duplicate object keys")
	}
}

func TestEncodeNonFiniteRejected(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		v := canonical.FloatOf(f)
		if _, err := canonical.MarshalKernel(v); err == nil {
			t.Fatalf("MarshalKernel accepted non-finite float %v", f)
		}
		if _, err := canonical.MarshalCommons(v); err == nil {
			t.Fatalf("MarshalCommons accepted non-finite float %v", f)
		}
		if _, err := canonical.DigestKernel(v); err == nil {
			t.Fatalf("DigestKernel accepted non-finite float %v", f)
		}
		if _, err := canonical.DigestCommons(v); err == nil {
			t.Fatalf("DigestCommons accepted non-finite float %v", f)
		}
	}
}

func TestEncodeCommonsFloatForbiddenNamesPath(t *testing.T) {
	v := mustFromJSON(t, `{"a":[{"x":1.5}]}`)
	_, err := canonical.MarshalCommons(v)
	if err == nil {
		t.Fatal("MarshalCommons accepted a float")
	}
	if !strings.Contains(err.Error(), "floats are forbidden in canonical inputs") {
		t.Fatalf("error does not carry the law's wording: %v", err)
	}
	if !strings.Contains(err.Error(), "$.a[0].x") {
		t.Fatalf("error does not name the JSON path $.a[0].x: %v", err)
	}
	// The same value is legal under the kernel profile (float formatting
	// itself is Task 3's law; acceptance here only checks it encodes).
	if _, err := canonical.MarshalKernel(v); err != nil {
		t.Fatalf("MarshalKernel rejected a finite float: %v", err)
	}
}

// --- Corpus-driven tests -------------------------------------------------

type corpusCase struct {
	Name                string          `json:"name"`
	Profile             string          `json:"profile"`
	Input               json.RawMessage `json:"input"`
	CanonicalText       *string         `json:"canonical_text"`
	Digest              *string         `json:"digest"`
	ExpectError         *string         `json:"expect_error"`
	RawCanonicalJSONB64 *string         `json:"raw_canonical_json_b64"`
}

func loadCorpus(t *testing.T, file string) []corpusCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "corpus", file))
	if err != nil {
		t.Fatalf("read corpus %s: %v", file, err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parse corpus %s: %v", file, err)
	}
	return cases
}

// runDigestCases checks every digest case in a corpus file against one
// profile's Marshal/Digest pair. No case is skipped: float formatting
// (Task 3's law) is implemented, so float-bearing cases run and pass.
func runDigestCases(t *testing.T, file string, marshal func(canonical.Value) ([]byte, error), digest func(canonical.Value) (string, error)) {
	t.Helper()
	for _, c := range loadCorpus(t, file) {
		if c.Digest == nil {
			continue // error/validation cases are exercised elsewhere
		}
		t.Run(c.Name, func(t *testing.T) {
			v, err := canonical.FromJSON(c.Input)
			if err != nil {
				t.Fatalf("FromJSON(input): %v", err)
			}
			got, err := marshal(v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != *c.CanonicalText {
				t.Fatalf("marshal =\n%q\nwant\n%q", got, *c.CanonicalText)
			}
			if c.RawCanonicalJSONB64 != nil {
				raw, err := base64.StdEncoding.DecodeString(*c.RawCanonicalJSONB64)
				if err != nil {
					t.Fatalf("decode raw_canonical_json_b64: %v", err)
				}
				body := strings.SplitN(string(got), "\n", 2)[1]
				if body != string(raw) {
					t.Fatalf("marshal body = %q, want raw bytes %q", body, raw)
				}
			}
			d, err := digest(v)
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if d != *c.Digest {
				t.Fatalf("digest = %s, want %s", d, *c.Digest)
			}
		})
	}
}

func TestEncodeCorpusKernel(t *testing.T) {
	runDigestCases(t, "kernel_v1.json", canonical.MarshalKernel, canonical.DigestKernel)
}

func TestEncodeCorpusCommons(t *testing.T) {
	runDigestCases(t, "commons_v1.json", canonical.MarshalCommons, canonical.DigestCommons)
}

func TestEncodeCorpusNumbers(t *testing.T) {
	// All thirteen cases run: the four integer cases (Task 2) and the
	// nine float cases under the Python-repr number law (Task 3).
	runDigestCases(t, "numbers.json", canonical.MarshalKernel, canonical.DigestKernel)
}

func TestEncodeCorpusCommonsErrorCases(t *testing.T) {
	for _, c := range loadCorpus(t, "commons_v1.json") {
		if c.ExpectError == nil {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			switch c.Name {
			case "error/float_forbidden":
				v, err := canonical.FromJSON(c.Input)
				if err != nil {
					t.Fatalf("FromJSON(input): %v", err)
				}
				if _, err := canonical.MarshalCommons(v); err == nil {
					t.Fatal("MarshalCommons accepted a float")
				} else if !strings.Contains(err.Error(), *c.ExpectError) {
					t.Fatalf("error %q does not contain %q", err, *c.ExpectError)
				}
			case "error/duplicate_key", "error/non_canonical_bytes":
				// Both are canonical_value_from_bytes cases: the strict
				// decoder (Task 5) must reject the payload — duplicate
				// keys at parse time, unsorted keys at the byte-equality
				// check — with the corpus's error wording.
				var in struct {
					PayloadB64 string `json:"payload_b64"`
				}
				if err := json.Unmarshal(c.Input, &in); err != nil {
					t.Fatalf("parse case input: %v", err)
				}
				payload, err := base64.StdEncoding.DecodeString(in.PayloadB64)
				if err != nil {
					t.Fatalf("decode payload_b64: %v", err)
				}
				if _, err := canonical.DecodeCommons(payload); err == nil {
					t.Fatalf("DecodeCommons accepted corpus payload for %s", c.Name)
				} else if !strings.Contains(err.Error(), *c.ExpectError) {
					t.Fatalf("error %q does not contain %q", err, *c.ExpectError)
				}
			default:
				t.Fatalf("unhandled commons error case %s", c.Name)
			}
		})
	}
}
