// Package record implements replay-core's record and witness digest
// law on top of the canonical encoder: the shapes that may be
// digested, and the validation that must pass before any digest is
// computed. The conformance corpus under testdata/corpus is the
// normative form of the law, including the validation-error cases.
package record

import (
	"bytes"
	"fmt"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// Record modes. The law admits exactly these two.
const (
	ModeCapture     = "capture"
	ModeDeclaration = "declaration"
)

// RootWitnessSchemaRef is the schema of the genesis witness: the one
// record whose witness field is the empty-string sentinel and whose
// body is fixed by the law.
const RootWitnessSchemaRef = "kernel.witness.root.v1"

// ValidationError is a record or witness shape violation. Messages
// carry the reference law's wording; the corpus's expect_error
// substrings match against them.
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) *ValidationError {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// RecordInput validates a record's parts and builds the record object
// that is digested:
//
//	{"body": ..., "caused_by": [...], "kind": "record",
//	 "mode": ..., "schema_ref": ..., "witness": ...}
//
// Validation runs before the object is built, in this order: the mode
// must be capture or declaration; a root-witness record (schema
// RootWitnessSchemaRef) must carry exactly the root witness body,
// mode capture, the empty witness sentinel, and no causal parents;
// any other record must carry a non-empty witness — the empty
// sentinel is reserved for the root witness record.
func RecordInput(schemaRef, mode string, body canonical.Value, causedBy []string, witness string) (canonical.Value, error) {
	if mode != ModeCapture && mode != ModeDeclaration {
		return canonical.Value{}, invalid("record: invalid mode %q: mode must be %q or %q", mode, ModeCapture, ModeDeclaration)
	}
	if schemaRef == RootWitnessSchemaRef {
		if err := validateRootRecord(mode, body, causedBy, witness); err != nil {
			return canonical.Value{}, err
		}
	} else if witness == "" {
		return canonical.Value{}, invalid("record: empty witness is permitted only for the root witness record")
	}
	parents := make([]canonical.Value, len(causedBy))
	for i, p := range causedBy {
		parents[i] = canonical.StringOf(p)
	}
	return canonical.ObjectOf(
		canonical.Pair{Key: "body", Value: body},
		canonical.Pair{Key: "caused_by", Value: canonical.ArrayOf(parents...)},
		canonical.Pair{Key: "kind", Value: canonical.StringOf("record")},
		canonical.Pair{Key: "mode", Value: canonical.StringOf(mode)},
		canonical.Pair{Key: "schema_ref", Value: canonical.StringOf(schemaRef)},
		canonical.Pair{Key: "witness", Value: canonical.StringOf(witness)},
	), nil
}

// RecordDigest validates a record's parts and returns the
// kernel-profile digest of the record object.
func RecordDigest(schemaRef, mode string, body canonical.Value, causedBy []string, witness string) (string, error) {
	rec, err := RecordInput(schemaRef, mode, body, causedBy, witness)
	if err != nil {
		return "", err
	}
	return canonical.DigestKernel(rec)
}

// validateRootRecord enforces the genesis shape: the body must equal
// the root witness body exactly (compared by canonical bytes, so any
// field difference is a violation), the mode must be capture, the
// witness must be the empty sentinel, and there must be no causal
// parents — the root has no predecessors.
func validateRootRecord(mode string, body canonical.Value, causedBy []string, witness string) error {
	got, err := canonical.MarshalKernel(body)
	if err != nil {
		return invalid("record: root witness record body must equal root_witness_body")
	}
	want, err := canonical.MarshalKernel(rootWitnessFields())
	if err != nil {
		return err // unreachable: the law's own constant marshals
	}
	if !bytes.Equal(got, want) {
		return invalid("record: root witness record body must equal root_witness_body")
	}
	if mode != ModeCapture {
		return invalid("record: root witness record must use mode %q, got %q", ModeCapture, mode)
	}
	if witness != "" {
		return invalid("record: root witness record must carry the empty witness sentinel, got %q", witness)
	}
	if len(causedBy) != 0 {
		return invalid("record: root witness record must not have causal parents")
	}
	return nil
}

// witnessFieldOrder lists the witness body's required fields in
// canonical (alphabetical) order; witnessRefListFields is the subset
// that must be arrays of strings (the rest must be strings).
var (
	witnessRefListFields = []string{"active_binding_refs", "authority_refs", "provenance_policy_refs", "semantic_environment_refs", "visibility_policy_refs"}
	witnessFieldOrder    = []string{"active_binding_refs", "actor_ref", "authority_refs", "containment", "provenance_policy_refs", "semantic_environment_refs", "substrate_ref", "visibility_policy_refs"}
	validContainments    = map[string]bool{"contained": true, "full": true}
)

// ValidateWitnessBody enforces the witness body shape: an object
// whose eight required fields are present and correctly typed —
// actor_ref, containment and substrate_ref are strings; the five
// *_refs fields are arrays of strings — and whose containment is
// "contained" or "full".
func ValidateWitnessBody(body canonical.Value) error {
	if body.Kind != canonical.KindObject {
		return invalid("witness body: must be a JSON object")
	}
	for _, name := range witnessFieldOrder {
		field, ok := lookupField(body, name)
		if !ok {
			return invalid("witness body: missing required field %q", name)
		}
		if isRefListField(name) {
			if field.Kind != canonical.KindArray {
				return invalid("witness body: field %q must be an array of strings", name)
			}
			for _, e := range field.Elems {
				if e.Kind != canonical.KindString {
					return invalid("witness body: field %q must be an array of strings", name)
				}
			}
			continue
		}
		if field.Kind != canonical.KindString {
			return invalid("witness body: field %q must be a string", name)
		}
		if name == "containment" && !validContainments[field.Str] {
			return invalid("witness body: containment must be \"contained\" or \"full\", got %q", field.Str)
		}
	}
	return nil
}

func isRefListField(name string) bool {
	for _, f := range witnessRefListFields {
		if f == name {
			return true
		}
	}
	return false
}

// WitnessBodyDigest validates the witness body, then returns the
// kernel-profile digest of the witness object:
//
//	{"body": ..., "kind": "witness", "schema_ref": ...}
//
// No digest is ever computed over an unvalidated body.
func WitnessBodyDigest(schemaRef string, body canonical.Value) (string, error) {
	if err := ValidateWitnessBody(body); err != nil {
		return "", err
	}
	return canonical.DigestKernel(witnessObject(schemaRef, body))
}

func witnessObject(schemaRef string, body canonical.Value) canonical.Value {
	return canonical.ObjectOf(
		canonical.Pair{Key: "body", Value: body},
		canonical.Pair{Key: "kind", Value: canonical.StringOf("witness")},
		canonical.Pair{Key: "schema_ref", Value: canonical.StringOf(schemaRef)},
	)
}

// RootWitnessBody returns the genesis witness object: the root
// witness fields wrapped with kind "witness" and the root schema.
// Its kernel-profile digest is the law's ground truth
// sha256:22e3bf15… (asserted in the tests and the phase gate).
func RootWitnessBody() canonical.Value {
	return witnessObject(RootWitnessSchemaRef, rootWitnessFields())
}

// rootWitnessFields is the fixed body of the genesis witness.
func rootWitnessFields() canonical.Value {
	empty := canonical.ArrayOf()
	return canonical.ObjectOf(
		canonical.Pair{Key: "active_binding_refs", Value: empty},
		canonical.Pair{Key: "actor_ref", Value: canonical.StringOf("kernel:root")},
		canonical.Pair{Key: "authority_refs", Value: empty},
		canonical.Pair{Key: "containment", Value: canonical.StringOf("full")},
		canonical.Pair{Key: "provenance_policy_refs", Value: empty},
		canonical.Pair{Key: "semantic_environment_refs", Value: empty},
		canonical.Pair{Key: "substrate_ref", Value: canonical.StringOf("kernel")},
		canonical.Pair{Key: "visibility_policy_refs", Value: empty},
	)
}

// RootWitnessRecordID returns the digest of the genesis record: the
// root witness body captured with no causal parents and the empty
// witness sentinel. It is computed through RecordDigest, not
// hardcoded; the tests pin it to the ground truth sha256:0b67497c….
func RootWitnessRecordID() string {
	id, err := RecordDigest(RootWitnessSchemaRef, ModeCapture, rootWitnessFields(), nil, "")
	if err != nil {
		panic(fmt.Sprintf("record: root witness record failed validation: %v", err))
	}
	return id
}

// CommonsObjectDigest returns the commons-profile digest of v. It is
// a deliberate passthrough: commons objects are plain canonical
// values, and their digest law is the encoder's.
func CommonsObjectDigest(v canonical.Value) (string, error) {
	return canonical.DigestCommons(v)
}

func lookupField(v canonical.Value, key string) (canonical.Value, bool) {
	for _, p := range v.Pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return canonical.Value{}, false
}
