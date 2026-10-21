package remotecalendar

import (
	"testing"
	"time"
)

func TestNHA101PublicationDeadlineZeroAndTerminalBounds(t *testing.T) {
	if _, _, _, _, ok := (PublicationWindow{}).Deadline(); ok {
		t.Fatal("zero publication admitted")
	}
	wall := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	p := PublicationWindow{wall: wall, until: wall.Add(time.Hour), mono: time.Hour, untilMono: 2 * time.Hour}
	// A deadline alone cannot manufacture an admitted calendar window.
	if _, _, _, _, ok := p.Deadline(); ok {
		t.Fatal("deadline without an admitted image became a publication")
	}
}
