package store_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
	"github.com/pratik-saptarshi/replay-core/internal/record"
	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

func TestGCSurvival(t *testing.T) { // AC-F2-7
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()

	// Fixture: main path runs/main with a published cut mid-chain.
	// Ordinals: r1=0, r2=1, cut=2, milestone=3, s1=4, s2=5, s3=6,
	// r3=7, r4=8 (frontier tail). The cut runs through r2.
	r1, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"r1"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append r1: %v", err)
	}
	r2, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"r2"}`, []string{r1.RecordID}))
	if err != nil {
		t.Fatalf("append r2: %v", err)
	}
	cutID, err := slice.PublishCut(s, "runs/main", r2.RecordID)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}
	milestone, err := s.AppendMilestone("runs/main", "task-gc", "snap-gc", []string{r2.RecordID})
	if err != nil {
		t.Fatalf("AppendMilestone: %v", err)
	}

	// Side branch: 3 records citing a main-path record (r1) but
	// cited by nothing — unreachable from any retention root: the
	// frontier tail's ancestry runs r4→r3→r2→r1, never through the
	// branch, and no cut or milestone reaches it either.
	s1, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"s1"}`, []string{r1.RecordID}))
	if err != nil {
		t.Fatalf("append s1: %v", err)
	}
	s2, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"s2"}`, []string{s1.RecordID}))
	if err != nil {
		t.Fatalf("append s2: %v", err)
	}
	s3, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"s3"}`, []string{s2.RecordID}))
	if err != nil {
		t.Fatalf("append s3: %v", err)
	}

	r3, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"r3"}`, []string{r2.RecordID}))
	if err != nil {
		t.Fatalf("append r3: %v", err)
	}
	r4, err := s.AppendFact("runs/main", draftCiting(t, "test.fact", `{"n":"r4"}`, []string{r3.RecordID}))
	if err != nil {
		t.Fatalf("append r4: %v", err)
	}

	// Resolve the cut and record the slice digest before GC.
	before, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut before GC: %v", err)
	}
	digestBefore, err := before.Digest()
	if err != nil {
		t.Fatalf("slice Digest before GC: %v", err)
	}

	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.Collected != 3 {
		t.Fatalf("Collected = %d, want 3 (the side branch)", report.Collected)
	}
	if report.Kept != 7 {
		t.Fatalf("Kept = %d, want 7 (root, r1, r2, cut, milestone, r3, r4)", report.Kept)
	}

	// Re-resolve the cut: the digest is identical to pre-GC and
	// every sliced record is still GetRecord-able.
	after, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut after GC: %v", err)
	}
	digestAfter, err := after.Digest()
	if err != nil {
		t.Fatalf("slice Digest after GC: %v", err)
	}
	if digestAfter != digestBefore {
		t.Fatalf("cut digest changed across GC: before %s, after %s", digestBefore, digestAfter)
	}
	for _, rec := range before.Records {
		if _, err := s.GetRecord(rec.ID); err != nil {
			t.Fatalf("sliced record %s not readable after GC: %v", rec.ID, err)
		}
	}

	// Each side-branch record is gone.
	for _, id := range []string{s1.RecordID, s2.RecordID, s3.RecordID} {
		if _, err := s.GetRecord(id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetRecord(%s) after GC: err = %v, want ErrNotFound", id, err)
		}
	}

	// The frontier tail and the milestone record survive.
	tail, err := s.Frontier("runs/main")
	if err != nil {
		t.Fatalf("Frontier after GC: %v", err)
	}
	if tail != r4.RecordID {
		t.Fatalf("frontier tail after GC = %s, want %s", tail, r4.RecordID)
	}
	if _, err := s.GetRecord(r4.RecordID); err != nil {
		t.Fatalf("frontier tail not readable after GC: %v", err)
	}
	if _, err := s.GetRecord(milestone.RecordID); err != nil {
		t.Fatalf("milestone not readable after GC: %v", err)
	}
}

// sliceIDs returns the ordered record IDs of a resolved slice.
func sliceIDs(sl slice.Slice) []string {
	ids := make([]string, len(sl.Records))
	for i, rec := range sl.Records {
		ids[i] = rec.ID
	}
	return ids
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestGCRetainsCutPrefix pins the per-class reachability law for
// published cuts (Codex P1 on PR #2): a cut addresses an ordinal
// prefix of its path — ResolveCut returns every record in the
// prefix plus causal closure — so GC must retain that whole set,
// not just the cut record's causal ancestors. An independent
// in-prefix fact is resolvable through the cut; collecting it
// silently changed the cut's contents and digest.
func TestGCRetainsCutPrefix(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()

	// A and B are independent facts on one path: both cite only
	// the root, never each other. Ordinals: A=0, B=1, cut=2.
	a, err := s.AppendFact("runs/prefix", draftCiting(t, "test.fact", `{"n":"a"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append A: %v", err)
	}
	b, err := s.AppendFact("runs/prefix", draftCiting(t, "test.fact", `{"n":"b"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append B: %v", err)
	}
	cutID, err := slice.PublishCut(s, "runs/prefix", b.RecordID)
	if err != nil {
		t.Fatalf("PublishCut: %v", err)
	}

	// D (ordinal 3) sits beyond the cut and outside the frontier
	// tail's closure once E (ordinal 4, citing only the root)
	// takes the frontier: D is genuinely unreachable and must
	// still be collected — the prefix law retains what the cut
	// addresses, not the whole path.
	d, err := s.AppendFact("runs/prefix", draftCiting(t, "test.fact", `{"n":"d"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append D: %v", err)
	}
	if _, err := s.AppendFact("runs/prefix", draftCiting(t, "test.fact", `{"n":"e"}`, []string{rootID})); err != nil {
		t.Fatalf("append E: %v", err)
	}

	before, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut before GC: %v", err)
	}
	digestBefore, err := before.Digest()
	if err != nil {
		t.Fatalf("slice Digest before GC: %v", err)
	}
	if !equalIDs(sliceIDs(before), []string{rootID, a.RecordID, b.RecordID}) {
		t.Fatalf("cut slice before GC = %v, want [root, A, B]", sliceIDs(before))
	}

	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.Collected != 1 {
		t.Fatalf("Collected = %d, want 1 (only D, the unreachable post-cut fact)", report.Collected)
	}
	if _, err := s.GetRecord(a.RecordID); err != nil {
		t.Fatalf("independent in-prefix fact A collected despite the cut: %v", err)
	}
	if _, err := s.GetRecord(d.RecordID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecord(D) after GC: err = %v, want ErrNotFound", err)
	}

	after, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut after GC: %v", err)
	}
	digestAfter, err := after.Digest()
	if err != nil {
		t.Fatalf("slice Digest after GC: %v", err)
	}
	if digestAfter != digestBefore {
		t.Fatalf("cut digest changed across GC: before %s, after %s", digestBefore, digestAfter)
	}
	if !equalIDs(sliceIDs(after), sliceIDs(before)) {
		t.Fatalf("cut membership changed across GC: before %v, after %v", sliceIDs(before), sliceIDs(after))
	}
}

// TestGCEpochWindowUnderClosureLaw pins the ruled reachability
// law for milestones (controller ruling correcting the milestone
// half of the Codex P1 response, PR #2): a milestone is a
// retention root via its caused_by closure only. Unlike a
// published cut, a milestone publishes no immutable address —
// EpochSlice is a derived view over current state — so a
// non-causal record inside an epoch window remains collectable,
// and pruning it is AC-F2-7's purpose. What must survive is the
// milestone record itself, body intact: the snapshot citation
// lives in the body, and snapshot.SnapshotAt reads it.
func TestGCEpochWindowUnderClosureLaw(t *testing.T) {
	s := openTemp(t)
	rootID := record.RootWitnessRecordID()

	// Ordinals on runs/epoch: r1=0, m1=1, X=2, m2=3, Z=4, T=5.
	// m1 and m2 each cite r1, so r1 is in their causal closures.
	// X cites only the root: it sits inside m2's prefix (the
	// epoch window m1..m2) but in no causal closure. Z is beyond
	// the window and likewise closure-free. T takes the
	// frontier, citing only the root.
	r1, err := s.AppendFact("runs/epoch", draftCiting(t, "test.fact", `{"n":"r1"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append r1: %v", err)
	}
	m1, err := s.AppendMilestone("runs/epoch", "task-1", "snap-1", []string{r1.RecordID})
	if err != nil {
		t.Fatalf("AppendMilestone m1: %v", err)
	}
	x, err := s.AppendFact("runs/epoch", draftCiting(t, "test.fact", `{"n":"x"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append X: %v", err)
	}
	m2, err := s.AppendMilestone("runs/epoch", "task-2", "snap-2", []string{r1.RecordID})
	if err != nil {
		t.Fatalf("AppendMilestone m2: %v", err)
	}
	z, err := s.AppendFact("runs/epoch", draftCiting(t, "test.fact", `{"n":"z"}`, []string{rootID}))
	if err != nil {
		t.Fatalf("append Z: %v", err)
	}
	if _, err := s.AppendFact("runs/epoch", draftCiting(t, "test.fact", `{"n":"t"}`, []string{rootID})); err != nil {
		t.Fatalf("append T: %v", err)
	}

	// Before GC the epoch window reads [X, m2]: X starts out
	// inside the window.
	before, err := slice.EpochSlice(s, "runs/epoch", m1.RecordID, m2.RecordID)
	if err != nil {
		t.Fatalf("EpochSlice before GC: %v", err)
	}
	if !equalIDs(sliceIDs(before), []string{x.RecordID, m2.RecordID}) {
		t.Fatalf("epoch before GC = %v, want [X, m2]", sliceIDs(before))
	}

	report, err := s.CollectGarbage()
	if err != nil {
		t.Fatalf("CollectGarbage: %v", err)
	}
	if report.Collected != 2 {
		t.Fatalf("Collected = %d, want 2 (X and Z, the closure-free facts)", report.Collected)
	}

	// The milestones and their causal closure survive.
	for name, id := range map[string]string{"r1": r1.RecordID, "m1": m1.RecordID, "m2": m2.RecordID} {
		if _, err := s.GetRecord(id); err != nil {
			t.Fatalf("%s not readable after GC: %v", name, err)
		}
	}

	// The milestone record's body — the snapshot citation
	// SnapshotAt consumes — survives byte-identical.
	m2Rec, err := s.GetRecord(m2.RecordID)
	if err != nil {
		t.Fatalf("GetRecord(m2) after GC: %v", err)
	}
	var m2Body *canonical.Value
	for _, p := range m2Rec.Record.Pairs {
		if p.Key == "body" {
			v := p.Value
			m2Body = &v
		}
	}
	if m2Body == nil {
		t.Fatalf("milestone record has no body field after GC")
	}
	gotBody, err := canonical.MarshalKernel(*m2Body)
	if err != nil {
		t.Fatalf("marshal m2 body: %v", err)
	}
	wantBody, err := canonical.MarshalKernel(store.MilestoneBody("task-2", "snap-2"))
	if err != nil {
		t.Fatalf("marshal want body: %v", err)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("milestone body changed across GC: got %s, want %s", gotBody, wantBody)
	}

	// X IS collected, even though it sat inside the epoch
	// window: epochs are views over current state, not
	// immutable addresses, so an epoch confers no retention
	// beyond the milestones' own causal closures.
	if _, err := s.GetRecord(x.RecordID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecord(X) after GC: err = %v, want ErrNotFound (in-window but closure-free)", err)
	}
	if _, err := s.GetRecord(z.RecordID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRecord(Z) after GC: err = %v, want ErrNotFound", err)
	}

	// The post-GC epoch is the view over what remains: [m2].
	after, err := slice.EpochSlice(s, "runs/epoch", m1.RecordID, m2.RecordID)
	if err != nil {
		t.Fatalf("EpochSlice after GC: %v", err)
	}
	if !equalIDs(sliceIDs(after), []string{m2.RecordID}) {
		t.Fatalf("epoch after GC = %v, want [m2] (view over current state)", sliceIDs(after))
	}
}
