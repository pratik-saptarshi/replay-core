# replay-core

Native (Go) replay capability for AEP. Design intent: an execution store
that records a run's effects by value and can replay them by digest match,
fork a run at any recorded cut with one input amended, and settle workspace
changes fail-closed. Phase 0 (current) builds only the canonical-digest
law and its conformance corpus — no runtime exists yet; see Status.

Charter: replay-core is an **execution store**, not a memory system.
NeuroStrata remains the memory of record; replay-core inscribes pointers
(trace ids, cut addresses) into NeuroStrata at run close, never the reverse.
No search/semantic APIs live here — retrieval is NeuroStrata's job.

Claims discipline: every capability statement in these docs cites the test
that proves it. No unverified performance or cache-reuse numbers.

Status: Phase 0 — canonical-digest law + conformance fixtures (no runtime).
The canonical codec (`internal/canonical`) and the record/witness API
(`internal/record`) pass every case in the committed conformance corpus
with zero skipped cases; `scripts/check-phase0.sh` is the Phase 0 gate
(gofmt, vet, the full test suite, corpus integrity against the manifest,
and the named golden digests).
Plan: `docs/plans/2026-11-18-replay-core-phase-0-plan.html`;
semantics: `docs/replay-semantics.html`;
conformance corpus: `testdata/corpus/` (see its README).
