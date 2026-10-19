package sequencer

import "unsafe"

// SubscribeTriggerViewAfter retains the established view subscription's
// replay/live boundary and loss policy. It precharges the entire retained
// suffix, including its slice backing, before copying entries or attaching a
// subscription. The caller must separately reserve MaxViewQueueBytes for the
// existing live mailbox: this API does not silently change that shared limit.
func (s *Sequencer) SubscribeTriggerViewAfter(cursor Cursor, maxEntries, maxBytes int) (*Subscription, []Entry, Cursor, int, error) {
	if s == nil || s.journal == nil || maxEntries < 1 || maxEntries > MaxJournalCapacity || maxBytes <= 0 || maxBytes > MaxJournalBytes {
		return nil, nil, Cursor{}, 0, ErrInvalidInput
	}
	if _, _, err := cursor.Components(); err != nil {
		return nil, nil, Cursor{}, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneJournal(s.MonotonicNow())
	if s.registryState == registryUnavailable {
		return nil, nil, Cursor{}, 0, ErrRegistryUnavailable
	}
	if cursor.epoch != s.epoch {
		return nil, nil, Cursor{}, 0, ErrEpochMismatch
	}
	if cursor.seq > s.seq {
		return nil, nil, Cursor{}, 0, ErrInvalidInput
	}
	available := s.seq - cursor.seq
	if available > uint64(s.count) || available > uint64(maxEntries) {
		return nil, nil, Cursor{}, 0, ErrCursorExpired
	}
	if len(s.subs) >= MaxSubscriptions {
		return nil, nil, Cursor{}, 0, ErrSubscriptionFull
	}
	charge := int(available) * int(unsafe.Sizeof(Entry{}))
	if charge > maxBytes {
		return nil, nil, Cursor{}, 0, ErrTriggerViewBudget
	}
	for i := uint64(0); i < available; i++ {
		seq := cursor.seq + i + 1
		item := s.journalSizes[(seq-1)%uint64(len(s.journal))]
		if item <= 0 || item > maxBytes-charge {
			return nil, nil, Cursor{}, 0, ErrTriggerViewBudget
		}
		charge += item
	}
	replay := make([]Entry, 0, available)
	for i := uint64(0); i < available; i++ {
		seq := cursor.seq + i + 1
		replay = append(replay, s.journal[(seq-1)%uint64(len(s.journal))])
	}
	sub := &Subscription{owner: s, class: SubscriptionView, capacity: s.limits.ui,
		queue: make([]Entry, 0, s.limits.ui), wake: make(chan struct{}), replayValid: true}
	s.subs[sub] = struct{}{}
	return sub, replay, Cursor{epoch: s.epoch, seq: s.seq}, charge, nil
}
