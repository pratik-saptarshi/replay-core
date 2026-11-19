package store_test

import (
	"testing"

	"github.com/pratik-saptarshi/replay-core/internal/canonical"
)

// mustObject returns the object value stored under key in v,
// failing the test if the key is absent or not an object.
func mustObject(t *testing.T, v canonical.Value, key string) canonical.Value {
	t.Helper()
	for _, p := range v.Pairs {
		if p.Key == key {
			if p.Value.Kind != canonical.KindObject {
				t.Fatalf("key %q: kind = %v, want object", key, p.Value.Kind)
			}
			return p.Value
		}
	}
	t.Fatalf("object missing key %q", key)
	return canonical.Value{}
}

// mustString returns the string value stored under key in v,
// failing the test if the key is absent or not a string.
func mustString(t *testing.T, v canonical.Value, key string) string {
	t.Helper()
	for _, p := range v.Pairs {
		if p.Key == key {
			if p.Value.Kind != canonical.KindString {
				t.Fatalf("key %q: kind = %v, want string", key, p.Value.Kind)
			}
			return p.Value.Str
		}
	}
	t.Fatalf("object missing key %q", key)
	return ""
}

func TestAppendMilestoneRoundTrip(t *testing.T) { // AC-F2-8
	s := openTemp(t)
	r, err := s.AppendMilestone("runs/a", "task-7", "c0ffee1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.GetRecord(r.RecordID)
	if err != nil {
		t.Fatalf("milestone not addressable by digest: %v", err)
	}
	body := mustObject(t, rec.Record, "body")
	if got := mustString(t, body, "task_id"); got != "task-7" {
		t.Fatalf("task_id = %q", got)
	}
	if got := mustString(t, body, "snapshot_ref"); got != "c0ffee1234" {
		t.Fatalf("snapshot_ref = %q", got)
	}
	next, err := s.AppendFact("runs/a", draftCiting(t, "test.fact", `{"n":1}`, []string{r.RecordID}))
	if err != nil {
		t.Fatalf("successor cannot cite milestone: %v", err)
	}
	_ = next
}

func TestMilestoneUnknownTaskAccepted(t *testing.T) { // AC-F2-9: claimed, not enforced
	s := openTemp(t)
	if _, err := s.AppendMilestone("runs/a", "task-that-exists-in-no-registry", "abc", nil); err != nil {
		t.Fatalf("store enforced a task boundary: %v", err)
	}
}
