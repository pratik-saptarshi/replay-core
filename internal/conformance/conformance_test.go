package conformance_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
)

// The gate: every case in every corpus file is accounted for by name
// and executed. Classes are derived from the case's own shape (op,
// profile, kind, expect_error), never from a name allowlist; a case
// whose shape matches no class fails the test at classification time.

const (
	profileKernel  = "replaycore.kernel.canonical.v1"
	profileCommons = "commons.canonical.v1"
)

var corpusFiles = []string{"kernel_v1.json", "commons_v1.json", "numbers.json"}

type corpusCase struct {
	Name                string          `json:"name"`
	Profile             string          `json:"profile"`
	Input               json.RawMessage `json:"input"`
	CanonicalText       *string         `json:"canonical_text"`
	Digest              *string         `json:"digest"`
	ExpectError         *string         `json:"expect_error"`
	RawCanonicalJSONB64 *string         `json:"raw_canonical_json_b64"`
}

func corpusDir() string {
	return filepath.Join("..", "..", "testdata", "corpus")
}

func loadFile(t *testing.T, file string) []corpusCase {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(corpusDir(), file))
	if err != nil {
		t.Fatalf("read corpus %s: %v", file, err)
	}
	var cases []corpusCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("parse corpus %s: %v", file, err)
	}
	if len(cases) == 0 {
		t.Fatalf("corpus %s contains no cases", file)
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

// opOf reports the case input's op, if the input is an op object.
func opOf(t *testing.T, c corpusCase) (string, bool) {
	t.Helper()
	var probe struct {
		Op *string `json:"op"`
	}
	if err := json.Unmarshal(c.Input, &probe); err != nil || probe.Op == nil {
		return "", false
	}
	return *probe.Op, true
}

// classify assigns every case to exactly one execution class from its
// shape. The bool is false for a shape the gate does not know — the
// caller fails the test, so a new case kind can never slip through
// unexecuted.
func classify(t *testing.T, c corpusCase) (string, bool) {
	t.Helper()
	if op, ok := opOf(t, c); ok {
		switch op {
		case "canonical_value_from_bytes":
			return "decoder-bytes", true
		case "canonical_record_input":
			return "record-validation", true
		case "witness_body_digest":
			return "witness-validation", true
		}
		return "", false
	}
	if c.ExpectError != nil {
		return "value-error", true
	}
	if c.Digest == nil || c.CanonicalText == nil {
		return "", false
	}
	switch c.Profile {
	case profileCommons:
		return "commons-digest", true
	case profileKernel:
		input := mustValue(t, c.Input)
		if kind, ok := objGet(input, "kind"); ok && kind.Kind == canonical.KindString {
			switch kind.Str {
			case "record":
				return "record-digest", true
			case "witness":
				return "witness-digest", true
			}
		}
		return "kernel-digest", true
	}
	return "", false
}

func profileFuncs(profile string) (marshal func(canonical.Value) ([]byte, error), digest func(canonical.Value) (string, error), decode func([]byte) (canonical.Value, error), ok bool) {
	switch profile {
	case profileKernel:
		return canonical.MarshalKernel, canonical.DigestKernel, canonical.DecodeKernel, true
	case profileCommons:
		return canonical.MarshalCommons, canonical.DigestCommons, canonical.DecodeCommons, true
	}
	return nil, nil, nil, false
}

// checkDigestCase verifies a digest case end to end: marshal output
// is byte-equal to canonical_text, the digest recomputes, the strict
// decoder accepts canonical_text, and (when present) the unprefixed
// body equals raw_canonical_json_b64.
func checkDigestCase(t *testing.T, c corpusCase, marshal func(canonical.Value) ([]byte, error), digest func(canonical.Value) (string, error), decode func([]byte) (canonical.Value, error)) canonical.Value {
	t.Helper()
	v := mustValue(t, c.Input)
	got, err := marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != *c.CanonicalText {
		t.Fatalf("marshal =\n%q\nwant canonical_text\n%q", got, *c.CanonicalText)
	}
	d, err := digest(v)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if d != *c.Digest {
		t.Fatalf("digest = %s, want %s", d, *c.Digest)
	}
	// The recorded digest must be the SHA-256 of the recorded bytes:
	// a corpus edited consistently in input and digest still has to
	// satisfy this identity against canonical_text.
	sum := sha256.Sum256([]byte(*c.CanonicalText))
	if "sha256:"+hex.EncodeToString(sum[:]) != *c.Digest {
		t.Fatalf("corpus self-consistency: sha256(canonical_text) != digest %s", *c.Digest)
	}
	// Canonical bytes must pass the strict decoder and round-trip.
	dv, err := decode([]byte(*c.CanonicalText))
	if err != nil {
		t.Fatalf("strict decode of canonical_text: %v", err)
	}
	again, err := marshal(dv)
	if err != nil {
		t.Fatalf("re-marshal of decoded value: %v", err)
	}
	if string(again) != *c.CanonicalText {
		t.Fatalf("decode/marshal round trip =\n%q\nwant\n%q", again, *c.CanonicalText)
	}
	if c.RawCanonicalJSONB64 != nil {
		raw, err := base64.StdEncoding.DecodeString(*c.RawCanonicalJSONB64)
		if err != nil {
			t.Fatalf("decode raw_canonical_json_b64: %v", err)
		}
		body := strings.SplitN(*c.CanonicalText, "\n", 2)[1]
		if body != string(raw) {
			t.Fatalf("canonical body = %q, want raw bytes %q", body, raw)
		}
	}
	return v
}

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

func expectErrContains(t *testing.T, what string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, want one containing %q", what, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: error = %q, want substring %q", what, err.Error(), want)
	}
}

// runCase executes one case under its class. Any mismatch is fatal to
// the subtest.
func runCase(t *testing.T, c corpusCase, class string) {
	t.Helper()
	marshal, digest, decode, ok := profileFuncs(c.Profile)
	if !ok {
		t.Fatalf("unknown profile %q", c.Profile)
	}
	switch class {
	case "kernel-digest", "commons-digest":
		checkDigestCase(t, c, marshal, digest, decode)
	case "record-digest":
		v := checkDigestCase(t, c, marshal, digest, decode)
		schemaRef, mode, body, causedBy, witness := recordParams(t, v)
		got, err := record.RecordDigest(schemaRef, mode, body, causedBy, witness)
		if err != nil {
			t.Fatalf("RecordDigest: %v", err)
		}
		if got != *c.Digest {
			t.Fatalf("RecordDigest = %s, want %s", got, *c.Digest)
		}
	case "witness-digest":
		v := checkDigestCase(t, c, marshal, digest, decode)
		got, err := record.WitnessBodyDigest(mustString(t, v, "schema_ref"), mustGet(t, v, "body"))
		if err != nil {
			t.Fatalf("WitnessBodyDigest: %v", err)
		}
		if got != *c.Digest {
			t.Fatalf("WitnessBodyDigest = %s, want %s", got, *c.Digest)
		}
	case "record-validation":
		input := mustValue(t, c.Input)
		args := mustGet(t, input, "args")
		schemaRef, mode, body, causedBy, witness := recordParams(t, args)
		_, err := record.RecordInput(schemaRef, mode, body, causedBy, witness)
		expectErrContains(t, "RecordInput", err, *c.ExpectError)
		_, err = record.RecordDigest(schemaRef, mode, body, causedBy, witness)
		expectErrContains(t, "RecordDigest", err, *c.ExpectError)
	case "witness-validation":
		input := mustValue(t, c.Input)
		args := mustGet(t, input, "args")
		schemaRef := mustString(t, args, "schema_ref")
		body := mustGet(t, args, "body")
		expectErrContains(t, "ValidateWitnessBody", record.ValidateWitnessBody(body), *c.ExpectError)
		_, err := record.WitnessBodyDigest(schemaRef, body)
		expectErrContains(t, "WitnessBodyDigest", err, *c.ExpectError)
	case "value-error":
		v := mustValue(t, c.Input)
		_, err := marshal(v)
		expectErrContains(t, "marshal", err, *c.ExpectError)
		_, err = digest(v)
		expectErrContains(t, "digest", err, *c.ExpectError)
	case "decoder-bytes":
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
		_, derr := decode(payload)
		if c.ExpectError != nil {
			expectErrContains(t, "strict decode", derr, *c.ExpectError)
		} else if derr != nil {
			t.Fatalf("strict decode of canonical bytes: %v", derr)
		}
	default:
		t.Fatalf("unhandled class %q", class)
	}
}

func TestConformanceCorpus(t *testing.T) {
	want := map[string]string{}    // case name -> corpus file
	handled := map[string]string{} // case name -> class
	classCounts := map[string]int{}
	for _, file := range corpusFiles {
		for _, c := range loadFile(t, file) {
			if prev, dup := want[c.Name]; dup {
				t.Fatalf("duplicate corpus case name %q in %s and %s", c.Name, prev, file)
			}
			want[c.Name] = file
			class, ok := classify(t, c)
			if !ok {
				t.Fatalf("corpus case %s (%s): shape matches no known class; the gate does not know how to execute it", c.Name, file)
			}
			handled[c.Name] = class
			classCounts[class]++
			c := c
			t.Run(file+"/"+c.Name, func(t *testing.T) {
				runCase(t, c, class)
			})
		}
	}
	// Accounting: the handled set must equal the file set, by name.
	if len(handled) != len(want) {
		t.Fatalf("accounted for %d cases, corpus carries %d", len(handled), len(want))
	}
	for name := range want {
		if _, ok := handled[name]; !ok {
			t.Fatalf("corpus case %q was not executed", name)
		}
	}
	t.Logf("corpus cases executed: %d total", len(handled))
	for class, n := range classCounts {
		t.Logf("  class %-18s %d", class, n)
	}
}

// TestNamedGoldenDigests pins the three headline digests as literals
// in the gate itself, independent of the corpus tables: a corpus that
// drifted consistently would still have to match these.
func TestNamedGoldenDigests(t *testing.T) {
	pins := map[string]string{
		"golden/capture_record":           "sha256:c1130f7072093a8a923135960f418ce2259bfe2050190bf0a464e97ee80c9df5",
		"golden/root_witness.body_digest": "sha256:22e3bf15914b5007dea08a1079d1542470a369bcbed067687f345e7e85721130",
		"golden/root_witness.record":      "sha256:0b67497cb4c0f2ed954b7988ecc321b481e7d173c4fc201adeb86591b2fd6310",
	}
	seen := map[string]bool{}
	for _, c := range loadFile(t, "kernel_v1.json") {
		want, ok := pins[c.Name]
		if !ok {
			continue
		}
		seen[c.Name] = true
		if c.Digest == nil || *c.Digest != want {
			t.Fatalf("case %s digest = %v, want pinned %s", c.Name, c.Digest, want)
		}
	}
	for name := range pins {
		if !seen[name] {
			t.Fatalf("pinned case %q missing from kernel_v1.json", name)
		}
	}
	// The root record id is also computed by the API, never hardcoded.
	if got := record.RootWitnessRecordID(); got != pins["golden/root_witness.record"] {
		t.Fatalf("RootWitnessRecordID = %s, want %s", got, pins["golden/root_witness.record"])
	}
}

// TestCorpusManifestIntegrity pins the manifest's law versions and
// the recorded SHA-256 of every corpus case file: a hand-edited case
// file fails here even before its digests are recomputed.
func TestCorpusManifestIntegrity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(corpusDir(), "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		CanonicalVersion string            `json:"canonical_version"`
		CorpusVersion    int               `json:"corpus_version"`
		FileHashes       map[string]string `json:"file_hashes"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.CanonicalVersion != profileKernel {
		t.Fatalf("manifest canonical_version = %q, want %q", manifest.CanonicalVersion, profileKernel)
	}
	if manifest.CorpusVersion != 2 {
		t.Fatalf("manifest corpus_version = %d, want 2", manifest.CorpusVersion)
	}
	if len(manifest.FileHashes) != len(corpusFiles) {
		t.Fatalf("manifest file_hashes covers %d files, want %d", len(manifest.FileHashes), len(corpusFiles))
	}
	for _, file := range corpusFiles {
		want, ok := manifest.FileHashes[file]
		if !ok {
			t.Fatalf("manifest file_hashes missing %s", file)
		}
		raw, err := os.ReadFile(filepath.Join(corpusDir(), file))
		if err != nil {
			t.Fatalf("read corpus %s: %v", file, err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("corpus %s sha256 = %s, manifest records %s", file, got, want)
		}
	}
}
