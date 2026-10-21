package observability

import (
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Author struct{ wire *obs.RuntimeAuthor }

func NewProcessAuthor() Author {
	return Author{wire: &obs.RuntimeAuthor{Result: &obs.RuntimeAuthor_Process{Process: &emptypb.Empty{}}}}
}
func NewInternalAutomationAuthor() Author {
	return Author{wire: &obs.RuntimeAuthor{Result: &obs.RuntimeAuthor_InternalAutomation{InternalAutomation: &emptypb.Empty{}}}}
}
func NewUnauthenticatedAuthor() Author {
	return Author{wire: &obs.RuntimeAuthor{Result: &obs.RuntimeAuthor_Unauthenticated{Unauthenticated: &emptypb.Empty{}}}}
}
func NewHumanAuthor(account contracts.AccountID) (Author, error) {
	value, err := account.Value()
	if err != nil || len(value) > 128 || !utf8.ValidString(value) {
		return Author{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_AUTHOR)
	}
	wire := &obs.RuntimeAuthor{Result: &obs.RuntimeAuthor_HumanAccount{HumanAccount: &auth.AccountId{Value: strings.Clone(value)}}}
	if contracts.AdmitObservabilityValue(wire) != nil {
		return Author{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_AUTHOR)
	}
	return Author{wire: wire}, nil
}

type Event struct {
	code      obs.LogCode
	level     obs.LogLevel
	component obs.LogComponent
	at        time.Time
	author    Author
	admitted  bool
}

func NewEvent(code obs.LogCode, level obs.LogLevel, component obs.LogComponent, at time.Time, author Author) (Event, error) {
	if at.IsZero() || author.wire == nil {
		return Event{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_AUTHOR)
	}
	event := Event{code: code, level: level, component: component, at: at.UTC(), author: author}
	if contracts.AdmitObservabilityValue(event.Wire()) != nil {
		return Event{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CODE)
	}
	event.author.wire = proto.Clone(author.wire).(*obs.RuntimeAuthor)
	event.admitted = true
	return event, nil
}
func (e Event) Wire() *obs.RuntimeEvent {
	var author *obs.RuntimeAuthor
	if e.author.wire != nil {
		author = proto.Clone(e.author.wire).(*obs.RuntimeAuthor)
	}
	return &obs.RuntimeEvent{Code: e.code, Level: e.level, Component: e.component, OccurredAt: timestamppb.New(e.at), Author: author}
}
func (e Event) charge() uint64 {
	// Retained author protobufs are independently owned, including their nested
	// message and account bytes. Conservative fixed overhead covers heap objects.
	return uint64(unsafe.Sizeof(Event{})) + 256 + uint64(len(e.author.wire.GetHumanAccount().GetValue()))
}

type retainedRuntimeRow struct {
	sequence uint64
	event    Event
	charge   uint64
}
type Journal struct {
	mu               sync.Mutex
	epoch            ProcessObservationEpoch
	cells            *Cells
	rows             [1024]retainedRuntimeRow
	start, count     int
	through, charged uint64
}

func NewJournal(epoch ProcessObservationEpoch, cells *Cells) (*Journal, error) {
	if !epoch.valid() || !cells.valid() || epoch != cells.epoch || uint64(unsafe.Sizeof(Journal{})) > 1<<20 {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	return &Journal{epoch: epoch, cells: cells, charged: uint64(unsafe.Sizeof(Journal{}))}, nil
}
func (j *Journal) valid() bool { return j != nil && j.epoch.valid() && j.cells.valid() }
func (j *Journal) Emit(event Event) error {
	if !j.valid() || !event.admitted || event.author.wire == nil {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_CODE)
	}
	charge := event.charge()
	if charge > 1024 {
		return invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_AUTHOR)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.through == math.MaxUint64 {
		return failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_CAPACITY, obs.ObservabilityField_OBSERVABILITY_FIELD_COUNTER)
	}
	for j.count == len(j.rows) || charge > uint64(1<<20)-j.charged {
		if j.count == 0 {
			return failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_CAPACITY, obs.ObservabilityField_OBSERVABILITY_FIELD_AUTHOR)
		}
		old := &j.rows[j.start]
		j.charged -= old.charge
		*old = retainedRuntimeRow{}
		j.start = (j.start + 1) % len(j.rows)
		j.count--
		j.cells.mu.Lock()
		j.cells.journalEvicted.add(1)
		j.cells.mu.Unlock()
	}
	// Admission and all variable-size checks precede this bounded retained copy.
	event.author.wire = proto.Clone(event.author.wire).(*obs.RuntimeAuthor)
	j.through++
	j.rows[(j.start+j.count)%len(j.rows)] = retainedRuntimeRow{sequence: j.through, event: event, charge: charge}
	j.count++
	j.charged += charge
	return nil
}

type JournalSnapshot struct {
	epoch                        ProcessObservationEpoch
	rows, charged, from, through uint64
	evicted                      counter
}

func (j *Journal) Snapshot() (JournalSnapshot, error) {
	if !j.valid() {
		return JournalSnapshot{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_METRICS)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cells.mu.Lock()
	defer j.cells.mu.Unlock()
	from := uint64(1)
	if j.count > 0 {
		from = j.rows[j.start].sequence
	} else if j.through < math.MaxUint64 {
		from = j.through + 1
	}
	return JournalSnapshot{epoch: j.epoch, rows: uint64(j.count), charged: j.charged, from: from, through: j.through, evicted: j.cells.journalEvicted}, nil
}
func (s JournalSnapshot) RetainedRows() uint64 { return s.rows }
func (s JournalSnapshot) ChargedBytes() uint64 { return s.charged }
func (s JournalSnapshot) Through() uint64      { return s.through }
func (s JournalSnapshot) RetainedFrom() uint64 { return s.from }
func (s JournalSnapshot) EvictedTotal() *obs.CounterMeasurement {
	if !s.epoch.valid() {
		return unavailableCounter()
	}
	return s.evicted.wire()
}
func (s JournalSnapshot) Metrics() (*obs.JournalMetrics, error) {
	if !s.epoch.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_METRICS)
	}
	return &obs.JournalMetrics{RetainedRows: gauge(s.rows), ChargedBytes: gauge(s.charged), EvictedTotal: s.evicted.wire()}, nil
}

type LogPageRequest struct {
	size   PageSize
	cursor LogCursor
}

func NewLogPageRequest(size PageSize, cursor LogCursor) (LogPageRequest, error) {
	if _, err := size.Value(); err != nil || !cursor.epoch.valid() || !cursor.binding.valid() || !cursor.first && (cursor.next == 0 || cursor.next > cursor.upper) {
		return LogPageRequest{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_TOKEN)
	}
	return LogPageRequest{size, cursor}, nil
}

type LogPageResult struct {
	wire *obs.QueryRuntimeLogsResponse
	next LogCursor
	more bool
}

func (r LogPageResult) Rows() []*obs.RuntimeLogRow {
	if r.wire == nil || r.wire.GetPage() == nil {
		return nil
	}
	return proto.Clone(r.wire.GetPage()).(*obs.RuntimeLogPage).Rows
}
func (r LogPageResult) Next() (LogCursor, bool) { return r.next, r.more }
func (r LogPageResult) CursorEvicted() *obs.CursorEvicted {
	if r.wire == nil || r.wire.GetCursorEvicted() == nil {
		return nil
	}
	return proto.Clone(r.wire.GetCursorEvicted()).(*obs.CursorEvicted)
}
func (r LogPageResult) Wire(nextToken string) (*obs.QueryRuntimeLogsResponse, error) {
	if r.wire == nil || r.more && (nextToken == "" || len(nextToken) > 512) || !r.more && nextToken != "" {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_TOKEN)
	}
	wire := proto.Clone(r.wire).(*obs.QueryRuntimeLogsResponse)
	if r.more {
		wire.GetPage().Continuation = &obs.PageContinuation{Result: &obs.PageContinuation_NextPageToken{NextPageToken: nextToken}}
	}
	if contracts.AdmitObservabilityResponse(wire) != nil {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_TOKEN)
	}
	return wire, nil
}
func (j *Journal) Read(request LogPageRequest) (LogPageResult, error) {
	limit, err := request.size.Value()
	if err != nil || !j.valid() || request.cursor.epoch != j.epoch || !request.cursor.binding.valid() {
		return LogPageResult{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_TOKEN)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if request.cursor.upper > j.through {
		return LogPageResult{}, stale()
	}
	from := uint64(1)
	if j.count > 0 {
		from = j.rows[j.start].sequence
	}
	next := request.cursor.next
	if request.cursor.first {
		next = from
	}
	if !request.cursor.first && next < from {
		j.cells.mu.Lock()
		evicted := j.cells.journalEvicted.wire()
		j.cells.mu.Unlock()
		return LogPageResult{wire: &obs.QueryRuntimeLogsResponse{Result: &obs.QueryRuntimeLogsResponse_CursorEvicted{CursorEvicted: &obs.CursorEvicted{RetainedFrom: from, EvictedTotal: evicted}}}}, nil
	}
	rows := make([]*obs.RuntimeLogRow, 0, int(limit))
	var last uint64
	for i := 0; i < j.count && len(rows) < int(limit); i++ {
		row := j.rows[(j.start+i)%len(j.rows)]
		if row.sequence >= next && row.sequence <= request.cursor.upper {
			rows = append(rows, &obs.RuntimeLogRow{Sequence: row.sequence, Event: row.event.Wire()})
			last = row.sequence
		}
	}
	epoch, _ := j.epoch.Wire()
	result := LogPageResult{wire: &obs.QueryRuntimeLogsResponse{Result: &obs.QueryRuntimeLogsResponse_Page{Page: &obs.RuntimeLogPage{ProcessEpoch: epoch, UpperSequence: request.cursor.upper, Rows: rows, Continuation: &obs.PageContinuation{Result: &obs.PageContinuation_End{End: &emptypb.Empty{}}}}}}}
	if last > 0 && last < request.cursor.upper {
		result.next = request.cursor
		result.next.first = false
		result.next.next = last + 1
		result.more = true
	}
	return result, nil
}

// Cells exposes this journal's already admitted process-wide cells to trusted
// composition; it creates no new epoch or label authority.
func (j *Journal) Cells() (*Cells, error) {
	if !j.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	return j.cells, nil
}
