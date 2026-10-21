package observability

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type TraceRow struct{ wire *obs.TraceRow }

func NewTraceRow(value *obs.TraceRow) (TraceRow, error) {
	if value == nil || contracts.AdmitObservabilityValue(value) != nil || proto.Size(value) > 1024 {
		return TraceRow{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	return TraceRow{wire: proto.Clone(value).(*obs.TraceRow)}, nil
}
func (r TraceRow) Wire() (*obs.TraceRow, error) {
	if r.wire == nil {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	return proto.Clone(r.wire).(*obs.TraceRow), nil
}

type TraceCutMetadata struct{ wire *obs.TraceCutMetadata }

func NewTraceCutMetadata(value *obs.TraceCutMetadata) (TraceCutMetadata, error) {
	if value == nil || contracts.AdmitObservabilityValue(value) != nil {
		return TraceCutMetadata{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	return TraceCutMetadata{wire: proto.Clone(value).(*obs.TraceCutMetadata)}, nil
}

type captureState struct {
	parent                       *TraceCaptures
	id                           [16]byte
	binding                      ReadBinding
	controller                   ControllerHandle
	run                          uint64
	deadline                     time.Duration
	rows                         []*obs.TraceRow
	cuts                         *obs.TraceCutMetadata
	truncation                   obs.TraceTruncationReason
	truncated, committed, closed bool
}
type TraceCaptures struct {
	mu    sync.Mutex
	epoch ProcessObservationEpoch
	codec *CursorCodec
	slots [4]*captureState
}

const captureReservationBytes = ((4 << 20) - uint64(unsafe.Sizeof(TraceCaptures{}))) / 4

func NewTraceCaptures(epoch ProcessObservationEpoch, codec *CursorCodec) (*TraceCaptures, error) {
	if !epoch.valid() || !codec.valid() || codec.epoch != epoch {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	return &TraceCaptures{epoch: epoch, codec: codec}, nil
}
func (s *TraceCaptures) valid() bool {
	return s != nil && s.epoch.valid() && s.codec.valid() && s.codec.epoch == s.epoch
}
func (s *TraceCaptures) removeLocked(state *captureState) {
	if state == nil || state.closed {
		return
	}
	for i, present := range s.slots {
		if present == state {
			s.slots[i] = nil
			break
		}
	}
	state.closed = true
	state.rows = nil
	state.cuts = nil
}
func (s *TraceCaptures) pruneLocked(now time.Duration) {
	for _, state := range s.slots {
		if state != nil && state.deadline <= now {
			s.removeLocked(state)
		}
	}
}

type CaptureReservation struct{ state *captureState }

func (r CaptureReservation) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: capture reservation"))
}
func (s *TraceCaptures) Reserve(binding ReadBinding, controller ControllerHandle, run uint64) (CaptureReservation, error) {
	if !s.valid() || !binding.valid() || !controller.in(s.epoch) || run == 0 {
		return CaptureReservation{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	now, err := s.codec.tick()
	if err != nil {
		return CaptureReservation{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil || id == [16]byte{} {
		return CaptureReservation{}, failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAVAILABLE, obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	for i, state := range s.slots {
		if state == nil {
			// Slot and full byte budget are reserved before any image allocation.
			state = &captureState{parent: s, id: id, binding: binding, controller: controller, run: run, deadline: now + cursorTTL}
			s.slots[i] = state
			return CaptureReservation{state: state}, nil
		}
	}
	return CaptureReservation{}, capacity()
}
func (r CaptureReservation) Release() {
	state := r.state
	if state == nil || state.parent == nil {
		return
	}
	s := state.parent
	s.mu.Lock()
	defer s.mu.Unlock()
	// All copies share this one state. A successful Commit transfers ownership
	// to the bounded store; deferred releases of reservations become no-ops.
	if !state.committed {
		s.removeLocked(state)
	}
}

type CaptureHandle struct {
	id         [16]byte
	controller ControllerHandle
	run        uint64
}

func (h CaptureHandle) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: capture handle"))
}
func (r CaptureReservation) Commit(rows []TraceRow, cuts TraceCutMetadata) (CaptureHandle, error) {
	return r.commit(rows, cuts, nil)
}
func (r CaptureReservation) CommitTruncated(rows []TraceRow, cuts TraceCutMetadata, truncation obs.TraceTruncationReason) (CaptureHandle, error) {
	switch truncation {
	case obs.TraceTruncationReason_TRACE_TRUNCATION_REASON_ROW_LIMIT, obs.TraceTruncationReason_TRACE_TRUNCATION_REASON_BYTE_LIMIT:
	default:
		return CaptureHandle{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	return r.commit(rows, cuts, &truncation)
}
func (r CaptureReservation) commit(rows []TraceRow, cuts TraceCutMetadata, truncation *obs.TraceTruncationReason) (CaptureHandle, error) {
	state := r.state
	if state == nil || state.parent == nil || len(rows) > 512 || cuts.wire == nil {
		return CaptureHandle{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	charge := uint64(unsafe.Sizeof(captureState{})) + 2048 + uint64(len(rows))*uint64(unsafe.Sizeof((*obs.TraceRow)(nil)))
	for _, row := range rows {
		if row.wire == nil || row.wire.RunId != state.run || row.wire.GetOrdinal().GetExhausted() != nil && cuts.wire.Order == obs.TraceOrderState_TRACE_ORDER_STATE_HEALTHY {
			return CaptureHandle{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
		}
		charge += 1024 + 4*uint64(proto.Size(row.wire))
	}
	if charge > captureReservationBytes {
		return CaptureHandle{}, capacity()
	}
	s := state.parent
	now, err := s.codec.tick()
	if err != nil {
		return CaptureHandle{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if state.closed || state.committed {
		return CaptureHandle{}, stale()
	}
	image := make([]*obs.TraceRow, len(rows))
	for i, row := range rows {
		image[i] = proto.Clone(row.wire).(*obs.TraceRow)
		image[i].LocalIndex = uint32(i)
	}
	state.rows = image
	state.cuts = proto.Clone(cuts.wire).(*obs.TraceCutMetadata)
	state.truncated = truncation != nil
	if truncation != nil {
		state.truncation = *truncation
	}
	state.committed = true
	return CaptureHandle{id: state.id, controller: state.controller, run: state.run}, nil
}

type CaptureUsage struct {
	charged uint64
	count   uint64
}

func (u CaptureUsage) ChargedBytes() uint64 { return u.charged }
func (u CaptureUsage) Captures() uint64     { return u.count }

// Usage also prunes expired images, including images referenced by retained
// copies of a reservation. There is no background collector or timer task.
func (s *TraceCaptures) Usage() CaptureUsage {
	if !s.valid() {
		return CaptureUsage{}
	}
	now, err := s.codec.tick()
	if err != nil {
		return CaptureUsage{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	usage := CaptureUsage{charged: uint64(unsafe.Sizeof(TraceCaptures{}))}
	for _, state := range s.slots {
		if state != nil {
			usage.count++
			usage.charged += captureReservationBytes
		}
	}
	return usage
}

type TraceCursor struct {
	epoch    ProcessObservationEpoch
	binding  ReadBinding
	handle   CaptureHandle
	next     uint32
	deadline time.Duration
}

func (c *CursorCodec) EncodeTrace(cursor TraceCursor) (string, error) {
	if !c.valid() || cursor.epoch != c.epoch || !cursor.handle.controller.in(c.epoch) || cursor.handle.run == 0 || cursor.handle.id == [16]byte{} || cursor.next == 0 || cursor.next >= 512 {
		return "", stale()
	}
	payload, err := c.base(cursor.binding, cursor.deadline)
	if err != nil {
		return "", err
	}
	payload = append(payload, cursor.handle.controller.value[:]...)
	payload = appendNumber(payload, cursor.handle.run)
	payload = append(payload, cursor.handle.id[:]...)
	payload = appendNumber(payload, uint64(cursor.next))
	return c.seal(observabilityv1connect.ObservabilityServiceReadRunTraceProcedure, payload)
}
func (c *CursorCodec) DecodeTrace(token string, binding ReadBinding, controller ControllerHandle, run uint64) (TraceCursor, error) {
	if !c.valid() || !controller.in(c.epoch) || run == 0 {
		return TraceCursor{}, stale()
	}
	payload, err := c.open(observabilityv1connect.ObservabilityServiceReadRunTraceProcedure, token)
	if err != nil || len(payload) != 128 {
		return TraceCursor{}, stale()
	}
	deadline, err := c.checkBase(payload, binding)
	if err != nil || !bytes.Equal(payload[64:96], controller.value[:]) || binary.BigEndian.Uint64(payload[96:104]) != run {
		return TraceCursor{}, stale()
	}
	var id [16]byte
	copy(id[:], payload[104:120])
	next := binary.BigEndian.Uint64(payload[120:128])
	if id == [16]byte{} || next == 0 || next >= 512 {
		return TraceCursor{}, stale()
	}
	return TraceCursor{epoch: c.epoch, binding: binding, handle: CaptureHandle{id: id, controller: controller, run: run}, next: uint32(next), deadline: deadline}, nil
}

type TracePageRequest struct {
	size   PageSize
	handle CaptureHandle
	next   uint32
	cursor *TraceCursor
}

func NewTracePageRequest(size PageSize, handle CaptureHandle) (TracePageRequest, error) {
	if _, err := size.Value(); err != nil || handle.id == [16]byte{} || !handle.controller.valid() || handle.run == 0 {
		return TracePageRequest{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	return TracePageRequest{size: size, handle: handle}, nil
}
func TracePageRequestFromCursor(size PageSize, cursor TraceCursor) (TracePageRequest, error) {
	request, err := NewTracePageRequest(size, cursor.handle)
	if err != nil || !cursor.epoch.valid() || cursor.next == 0 || cursor.next >= 512 {
		return TracePageRequest{}, stale()
	}
	request.next = cursor.next
	request.cursor = &cursor
	return request, nil
}

type TracePageResult struct {
	wire *obs.ReadRunTraceResponse
	next TraceCursor
	more bool
}

func (r TracePageResult) Next() (TraceCursor, bool) { return r.next, r.more }
func (r TracePageResult) Wire(nextToken string) (*obs.ReadRunTraceResponse, error) {
	if r.wire == nil || r.more && (nextToken == "" || len(nextToken) > 512) || !r.more && nextToken != "" {
		return nil, stale()
	}
	wire := proto.Clone(r.wire).(*obs.ReadRunTraceResponse)
	page := wire.GetPage()
	if page == nil && wire.GetTruncated() != nil {
		page = wire.GetTruncated().Page
	}
	if r.more {
		page.Continuation = &obs.PageContinuation{Result: &obs.PageContinuation_NextPageToken{NextPageToken: nextToken}}
	}
	if contracts.AdmitObservabilityResponse(wire) != nil {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CAPTURE)
	}
	return wire, nil
}
func (s *TraceCaptures) Read(request TracePageRequest, binding ReadBinding) (TracePageResult, error) {
	limit, err := request.size.Value()
	if err != nil || !s.valid() || !binding.valid() || !request.handle.controller.in(s.epoch) || request.handle.run == 0 || request.next >= 512 {
		return TracePageResult{}, stale()
	}
	now, err := s.codec.tick()
	if err != nil {
		return TracePageResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	var state *captureState
	for _, candidate := range s.slots {
		if candidate != nil && candidate.id == request.handle.id {
			state = candidate
			break
		}
	}
	if state == nil || state.closed || !state.committed || state.binding != binding || state.controller != request.handle.controller || state.run != request.handle.run {
		return TracePageResult{}, stale()
	}
	if request.cursor != nil && (request.cursor.epoch != s.epoch || request.cursor.binding != binding || request.cursor.deadline != state.deadline) {
		return TracePageResult{}, stale()
	}
	if int(request.next) > len(state.rows) {
		return TracePageResult{}, stale()
	}
	end := min(len(state.rows), int(request.next)+int(limit))
	rows := make([]*obs.TraceRow, end-int(request.next))
	for i := range rows {
		rows[i] = proto.Clone(state.rows[int(request.next)+i]).(*obs.TraceRow)
	}
	controller, _ := state.controller.Wire()
	page := &obs.TracePage{Controller: controller, RunId: state.run, Cuts: proto.Clone(state.cuts).(*obs.TraceCutMetadata), Rows: rows, Continuation: &obs.PageContinuation{Result: &obs.PageContinuation_End{End: &emptypb.Empty{}}}}
	result := TracePageResult{wire: &obs.ReadRunTraceResponse{Result: &obs.ReadRunTraceResponse_Page{Page: page}}}
	if state.truncated {
		result.wire.Result = &obs.ReadRunTraceResponse_Truncated{Truncated: &obs.TruncatedTracePage{Reason: state.truncation, Page: page}}
	}
	if end < len(state.rows) {
		result.more = true
		result.next = TraceCursor{epoch: s.epoch, binding: binding, handle: request.handle, next: uint32(end), deadline: state.deadline}
	}
	return result, nil
}
