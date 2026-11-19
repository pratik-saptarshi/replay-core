# Conformance corpus

Normative test vectors for replay-core's canonical-digest law. The Go
implementation is proven against this corpus bit-for-bit; the corpus is
the in-repo form of the law, together with the Global Constraints in
`docs/plans/2026-11-18-replay-core-phase-0-plan.html`.

- `manifest.json` — corpus version and the law versions it pins
  (kernel profile `replaycore.kernel.canonical.v1`; commons profile
  `commons.canonical.v1`), plus the source list for every case group.
- `kernel_v1.json` — kernel-profile cases: golden record/witness digests,
  the canonical byte-stability case, validation-error cases.
- `commons_v1.json` — commons-profile cases: shipped vectors plus error
  cases (float forbidden, duplicate key, non-canonical bytes).
- `numbers.json` — number-law cases: float/int edge values with their
  reference-canonical bytes and digests. Inputs are pre-wrapped objects
  (`{"value": v}`); consumers digest the wrapped object.

Each case is `{name, profile, input, canonical_text|canonical_bytes_b64,
digest, expect_error?}`. Case `canonical_text` is always present on digest
cases; `raw_canonical_json_b64` appears only on the byte-stability case
and carries the exact unprefixed canonical JSON bytes.

Governance: cases are sorted by name, carry no timestamps, and change
only by a reviewed regeneration, which bumps `corpus_version` in the
manifest. Generation tooling and its reference inputs are held outside
this repo. The Phase 0 gate recomputes every digest from its case input —
a hand-edited corpus fails the gate.
