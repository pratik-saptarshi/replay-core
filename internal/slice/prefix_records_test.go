package slice_test

import (
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/slice"
	"github.com/pratik-saptarshi/replay-core/internal/store"
)

// The ResolveCut leg of the prefix-membership suite (panel
// W3-3): ResolveCut selected the prefix through PathRecords,
// whose membership came from path_entries, while the ReadCut
// cross-check (issue #6) validates the cut body against the
// records table. Moving only the through record's path_entries
// row to another path left ReadCut green while ResolveCut
// silently omitted its own through record. PathRecords now
// derives membership from the records table — the placement
// truth the cross-check reads — so the tamper changes nothing.

func TestResolveCutIgnoresPathEntriesTamper(t *testing.T) {
	path, cutID, through := seedCutFixture(t)
	rawStoreExec(t, path, "UPDATE path_entries SET owner_path = 'runs/other' WHERE record_id = ?", through)

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	if _, err := slice.ReadCut(s, cutID); err != nil {
		t.Fatalf("ReadCut over tampered path_entries: %v", err)
	}
	sl, err := slice.ResolveCut(s, cutID, nil)
	if err != nil {
		t.Fatalf("ResolveCut over tampered path_entries: %v", err)
	}
	order := sl.Order()
	if len(order) != 3 {
		t.Fatalf("ResolveCut returned %d records, want 3 (root, f1, through): %v", len(order), order)
	}
	found := false
	for _, id := range order {
		if id == through {
			found = true
		}
	}
	if !found {
		t.Fatalf("ResolveCut omitted its own through record %s after the path_entries tamper: %v", through, order)
	}
}
