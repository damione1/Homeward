package calendars

import "testing"

// A configured remote source exists before it has admitted its first document.
// These assertions use only the current read API. The explicit NeverValidated
// quality on snapshots and empty pages needs its own tests once that API exists.
func TestNHA101BoundRemoteReadableBeforeFirstFetch(t *testing.T) {
	f := nha100Setup(t)
	local := f.calendar(t, "calendar.family")
	remote := f.bind(t, "calendar.famille")
	if remote.Origin() != RemoteICS || remote == local {
		t.Fatal("the real registry binding did not produce a distinct RemoteICS CalendarID")
	}

	t.Run("ListedBesideLocal", func(t *testing.T) {
		snapshots, err := f.service.ListCalendars(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		counts := make(map[CalendarID]int)
		for _, snapshot := range snapshots {
			if !snapshot.Valid() {
				t.Fatal("ListCalendars returned an invalid snapshot")
			}
			counts[snapshot.ID()]++
		}
		if counts[remote] != 1 || counts[local] != 1 || len(snapshots) != 2 {
			t.Fatalf("bound sources before first fetch: remote=%d local=%d total=%d; want 1, 1, 2", counts[remote], counts[local], len(snapshots))
		}
	})

	t.Run("Metadata", func(t *testing.T) {
		snapshot, err := f.service.GetCalendar(f.ctx, remote)
		if err != nil {
			t.Fatal(err)
		}
		if !snapshot.Valid() || snapshot.ID() != remote {
			t.Fatal("GetCalendar lost the bound RemoteICS identity")
		}
		if name, present := snapshot.Name(); !present || name == "" {
			t.Fatal("bound RemoteICS source has no readable name before its first fetch")
		}
	})

	t.Run("EmptyPage", func(t *testing.T) {
		window := nha100Window(t, "2026-01-01", "2026-01-03")
		page, err := f.service.Query(f.ctx, remote, window, 1, Cursor{})
		if err != nil {
			t.Fatalf("bound RemoteICS source must be readable before its first fetch: %v", err)
		}
		if len(page.Items()) != 0 {
			t.Fatal("a source with no admitted document fabricated occurrences")
		}
		if _, more := page.Next(); more {
			t.Fatal("a source with no admitted document fabricated a continuation")
		}
		calendar, err := f.service.GetCalendar(f.ctx, remote)
		if err != nil {
			t.Fatal(err)
		}
		if number, valid := page.Revision().Number(); !valid || number == 0 || page.Revision() != calendar.Revision() {
			t.Fatal("empty remote page is not bound to the readable source revision")
		}
	})
}
