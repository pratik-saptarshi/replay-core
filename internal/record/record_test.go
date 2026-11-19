package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// The conformance corpus is the normative form of the law; these tests
// drive the record/witness API from it case by case.

type corpusCase struct {
	Name        string          `json:"name"`
	Profile     string          `json:"profile"`
	Input       json.RawMessage `json:"input"`
	Digest      string          `json:"digest"`
	ExpectError string          `json:"expect_error"`
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

func mustValue(t *testing.T, raw json.RawMessage) canonical.Value {
	t.Helper()
	v, err := canonical.FromJSON(raw)
	if err != nil {
		t.Fatalf("FromJSON(%s): %v", raw, err)
	}
	return v
}

func objGet(v canonical.Value, key string) (canonical.Value, bool) {
	if v.Kind != canonical.KindObject {
		return canonical.Value{}, false
	}
	for _, p := range v.Pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}

func mustGet(t *testing.T, v canonical.Value, key string) canonical.Value {
	t.Helper()
	got, ok := objGet(v, key)
	if !ok {
		t.Fatalf("object missing key %q", key)
	}
	return got
}

func mustString(t *testing.T, v canonical.Value, key string) string {
	t.Helper()
	got := mustGet(t, v, key)
	if got.Kind != canonical.KindString {
		t.Fatalf("key %q: kind = %v, want string", key, got.Kind)
	}
	return got.Str
}

// recordParams decomposes a record-shaped object (or a
// canonical_record_input args object, which carries the same fields)
// into RecordDigest arguments.
func recordParams(t *testing.T, rec canonical.Value) (schemaRef, mode string, body canonical.Value, causedBy []string, witness string) {
	t.Helper()
	schemaRef = mustString(t, rec, "schema_ref")
	mode = mustString(t, rec, "mode")
	body = mustGet(t, rec, "body")
	witness = mustString(t, rec, "witness")
	cb := mustGet(t, rec, "caused_by")
	if cb.Kind != canonical.KindArray {
		t.Fatalf("caused_by: kind = %v, want array", cb.Kind)
	}
	for _, e := range cb.Elems {
		if e.Kind != canonical.KindString {
			t.Fatalf("caused_by element: kind = %v, want string", e.Kind)
		}
		causedBy = append(causedBy, e.Str)
	}
	return schemaRef, mode, body, causedBy, witness
}

func isOpCase(c corpusCase) bool {
	return strings.Contains(string(c.Input), `"op"`)
}

// TestCorpusKernelDigestCases runs every kernel-profile digest case:
// record-shaped inputs through RecordDigest, witness-shaped inputs
// through WitnessBodyDigest, the plain byte-stability case through
// DigestKernel.
func TestCorpusKernelDigestCases(t *testing.T) {
	cases := loadCorpus(t, "kernel_v1.json")
	ran := 0
	for _, c := range cases {
		if c.ExpectError != "" || isOpCase(c) {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			input := mustValue(t, c.Input)
			kind, _ := objGet(input, "kind")
			var got string
			var err error
			switch {
			case kind.Kind == canonical.KindString && kind.Str == "record":
				schemaRef, mode, body, causedBy, witness := recordParams(t, input)
				got, err = RecordDigest(schemaRef, mode, body, causedBy, witness)
			case kind.Kind == canonical.KindString && kind.Str == "witness":
				got, err = WitnessBodyDigest(mustString(t, input, "schema_ref"), mustGet(t, input, "body"))
			default:
				got, err = canonical.DigestKernel(input)
			}
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if got != c.Digest {
				t.Fatalf("digest = %s, want %s", got, c.Digest)
			}
		})
	}
	if ran != 12 {
		t.Fatalf("ran %d kernel digest cases, want 12", ran)
	}
}

// TestCorpusValidationCases runs all 9 validation-error cases. Record
// cases must fail in both RecordInput and RecordDigest; witness-body
// cases must fail in both ValidateWitnessBody and WitnessBodyDigest
// (both entry points exercised — no digest over an unvalidated body).
func TestCorpusValidationCases(t *testing.T) {
	cases := loadCorpus(t, "kernel_v1.json")
	ran := 0
	for _, c := range cases {
		if c.ExpectError == "" {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			input := mustValue(t, c.Input)
			op := mustString(t, input, "op")
			args := mustGet(t, input, "args")
			switch op {
			case "canonical_record_input":
				schemaRef, mode, body, causedBy, witness := recordParams(t, args)
				if _, err := RecordInput(schemaRef, mode, body, causedBy, witness); err == nil {
					t.Fatalf("RecordInput: no error, want one containing %q", c.ExpectError)
				} else if !strings.Contains(err.Error(), c.ExpectError) {
					t.Fatalf("RecordInput error = %q, want substring %q", err.Error(), c.ExpectError)
				}
				if _, err := RecordDigest(schemaRef, mode, body, causedBy, witness); err == nil {
					t.Fatalf("RecordDigest: no error, want one containing %q", c.ExpectError)
				} else if !strings.Contains(err.Error(), c.ExpectError) {
					t.Fatalf("RecordDigest error = %q, want substring %q", err.Error(), c.ExpectError)
				}
			case "witness_body_digest":
				schemaRef := mustString(t, args, "schema_ref")
				body := mustGet(t, args, "body")
				if err := ValidateWitnessBody(body); err == nil {
					t.Fatalf("ValidateWitnessBody: no error, want one containing %q", c.ExpectError)
				} else if !strings.Contains(err.Error(), c.ExpectError) {
					t.Fatalf("ValidateWitnessBody error = %q, want substring %q", err.Error(), c.ExpectError)
				}
				if got, err := WitnessBodyDigest(schemaRef, body); err == nil {
					t.Fatalf("WitnessBodyDigest = %s, no error; want one containing %q", got, c.ExpectError)
				} else if !strings.Contains(err.Error(), c.ExpectError) {
					t.Fatalf("WitnessBodyDigest error = %q, want substring %q", err.Error(), c.ExpectError)
				}
			default:
				t.Fatalf("unknown op %q", op)
			}
		})
	}
	if ran != 9 {
		t.Fatalf("ran %d validation cases, want 9", ran)
	}
}

// TestNamedGoldenDigests pins the headline digests as literals,
// independent of the corpus tables above.
func TestNamedGoldenDigests(t *testing.T) {
	const (
		captureDigest     = "sha256:c1130f7072093a8a923135960f418ce2259bfe2050190bf0a464e97ee80c9df5"
		declarationDigest = "sha256:21cdb1f83bc4aadad0dae78498bd47c23cd5bb87ba3036aefabec79a87ad0407"
		orderedDigest     = "sha256:8220f74fa4352443bb3e08977d368a9094a417746c20139331c8c51f06bb299a"
		reversedDigest    = "sha256:1c6b78e033c8b005eb1a756706e871cd553bf2cdbeb188e75db9667d2c148dfd"
		rootBodyDigest    = "sha256:22e3bf15914b5007dea08a1079d1542470a369bcbed067687f345e7e85721130"
		rootRecordDigest  = "sha256:0b67497cb4c0f2ed954b7988ecc321b481e7d173c4fc201adeb86591b2fd6310"
		itemWitness       = "sha256:8872c210b2476cb0a9c2ab06d303d39d785393b761c0a24c3172317c19ba4bf1"
		parent1           = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		parent2           = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)

	itemBody := mustValue(t, json.RawMessage(`{"name":"alpha","value":1}`))

	capture, err := RecordDigest("example.kernel.item.v1", ModeCapture, itemBody, nil, itemWitness)
	if err != nil {
		t.Fatalf("capture RecordDigest: %v", err)
	}
	if capture != captureDigest {
		t.Fatalf("capture digest = %s, want %s", capture, captureDigest)
	}

	declaration, err := RecordDigest("example.kernel.item.v1", ModeDeclaration, itemBody, nil, itemWitness)
	if err != nil {
		t.Fatalf("declaration RecordDigest: %v", err)
	}
	if declaration != declarationDigest {
		t.Fatalf("declaration digest = %s, want %s", declaration, declarationDigest)
	}
	if declaration == capture {
		t.Fatal("capture and declaration digests must differ")
	}

	ordered, err := RecordDigest("example.kernel.item.v1", ModeCapture, itemBody, []string{parent1, parent2}, itemWitness)
	if err != nil {
		t.Fatalf("ordered RecordDigest: %v", err)
	}
	if ordered != orderedDigest {
		t.Fatalf("ordered-parent digest = %s, want %s", ordered, orderedDigest)
	}
	reversed, err := RecordDigest("example.kernel.item.v1", ModeCapture, itemBody, []string{parent2, parent1}, itemWitness)
	if err != nil {
		t.Fatalf("reversed RecordDigest: %v", err)
	}
	if reversed != reversedDigest {
		t.Fatalf("reversed-parent digest = %s, want %s", reversed, reversedDigest)
	}
	if ordered == reversed {
		t.Fatal("parent order must change the digest")
	}

	// Root witness: body digest and record id are distinct ground
	// truths, each asserted exactly.
	bodyDigest, err := canonical.DigestKernel(RootWitnessBody())
	if err != nil {
		t.Fatalf("DigestKernel(RootWitnessBody): %v", err)
	}
	if bodyDigest != rootBodyDigest {
		t.Fatalf("root witness body digest = %s, want %s", bodyDigest, rootBodyDigest)
	}
	if got := RootWitnessRecordID(); got != rootRecordDigest {
		t.Fatalf("RootWitnessRecordID = %s, want %s", got, rootRecordDigest)
	}
	if bodyDigest == rootRecordDigest {
		t.Fatal("root witness body digest and record id must differ")
	}

	// WitnessBodyDigest over the root fields agrees with
	// DigestKernel(RootWitnessBody()).
	viaAPI, err := WitnessBodyDigest(RootWitnessSchemaRef, rootWitnessFields())
	if err != nil {
		t.Fatalf("WitnessBodyDigest(root): %v", err)
	}
	if viaAPI != rootBodyDigest {
		t.Fatalf("WitnessBodyDigest(root) = %s, want %s", viaAPI, rootBodyDigest)
	}
}

// TestWitnessEntryPoints exercises ValidateWitnessBody and
// WitnessBodyDigest directly: a valid body passes both and digests to
// the golden value; an invalid body fails both, with no digest.
func TestWitnessEntryPoints(t *testing.T) {
	ordinary := mustValue(t, json.RawMessage(`{
		"active_binding_refs": ["binding:task"],
		"actor_ref": "runtime:test",
		"authority_refs": ["trusted:internal"],
		"containment": "contained",
		"provenance_policy_refs": [],
		"semantic_environment_refs": ["schema-set:test"],
		"substrate_ref": "sqlite.local.v1",
		"visibility_policy_refs": ["visibility:payload"]
	}`))
	if err := ValidateWitnessBody(ordinary); err != nil {
		t.Fatalf("ValidateWitnessBody(ordinary): %v", err)
	}
	got, err := WitnessBodyDigest("kernel.witness.v1", ordinary)
	if err != nil {
		t.Fatalf("WitnessBodyDigest(ordinary): %v", err)
	}
	const ordinaryBodyDigest = "sha256:66f9e7b7cdfe45ed7bbaee41181f85564a2c7c87eaa88d5ae3492ae4a7581227"
	if got != ordinaryBodyDigest {
		t.Fatalf("ordinary witness body digest = %s, want %s", got, ordinaryBodyDigest)
	}

	// The root fields are a valid witness body (containment "full").
	if err := ValidateWitnessBody(rootWitnessFields()); err != nil {
		t.Fatalf("ValidateWitnessBody(root fields): %v", err)
	}

	// Non-object body: both entry points fail.
	notObject := canonical.ArrayOf()
	if err := ValidateWitnessBody(notObject); err == nil {
		t.Fatal("ValidateWitnessBody(array): no error, want one")
	}
	if d, err := WitnessBodyDigest("kernel.witness.v1", notObject); err == nil {
		t.Fatalf("WitnessBodyDigest(array) = %s, no error; want one", d)
	}

	// Missing-field body: WitnessBodyDigest must error, never digest.
	missing := mustValue(t, json.RawMessage(`{"actor_ref":"runtime:test"}`))
	if d, err := WitnessBodyDigest("kernel.witness.v1", missing); err == nil {
		t.Fatalf("WitnessBodyDigest(missing fields) = %s, no error; want one", d)
	}
}

// TestCommonsObjectDigestCases runs the commons-profile corpus
// through CommonsObjectDigest: the 4 digest vectors must match, and
// the float-forbidden value case must error.
func TestCommonsObjectDigestCases(t *testing.T) {
	cases := loadCorpus(t, "commons_v1.json")
	digests, errs := 0, 0
	for _, c := range cases {
		if isOpCase(c) {
			continue // canonical_value_from_bytes cases belong to the decoder, not this API
		}
		if c.ExpectError != "" {
			errs++
			t.Run(c.Name, func(t *testing.T) {
				input := mustValue(t, c.Input)
				if got, err := CommonsObjectDigest(input); err == nil {
					t.Fatalf("CommonsObjectDigest = %s, no error; want one containing %q", got, c.ExpectError)
				} else if !strings.Contains(err.Error(), c.ExpectError) {
					t.Fatalf("CommonsObjectDigest error = %q, want substring %q", err.Error(), c.ExpectError)
				}
			})
			continue
		}
		digests++
		t.Run(c.Name, func(t *testing.T) {
			input := mustValue(t, c.Input)
			got, err := CommonsObjectDigest(input)
			if err != nil {
				t.Fatalf("CommonsObjectDigest: %v", err)
			}
			if got != c.Digest {
				t.Fatalf("CommonsObjectDigest = %s, want %s", got, c.Digest)
			}
		})
	}
	if digests != 4 {
		t.Fatalf("ran %d commons digest cases, want 4", digests)
	}
	if errs != 1 {
		t.Fatalf("ran %d commons error cases, want 1", errs)
	}
}
