package server

import (
	"connectrpc.com/connect"
	"testing"
	"time"
)

func TestNHA161CalendarQueryRejectsUnboundedAndOversizedWindows(t *testing.T) {
	r := nhf015NewCalendarRig(t, 30*time.Second)
	entity := r.create(t)
	missing := nhf015Week(r.name(t, entity))
	missing.Window = nil
	_, err := r.api.QueryOccurrences(r.ctx, nha162Bearer(missing, r.adminToken))
	nha162Code(t, err, connect.CodeInvalidArgument)
	oversized := nhf015Week(r.name(t, entity))
	oversized.Window.GetDates().EndDate.Year = 2036
	_, err = r.api.QueryOccurrences(r.ctx, nha162Bearer(oversized, r.adminToken))
	nha162Code(t, err, connect.CodeInvalidArgument)
}
