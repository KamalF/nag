package notify

import (
	"strings"
	"testing"
)

// §4.4: a cleared row older than retention_days is deleted by the sweep;
// one at exactly the boundary is not — the comparison is strict.
func TestPurgeBoundary(t *testing.T) {
	f := newFixture(t)
	f.retention = 30
	old := f.create("old", -40*86400)
	edge := f.create("edge", -40*86400)
	open := f.create("still in the list", -40*86400)
	f.clearAt(old.ID, -30*86400-1)
	f.clearAt(edge.ID, -30*86400)

	f.tickAt(0)

	for id, want := range map[int64]bool{old.ID: false, edge.ID: true, open.ID: true} {
		if _, err := f.st.GetReminder(t.Context(), id); (err == nil) != want {
			t.Errorf("row %d present = %v, want %v", id, err == nil, want)
		}
	}
	if !strings.Contains(f.logs.String(), "purged cleared reminders") || !strings.Contains(f.logs.String(), "count=1") {
		t.Errorf("no purge line: %s", f.logs.String())
	}
}

// retention_days = 0 means keep cleared reminders forever: the DELETE is
// skipped entirely.
func TestPurgeZeroKeepsForever(t *testing.T) {
	f := newFixture(t)
	f.retention = 0
	r := f.create("ancient", -4000*86400)
	f.clearAt(r.ID, -3999*86400)

	f.tickAt(0)

	if _, err := f.st.GetReminder(t.Context(), r.ID); err != nil {
		t.Errorf("retention 0 purged a row: %v", err)
	}
}

func (f *fixture) clearAt(id, offset int64) {
	f.t.Helper()
	if _, err := f.st.MarkDone(f.t.Context(), id, t0+offset); err != nil {
		f.t.Fatal(err)
	}
}
