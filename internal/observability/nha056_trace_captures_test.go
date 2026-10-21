package observability

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA056TraceReservationsShareOneReleaseAndExpire(t *testing.T) {
	epoch, _, codec := nha056Fixture(t)
	now := time.Duration(0)
	codec.elapsed = func() time.Duration { return now }
	captures, err := NewTraceCaptures(epoch, codec)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewControllerHandle(epoch)
	if err != nil {
		t.Fatal(err)
	}
	binding := nha056Binding(t, 1)
	reservations := make([]CaptureReservation, 4)
	for i := range reservations {
		reservations[i], err = captures.Reserve(binding, owner, uint64(i+1))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := captures.Reserve(binding, owner, 5); err == nil {
		t.Fatal("fifth reservation allocated")
	}
	copyOfReservation := reservations[0]
	reservations[0].Release()
	copyOfReservation.Release()
	replacement, err := captures.Reserve(binding, owner, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Release()
	if _, err := captures.Reserve(binding, owner, 6); err == nil {
		t.Fatal("copied reservation released twice")
	}
	now = 60 * time.Second
	if _, err := captures.Reserve(binding, owner, 7); err != nil {
		t.Fatal("TTL retained reservations", err)
	}
	if captures.Usage().ChargedBytes() > 4<<20 {
		t.Fatal("capture budget")
	}
}

func TestNHA056TraceImagesAreBoundedPagedImmutableAndClearedAtTTL(t *testing.T) {
	epoch, _, codec := nha056Fixture(t)
	now := time.Duration(0)
	codec.elapsed = func() time.Duration { return now }
	captures, _ := NewTraceCaptures(epoch, codec)
	owner, _ := NewControllerHandle(epoch)
	binding := nha056Binding(t, 1)
	reservation, err := captures.Reserve(binding, owner, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release()
	at := timestamppb.New(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
	cuts, err := NewTraceCutMetadata(&obs.TraceCutMetadata{Coherence: obs.TraceCutCoherence_TRACE_CUT_COHERENCE_SEPARATE_OWNER_CUTS, Order: obs.TraceOrderState_TRACE_ORDER_STATE_HEALTHY, ControllerCapturedAt: at, NodeCapturedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.CommitTruncated(nil, cuts, obs.TraceTruncationReason_TRACE_TRUNCATION_REASON_UNSPECIFIED); err == nil {
		t.Fatal("zero operational truncation admitted")
	}
	rows := make([]TraceRow, 512)
	for i := range rows {
		wire := &obs.TraceRow{RunId: 1, LocalIndex: uint32(i), Ordinal: &obs.TraceOrdinal{Result: &obs.TraceOrdinal_Native{Native: uint64(i + 1)}}, Record: &obs.TraceRow_Controller{Controller: &obs.ControllerTraceRecord{Decision: obs.TraceDecision_TRACE_DECISION_FINISHED, MaxExceeded: &obs.ControllerTraceRecord_NotApplicable{NotApplicable: &emptypb.Empty{}}}}}
		rows[i], err = NewTraceRow(wire)
		if err != nil {
			t.Fatal(err)
		}
		wire.Ordinal.Result = &obs.TraceOrdinal_Native{Native: 9999}
	}
	handle, err := reservation.Commit(rows, cuts)
	if err != nil {
		t.Fatal(err)
	}
	copyOfReservation := reservation
	copyOfReservation.Release()
	size, _ := NewPageSize(100)
	request, _ := NewTracePageRequest(size, handle)
	total := 0
	for {
		result, err := captures.Read(request, binding)
		if err != nil {
			t.Fatal(err)
		}
		next, more := result.Next()
		token := ""
		if more {
			token, err = codec.EncodeTrace(next)
			if err != nil {
				t.Fatal(err)
			}
		}
		wire, err := result.Wire(token)
		if err != nil || contracts.AdmitObservabilityResponse(wire) != nil {
			t.Fatal("invalid trace wire page", err)
		}
		if len(wire.GetPage().Rows) > 100 {
			t.Fatal("page row budget")
		}
		for _, row := range wire.GetPage().Rows {
			if row.Ordinal.GetNative() != uint64(row.LocalIndex)+1 {
				t.Fatal("native ordinal renumbered")
			}
		}
		total += len(wire.GetPage().Rows)
		if !more {
			break
		}
		decoded, err := codec.DecodeTrace(token, binding, owner, 1)
		if err != nil {
			t.Fatal(err)
		}
		request, err = TracePageRequestFromCursor(size, decoded)
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != 512 {
		t.Fatalf("trace pages contained %d rows", total)
	}
	now = 60 * time.Second
	_ = captures.Usage()
	if reservation.state.rows != nil || reservation.state.cuts != nil || !copyOfReservation.state.closed {
		t.Fatal("expired reservation copy retained image")
	}
	if _, err := captures.Read(request, binding); err == nil {
		t.Fatal("expired image readable")
	}
}
