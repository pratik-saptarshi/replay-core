package canonical_test

import (
	"strings"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// The strict decoders accept exactly the bytes the matching Marshal
// produces — prefix included — and reject everything else. These
// tests pin the acceptance set (canonical bytes from the corpus) and
// the rejection classes (ordering, whitespace, escaping, number form,
// profile mixups, duplicate keys).

func TestDecodeKernelRoundTripCorpusBytes(t *testing.T) {
	// The corpus byte-stability case: prefix + raw_canonical_json_b64.
	const data = "replaycore.kernel.canonical.v1\n{\"a\":{\"snowman\":\"snowman-☃\"},\"z\":1}"
	v, err := canonical.DecodeKernel([]byte(data))
	if err != nil {
		t.Fatalf("DecodeKernel(corpus bytes): %v", err)
	}
	got, err := canonical.MarshalKernel(v)
	if err != nil {
		t.Fatalf("MarshalKernel(decoded): %v", err)
	}
	if string(got) != data {
		t.Fatalf("round trip =\n%q\nwant\n%q", got, data)
	}
	d, err := canonical.DigestKernel(v)
	if err != nil {
		t.Fatalf("DigestKernel(decoded): %v", err)
	}
	const wantDigest = "sha256:d370e6ba6d4f561c741a6c486cb063c37b5baf0e4536c928ff9099027b66e236"
	if d != wantDigest {
		t.Fatalf("digest = %s, want %s", d, wantDigest)
	}
}

func TestDecodeCommonsRoundTripCorpusVectors(t *testing.T) {
	// canonical_text of the four shipped commons vectors.
	vectors := []string{
		"commons.canonical.v1\n{\"n\":9007199254740993}",
		"commons.canonical.v1\n{\"a\":1,\"b\":[true,null,\"x\"]}",
		"commons.canonical.v1\nnull",
		"commons.canonical.v1\n{\"text\":\"caf\\u00e9 \\u2615\"}",
	}
	for _, data := range vectors {
		v, err := canonical.DecodeCommons([]byte(data))
		if err != nil {
			t.Fatalf("DecodeCommons(%q): %v", data, err)
		}
		got, err := canonical.MarshalCommons(v)
		if err != nil {
			t.Fatalf("MarshalCommons(decoded %q): %v", data, err)
		}
		if string(got) != data {
			t.Fatalf("round trip =\n%q\nwant\n%q", got, data)
		}
	}
}

func TestDecodeRejectsNonCanonicalBytes(t *testing.T) {
	cases := []struct {
		name    string
		decode  func([]byte) (canonical.Value, error)
		data    string
		wantErr string
	}{
		// Key order is law: unsorted keys are not canonical bytes.
		{"commons unsorted keys", canonical.DecodeCommons,
			"commons.canonical.v1\n{\"b\":1,\"a\":2}", "not in canonical byte form"},
		{"kernel unsorted keys", canonical.DecodeKernel,
			"replaycore.kernel.canonical.v1\n{\"b\":1,\"a\":2}", "not in canonical byte form"},
		// Whitespace anywhere outside strings is not canonical.
		{"kernel added whitespace", canonical.DecodeKernel,
			"replaycore.kernel.canonical.v1\n{ \"a\": 1 }", "not in canonical byte form"},
		{"commons trailing newline", canonical.DecodeCommons,
			"commons.canonical.v1\n{\"a\":1}\n", "not in canonical byte form"},
		// The kernel profile emits raw UTF-8; an escaped snowman is a
		// different byte string and must not decode as kernel bytes.
		{"kernel escaped snowman", canonical.DecodeKernel,
			"replaycore.kernel.canonical.v1\n{\"s\":\"\\u2603\"}", "not in canonical byte form"},
		// Number forms are law: 1e2 parses to a float whose canonical
		// rendering is 100.0, so the literal is not canonical bytes.
		{"kernel exponent literal", canonical.DecodeKernel,
			"replaycore.kernel.canonical.v1\n{\"n\":1e2}", "not in canonical byte form"},
		// Profiles are never conflated: the prefix is part of the bytes.
		{"kernel bytes to commons decoder", canonical.DecodeCommons,
			"replaycore.kernel.canonical.v1\n{\"a\":1}", "prefix"},
		{"commons bytes to kernel decoder", canonical.DecodeKernel,
			"commons.canonical.v1\n{\"a\":1}", "prefix"},
		{"no prefix at all", canonical.DecodeKernel,
			"{\"a\":1}", "prefix"},
		// Duplicate keys have no canonical meaning (corpus case
		// error/duplicate_key, exercised here at the decoder).
		{"commons duplicate keys", canonical.DecodeCommons,
			"commons.canonical.v1\n{\"a\":1,\"a\":2}", "duplicate canonical object key"},
		// A float under the commons profile is out of law even in
		// otherwise canonical-looking bytes.
		{"commons float", canonical.DecodeCommons,
			"commons.canonical.v1\n{\"x\":1.5}", "floats are forbidden in canonical inputs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.decode([]byte(c.data)); err == nil {
				t.Fatalf("decoded non-canonical bytes %q", c.data)
			} else if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), c.wantErr)
			}
		})
	}
}
