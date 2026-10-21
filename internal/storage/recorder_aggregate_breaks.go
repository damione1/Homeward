package storage

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/recordermaintenance"
)

// RecorderAnalyticalBreak is an admitted uncertainty envelope. It keeps an
// explicit cause even where a refused long edge produced no numeric bucket.
// Its envelope is not a count of lost reports or an interpolated UTC coverage.
type RecorderAnalyticalBreak struct {
	start, end time.Time
	reasons    recordermaintenance.Reason
}

func (b RecorderAnalyticalBreak) Bounds() (time.Time, time.Time)      { return b.start, b.end }
func (b RecorderAnalyticalBreak) Reasons() recordermaintenance.Reason { return b.reasons }
func (s *Store) ReadRecorderAnalyticalBreaks(ctx context.Context, entity string, start, end time.Time) ([]RecorderAnalyticalBreak, error) {
	if s == nil || s.pool == nil || !validHistoryContext(ctx) || entity == "" || !start.Before(end) || start.Location() != time.UTC || end.Location() != time.UTC {
		return nil, recordermaintenance.ErrInvalid
	}
	bounded, cancel := context.WithTimeout(ctx, recordermaintenance.AttemptTimeout)
	defer cancel()
	// Intervals are half-open; a zero-length refusal is an explicit point
	// included at the query start and excluded at its end.
	rows, err := s.pool.Query(bounded, `SELECT start_at,end_at,reasons FROM public.recorder_analytical_breaks WHERE entity_digest=$1 AND start_at<$3 AND (end_at>$2 OR (start_at=end_at AND start_at>=$2)) ORDER BY start_at,end_at LIMIT 501`, hashEntity(entity), start, end)
	if err != nil {
		return nil, recordermaintenance.ErrUnavailable
	}
	defer rows.Close()
	out := make([]RecorderAnalyticalBreak, 0)
	for rows.Next() {
		if len(out) == recordermaintenance.MaxPrefixRows {
			return nil, recordermaintenance.ErrBudget
		}
		var b RecorderAnalyticalBreak
		var reasons int32
		if err = rows.Scan(&b.start, &b.end, &reasons); err != nil {
			return nil, recordermaintenance.ErrUnavailable
		}
		if reasons <= 0 || reasons > 4095 || b.start.After(b.end) {
			return nil, recordermaintenance.ErrIntegrity
		}
		b.reasons = recordermaintenance.Reason(reasons)
		if !b.reasons.Valid() {
			return nil, recordermaintenance.ErrIntegrity
		}
		b.start = b.start.UTC()
		b.end = b.end.UTC()
		out = append(out, b)
	}
	if rows.Err() != nil {
		return nil, recordermaintenance.ErrUnavailable
	}
	return out, nil
}
