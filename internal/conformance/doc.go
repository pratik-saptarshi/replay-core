// Package conformance holds the Phase 0 conformance gate as Go tests:
// every case in the committed corpus under testdata/corpus is loaded,
// classified by its shape, and executed against the public canonical
// and record APIs. A corpus case that matches no known class fails
// the gate loudly, so the corpus and the gate cannot drift apart
// silently. The corpus is the normative form of the law; this package
// is its enforcement.
package conformance
