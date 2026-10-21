// Package historyquery defines admitted, bounded read requests. It owns no
// recorder, policy, catalogue or SQL authority.
package historyquery

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"github.com/damione1/Homeward/internal/domain"
	"time"
)

const (
	MaxPoints        = 100
	MaxTracePoints   = 31
	MaxResponseBytes = 256 << 10
	MaxTokenBytes    = 512
	QueryTimeout     = 2 * time.Second
	CursorTTL        = 60 * time.Second
	// Coverage and envelope reservations precede selection of payloads.
	CoverageBytes = 96 << 10
	EnvelopeBytes = 4 << 10
	MaxPointBytes = MaxResponseBytes - CoverageBytes - EnvelopeBytes
	// One request owner covers decoding/capture/SQL/conversion/encoding.
	// The source charge ledger budgets16MiB for staged SQL inputs, receive and
	// digest copies plus bounded state graphs;8MiB for catalogue clones/encoding;
	// 4MiB for JSON escape checking/growth;1MiB coverage copies and1MiB control/
	// transport envelopes. The remaining2MiB is slack within32MiB. These are
	// ownership allowances; concrete composed allocation oracles qualify the
	// fixture and toolchain only. They do not constitute an RSS bound.
	ReaderReservationBytes = 32 << 20
	MaxReaderReservations  = 2
)

var (
	ErrInvalid       = errors.New("history: invalid request")
	ErrStale         = errors.New("history: stale cursor")
	ErrPointTooLarge = errors.New("history: point too large")
	ErrUnavailable   = errors.New("history: unavailable")
	ErrIntegrity     = errors.New("history: invalid stored data")
	ErrBudget        = errors.New("history: budget exceeded")
)

type Kind uint8

const (
	Invalid Kind = iota
	Observations
	Aggregates
	ExecutionTraces
)

type Query struct {
	kind       Kind
	entity     domain.EntityID
	start, end time.Time
	limit      int
	epoch      string
}

func NewQuery(kind Kind, entity string, start, end time.Time, limit int, epoch string) (Query, error) {
	if start.Year() < 1 || start.Year() > 9999 || end.Year() < 1 || end.Year() > 9999 || start.IsZero() || end.IsZero() || !start.Before(end) || start.Location() != time.UTC || end.Location() != time.UTC {
		return Query{}, ErrInvalid
	}
	maximum, span := MaxPoints, 7*24*time.Hour
	switch kind {
	case Observations:
		if epoch != "" {
			return Query{}, ErrInvalid
		}
	case Aggregates:
		span = 500 * time.Hour
		if epoch != "" || !start.Equal(start.Truncate(time.Hour)) || !end.Equal(end.Truncate(time.Hour)) {
			return Query{}, ErrInvalid
		}
	case ExecutionTraces:
		maximum = MaxTracePoints
		if entity != "" || (epoch != "" && !validEpoch(epoch)) {
			return Query{}, ErrInvalid
		}
	default:
		return Query{}, ErrInvalid
	}
	if end.Sub(start) > span || limit < 0 || limit > maximum {
		return Query{}, ErrInvalid
	}
	if limit == 0 {
		limit = maximum
	}
	q := Query{kind: kind, start: start, end: end, limit: limit, epoch: epoch}
	if kind != ExecutionTraces {
		admitted, err := domain.ParseEntityID(entity)
		if err != nil {
			return Query{}, ErrInvalid
		}
		q.entity = admitted
	}
	return q, nil
}
func validEpoch(value string) bool {
	if len(value) != 32 {
		return false
	}
	nonzero := false
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
		nonzero = nonzero || c != '0'
	}
	return nonzero
}
func (q Query) Kind() Kind                     { return q.kind }
func (q Query) Entity() domain.EntityID        { return q.entity }
func (q Query) Bounds() (time.Time, time.Time) { return q.start, q.end }
func (q Query) Limit() int                     { return q.limit }
func (q Query) Epoch() string                  { return q.epoch }
func (q Query) Valid() bool {
	entity := ""
	if q.kind != ExecutionTraces {
		var err error
		entity, err = q.entity.Value()
		if err != nil {
			return false
		}
	}
	admitted, err := NewQuery(q.kind, entity, q.start, q.end, q.limit, q.epoch)
	return err == nil && admitted == q
}
func (q Query) Digest() ([32]byte, error) {
	if !q.Valid() {
		return [32]byte{}, ErrInvalid
	}
	h := sha256.New()
	h.Write([]byte("homeward-history-query-v1"))
	h.Write([]byte{byte(q.kind)})
	var n [8]byte
	for _, v := range []uint64{uint64(q.start.Unix()), uint64(q.start.Nanosecond()), uint64(q.end.Unix()), uint64(q.end.Nanosecond()), uint64(q.limit)} {
		binary.BigEndian.PutUint64(n[:], v)
		h.Write(n[:])
	}
	entity := ""
	if q.kind != ExecutionTraces {
		entity, _ = q.entity.Value()
	}
	for _, v := range []string{entity, q.epoch} {
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write([]byte(v))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}
