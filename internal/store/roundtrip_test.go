package store_test

import (
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The round-trip suite (panel W3-1, P1): prepareDrafts validated
// each draft's witness shape and computed its record, but never
// checked that the encoded bytes decode under the kernel profile
// — the validation every read path applies. Two of the canonical
// package's own constructors produce values the decoder refuses:
// ObjectOf keeps duplicate keys, and StringOf accepts invalid
// UTF-8. A draft built with them appended cleanly and then
// poisoned the store: GetRecord failed on the record, PathRecords
// failed for the whole path, and CollectGarbage failed on the
// same record — one public-API append wedged GC store-wide,
// permanently. The append now refuses any draft whose record
// bytes or witness bytes do not round-trip through the readers'
// own validation, before any write.

// poisonDraft builds a draft around caller-supplied body and
// witness values, so a test can hand the append values the
// canonical constructors admit but the kernel decoder refuses.
func poisonDraft(body, witnessBody canonical.Value) store.RecordDraft {
	return store.RecordDraft{
		SchemaRef:        "test.fact",
		Mode:             record.ModeCapture,
		Body:             body,
		WitnessSchemaRef: record.RootWitnessSchemaRef,
		WitnessBody:      witnessBody,
	}
}

func TestAppendRefusesRecordWithDuplicateKeysInBody(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()
	body := canonical.ObjectOf(
		canonical.Pair{Key: "n", Value: canonical.StringOf("a")},
		canonical.Pair{Key: "n", Value: canonical.StringOf("b")},
	)
	d := poisonDraft(body, rootFields(t))
	d.CausedBy = []string{rootID}
	_, err := s.AppendBatch("runs/rt", "intent-rt-dup-body", []store.RecordDraft{d})
	if err == nil {
		t.Fatal("AppendBatch accepted a record whose body carries duplicate keys; reads and GC would fail on it")
	}
	if !strings.Contains(err.Error(), "round-trip") {
		t.Fatalf("err = %q, want it to name the round-trip failure", err)
	}
	if n := pathCount(t, s, "runs/rt"); n != 0 {
		t.Fatalf("path has %d records after refused append, want 0", n)
	}
	// No intent row was consumed: the same intent ID accepts a
	// corrected batch.
	good := draftCiting(t, "test.fact", `{"n":1}`, []string{rootID})
	if _, err := s.AppendBatch("runs/rt", "intent-rt-dup-body", []store.RecordDraft{good}); err != nil {
		t.Fatalf("corrected retry under the same intent: %v", err)
	}
}

func TestAppendRefusesRecordWithInvalidUTF8InBody(t *testing.T) {
	s := openTemp(t)
	body := canonical.ObjectOf(
		canonical.Pair{Key: "n", Value: canonical.StringOf("bad\xffstring")},
	)
	d := poisonDraft(body, rootFields(t))
	_, err := s.AppendBatch("runs/rt", "intent-rt-utf8", []store.RecordDraft{d})
	if err == nil {
		t.Fatal("AppendBatch accepted a record whose body string is invalid UTF-8; reads and GC would fail on it")
	}
	if !strings.Contains(err.Error(), "round-trip") {
		t.Fatalf("err = %q, want it to name the round-trip failure", err)
	}
	if n := pathCount(t, s, "runs/rt"); n != 0 {
		t.Fatalf("path has %d records after refused append, want 0", n)
	}
}

func TestAppendFactRefusesWitnessBodyWithDuplicateKeys(t *testing.T) {
	s := openTemp(t)
	// The root witness fields with actor_ref listed twice:
	// ValidateWitnessBody sees every required field correctly
	// typed and passes, the digest computes — but the marshaled
	// witness bytes carry a duplicate key, so the read path's
	// DecodeKernel refuses them and every read of the record
	// would fail.
	fields := rootFields(t)
	pairs := make([]canonical.Pair, 0, len(fields.Pairs)+1)
	pairs = append(pairs, fields.Pairs...)
	pairs = append(pairs, canonical.Pair{Key: "actor_ref", Value: canonical.StringOf("kernel:root")})
	d := poisonDraft(
		canonical.ObjectOf(canonical.Pair{Key: "n", Value: canonical.StringOf("x")}),
		canonical.ObjectOf(pairs...),
	)
	if _, err := s.AppendFact("runs/rt", d); err == nil {
		t.Fatal("AppendFact accepted a witness body whose bytes carry duplicate keys; reads would fail on it")
	} else if !strings.Contains(err.Error(), "round-trip") {
		t.Fatalf("err = %q, want it to name the round-trip failure", err)
	}
	if n := pathCount(t, s, "runs/rt"); n != 0 {
		t.Fatalf("path has %d records after refused append, want 0", n)
	}
}

// TestAppendAcceptsExoticButLawfulBody is the control: values
// that are unusual but canonical — floats, an integer beyond
// float64 precision, astral-plane text — append, read back, and
// pass GC exactly as before. The round-trip refusal is specific
// to values the kernel decoder rejects, not to exotic content.
func TestAppendAcceptsExoticButLawfulBody(t *testing.T) {
	s := openTemp(t)
	big, err := canonical.IntOf("123456789012345678901234567890")
	if err != nil {
		t.Fatalf("IntOf: %v", err)
	}
	body := canonical.ObjectOf(
		canonical.Pair{Key: "astral", Value: canonical.StringOf("\U00010348\U0001F600")},
		canonical.Pair{Key: "big", Value: big},
		canonical.Pair{Key: "exp", Value: canonical.FloatOf(1e10)},
		canonical.Pair{Key: "frac", Value: canonical.FloatOf(0.1)},
	)
	receipt, err := s.AppendFact("runs/rt", poisonDraft(body, rootFields(t)))
	if err != nil {
		t.Fatalf("AppendFact over an exotic but lawful body: %v", err)
	}
	if _, err := s.GetRecord(receipt.RecordID); err != nil {
		t.Fatalf("GetRecord of the exotic record: %v", err)
	}
	if _, err := s.CollectGarbage(); err != nil {
		t.Fatalf("CollectGarbage over the exotic record: %v", err)
	}
}
