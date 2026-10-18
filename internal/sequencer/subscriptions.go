package sequencer

import (
	"context"
	"errors"
)

var (
	ErrSubscriptionClosed = errors.New("subscription is closed")
	ErrSubscriptionFull   = errors.New("subscription limit reached")
	ErrCursorInvalidated  = errors.New("subscription cursor requires resynchronization")
	ErrEdgeOverflow       = errors.New("edge subscription overflowed")
	ErrRecorderGap        = errors.New("recorder history has a gap")
)

const MaxSubscriptions = 64
const MaxQueueCapacity = 4096

// QueueLimits is constructed as a whole so each class has an explicit,
// independent capacity. The defaults are provisional; final quotas are a
// separate operational decision.
type QueueLimits struct{ ui, edge, commands, recorder int }

func NewQueueLimits(ui, edge, commands, recorder int) (QueueLimits, error) {
	l := QueueLimits{ui: ui, edge: edge, commands: commands, recorder: recorder}
	if !l.valid() {
		return QueueLimits{}, ErrInvalidInput
	}
	return l, nil
}

func DefaultQueueLimits() QueueLimits {
	l, _ := NewQueueLimits(256, 128, 64, 1024)
	return l
}

func (l QueueLimits) valid() bool {
	return l.ui > 0 && l.ui <= MaxQueueCapacity && l.edge > 0 && l.edge <= MaxQueueCapacity && l.commands > 0 && l.commands <= MaxQueueCapacity && l.recorder > 0 && l.recorder <= MaxQueueCapacity
}

func (l QueueLimits) Capacities() (ui, edge, commands, recorder int, err error) {
	if !l.valid() {
		return 0, 0, 0, 0, ErrInvalidInput
	}
	return l.ui, l.edge, l.commands, l.recorder, nil
}

func (s *Sequencer) QueueLimits() (QueueLimits, error) {
	if s == nil || s.journal == nil {
		return QueueLimits{}, ErrInvalidInput
	}
	return s.limits, nil
}

type SubscriptionClass uint8

const (
	SubscriptionUnspecified SubscriptionClass = iota
	SubscriptionUI
	SubscriptionEdge
	SubscriptionRecorder
)

type LossReason uint8

const (
	LossUnspecified LossReason = iota
	LossUIResync
	LossEdgeOverflow
	LossRecorderOverflow
)

// Loss spans the admission positions affected by a bounded queue. Count is
// the number of undelivered entries, not necessarily To.seq-From.seq+1:
// other classes of entry may lie between two lost edge events.
type Loss struct {
	From   Cursor
	To     Cursor
	Count  uint64
	Reason LossReason
}

type DeliveryKind uint8

const (
	DeliveryUnspecified DeliveryKind = iota
	DeliveryEntry
	DeliveryLoss
)

// Delivery explicitly marks whether a stream cursor still describes a
// faithful sequence. After any coalescing or loss, ReplayValid remains false
// for this subscription; a consumer must obtain a new snapshot/subscription.
type Delivery struct {
	kind        DeliveryKind
	entry       Entry
	loss        Loss
	replayValid bool
}

func (d Delivery) Kind() DeliveryKind   { return d.kind }
func (d Delivery) Entry() (Entry, bool) { return d.entry, d.kind == DeliveryEntry }
func (d Delivery) Loss() (Loss, bool)   { return d.loss, d.kind == DeliveryLoss }
func (d Delivery) ReplayValid() bool    { return d.replayValid }

type SubscriptionStatus struct {
	Class       SubscriptionClass
	Capacity    int
	Queued      int
	Lost        uint64
	Coalesced   uint64
	ReplayValid bool
	Degraded    bool
	Closed      bool
	PendingLoss *Loss
}

// Subscription is a bounded mailbox. It starts no goroutine. The sequencer
// mutex protects its queue and lifecycle, while Next waits outside that lock.
type Subscription struct {
	owner       *Sequencer
	class       SubscriptionClass
	capacity    int
	queue       []Entry
	wake        chan struct{}
	closed      bool
	degraded    bool
	replayValid bool
	loss        *Loss
	lost        uint64
	coalesced   uint64
}

func (s *Sequencer) SubscribeUI() (*Subscription, Cursor, error) {
	return s.subscribe(SubscriptionUI)
}
func (s *Sequencer) SubscribeEdge() (*Subscription, Cursor, error) {
	return s.subscribe(SubscriptionEdge)
}
func (s *Sequencer) SubscribeRecorder() (*Subscription, Cursor, error) {
	return s.subscribe(SubscriptionRecorder)
}

func (s *Sequencer) subscribe(class SubscriptionClass) (*Subscription, Cursor, error) {
	if s == nil || s.journal == nil {
		return nil, Cursor{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subs) >= MaxSubscriptions {
		return nil, Cursor{}, ErrSubscriptionFull
	}
	var capacity int
	switch class {
	case SubscriptionUI:
		capacity = s.limits.ui
	case SubscriptionEdge:
		capacity = s.limits.edge
	case SubscriptionRecorder:
		capacity = s.limits.recorder
	case SubscriptionUnspecified:
		return nil, Cursor{}, ErrInvalidInput
	default:
		return nil, Cursor{}, ErrInvalidInput
	}
	sub := &Subscription{owner: s, class: class, capacity: capacity, queue: make([]Entry, 0, capacity), wake: make(chan struct{}), replayValid: true}
	s.subs[sub] = struct{}{}
	return sub, Cursor{epoch: s.epoch, seq: s.seq}, nil
}

func (sub *Subscription) signal() {
	close(sub.wake)
	sub.wake = make(chan struct{})
}

func (sub *Subscription) addLoss(entry Entry, reason LossReason) {
	if sub.loss == nil {
		sub.loss = &Loss{From: entry.cursor, To: entry.cursor, Count: 1, Reason: reason}
	} else {
		if entry.cursor.seq < sub.loss.From.seq {
			sub.loss.From = entry.cursor
		}
		if entry.cursor.seq > sub.loss.To.seq {
			sub.loss.To = entry.cursor
		}
		sub.loss.Count++
	}
	sub.lost++
	sub.replayValid = false
}

func (s *Sequencer) publish(entry Entry) {
	for sub := range s.subs {
		switch sub.class {
		case SubscriptionUI:
			if entry.kind == KindState {
				for i, old := range sub.queue {
					if old.kind == KindState && old.entity == entry.entity {
						sub.addLoss(old, LossUIResync)
						sub.coalesced++
						copy(sub.queue[i:], sub.queue[i+1:])
						sub.queue[len(sub.queue)-1] = Entry{}
						sub.queue = sub.queue[:len(sub.queue)-1]
						break
					}
				}
			}
			if len(sub.queue) == sub.capacity {
				sub.addLoss(sub.queue[0], LossUIResync)
				copy(sub.queue, sub.queue[1:])
				sub.queue[len(sub.queue)-1] = Entry{}
				sub.queue = sub.queue[:len(sub.queue)-1]
			}
			sub.queue = append(sub.queue, entry)
			sub.signal()
		case SubscriptionEdge:
			if entry.kind != KindEvent {
				continue
			}
			if sub.degraded {
				sub.addLoss(entry, LossEdgeOverflow)
			} else if len(sub.queue) == sub.capacity {
				for _, queued := range sub.queue {
					sub.addLoss(queued, LossEdgeOverflow)
				}
				sub.addLoss(entry, LossEdgeOverflow)
				clear(sub.queue)
				sub.queue = sub.queue[:0]
				sub.degraded = true
			} else {
				sub.queue = append(sub.queue, entry)
			}
			sub.signal()
		case SubscriptionRecorder:
			if len(sub.queue) == sub.capacity {
				sub.addLoss(sub.queue[0], LossRecorderOverflow)
				copy(sub.queue, sub.queue[1:])
				sub.queue[len(sub.queue)-1] = Entry{}
				sub.queue = sub.queue[:len(sub.queue)-1]
			}
			sub.queue = append(sub.queue, entry)
			sub.signal()
		case SubscriptionUnspecified:
			panic("invalid subscription class")
		default:
			panic("invalid subscription class")
		}
	}
}

// Next yields a loss before any later entry. Edge overflow is terminal for
// this subscription and repeats the explicit error until Close. Other gaps
// are reported once, while ReplayValid stays false permanently.
func (sub *Subscription) Next(ctx context.Context) (Delivery, error) {
	if sub == nil || sub.owner == nil || ctx == nil {
		return Delivery{}, ErrInvalidInput
	}
	for {
		sub.owner.mu.Lock()
		if sub.closed {
			sub.owner.mu.Unlock()
			return Delivery{}, ErrSubscriptionClosed
		}
		if sub.loss != nil {
			loss := *sub.loss
			if !sub.degraded {
				sub.loss = nil
			}
			sub.owner.mu.Unlock()
			switch loss.Reason {
			case LossUIResync:
				return Delivery{kind: DeliveryLoss, loss: loss}, ErrCursorInvalidated
			case LossEdgeOverflow:
				return Delivery{kind: DeliveryLoss, loss: loss}, ErrEdgeOverflow
			case LossRecorderOverflow:
				return Delivery{kind: DeliveryLoss, loss: loss}, ErrRecorderGap
			case LossUnspecified:
				return Delivery{}, ErrInvalidInput
			default:
				return Delivery{}, ErrInvalidInput
			}
		}
		if len(sub.queue) > 0 {
			entry := sub.queue[0]
			copy(sub.queue, sub.queue[1:])
			sub.queue[len(sub.queue)-1] = Entry{}
			sub.queue = sub.queue[:len(sub.queue)-1]
			valid := sub.replayValid
			sub.owner.mu.Unlock()
			return Delivery{kind: DeliveryEntry, entry: entry, replayValid: valid}, nil
		}
		wake := sub.wake
		sub.owner.mu.Unlock()
		select {
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		case <-wake:
		}
	}
}

func (sub *Subscription) Status() (SubscriptionStatus, error) {
	if sub == nil || sub.owner == nil {
		return SubscriptionStatus{}, ErrInvalidInput
	}
	sub.owner.mu.Lock()
	defer sub.owner.mu.Unlock()
	status := SubscriptionStatus{Class: sub.class, Capacity: sub.capacity, Queued: len(sub.queue), Lost: sub.lost, Coalesced: sub.coalesced, ReplayValid: sub.replayValid, Degraded: sub.degraded, Closed: sub.closed}
	if sub.loss != nil {
		loss := *sub.loss
		status.PendingLoss = &loss
	}
	return status, nil
}

func (sub *Subscription) Close() error {
	if sub == nil || sub.owner == nil {
		return ErrInvalidInput
	}
	sub.owner.mu.Lock()
	defer sub.owner.mu.Unlock()
	if sub.closed {
		return ErrSubscriptionClosed
	}
	sub.closed = true
	sub.queue = nil
	delete(sub.owner.subs, sub)
	sub.signal()
	return nil
}
