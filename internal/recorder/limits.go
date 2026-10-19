package recorder

import (
	"errors"
	"unsafe"

	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrInvalidLimits = errors.New("recorder: invalid memory limits")

const (
	DefaultBudgetBytes = 16 << 20
	MaxBudgetBytes     = 32 << 20
	MinBudgetBytes     = 16 << 20
	MaxQueuedEntries   = sequencer.MaxRecorderQueueEntries
	FlushStates        = 500
	FlushEncodedBytes  = 1 << 20
)

// Limits is admitted as one budget. The fixed mailbox allocation, retained
// queue payload, one batch or snapshot, and encoding/driver reserve all fit
// within totalBytes. The charge is conservative, not an exact heap measure.
type Limits struct {
	totalBytes        int
	queueBytes        int
	queueCount        int
	imageOrBatchBytes int
	encodingBytes     int
}

func DefaultLimits() Limits {
	l, _ := NewLimits(DefaultBudgetBytes)
	return l
}

func NewLimits(totalBytes int) (Limits, error) {
	if totalBytes < MinBudgetBytes || totalBytes > MaxBudgetBytes {
		return Limits{}, ErrInvalidLimits
	}
	const batchBytes = 4 << 20
	// Subscription.queue is preallocated at its count limit. A retained Entry
	// charge includes a fixed body too, intentionally counting both owners.
	queueFixed := int(unsafe.Sizeof(sequencer.Entry{}))*MaxQueuedEntries + 64*1024
	queuePayload := batchBytes - queueFixed
	if queuePayload < FlushEncodedBytes || totalBytes-batchBytes*2 < 8<<20 {
		return Limits{}, ErrInvalidLimits
	}
	return Limits{
		totalBytes: totalBytes, queueBytes: queuePayload,
		queueCount: MaxQueuedEntries, imageOrBatchBytes: batchBytes,
		encodingBytes: totalBytes - 2*batchBytes,
	}, nil
}

func (l Limits) Valid() bool {
	if l.totalBytes < MinBudgetBytes || l.totalBytes > MaxBudgetBytes {
		return false
	}
	admitted, err := NewLimits(l.totalBytes)
	return err == nil && admitted == l
}

func (l Limits) TotalBytes() int        { return l.totalBytes }
func (l Limits) QueueBytes() int        { return l.queueBytes }
func (l Limits) QueueCount() int        { return l.queueCount }
func (l Limits) ImageOrBatchBytes() int { return l.imageOrBatchBytes }
func (l Limits) EncodingBytes() int     { return l.encodingBytes }
