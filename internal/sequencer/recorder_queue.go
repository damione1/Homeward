package sequencer

// recorderQueue has one fixed body and one logical occupation. The sequencer
// mutex owns every mutation; the zero value has no body and is not an active
// Recorder mailbox. Payload accounting belongs only to Subscription.queueBytes.
type recorderQueue struct {
	body  []Entry
	head  int
	count int
}

func newRecorderQueue(capacity int) (recorderQueue, error) {
	if capacity < 1 || capacity > MaxQueueCapacity {
		return recorderQueue{}, ErrInvalidInput
	}
	return recorderQueue{body: make([]Entry, capacity)}, nil
}

func (q *recorderQueue) len() int { return q.count }

// push has no eviction policy. Its caller establishes a free slot after the
// count and byte limits have been applied. A broken internal invariant must
// fail explicitly rather than grow the body or silently discard an entry.
func (q *recorderQueue) push(entry Entry) {
	if len(q.body) == 0 || q.head < 0 || q.head >= len(q.body) || q.count < 0 || q.count >= len(q.body) {
		panic("invalid recorder queue push")
	}
	q.body[(q.head+q.count)%len(q.body)] = entry
	q.count++
}

func (q *recorderQueue) pop() (Entry, bool) {
	if q.count == 0 {
		return Entry{}, false
	}
	if len(q.body) == 0 || q.head < 0 || q.head >= len(q.body) || q.count < 0 || q.count > len(q.body) {
		panic("invalid recorder queue pop")
	}
	entry := q.body[q.head]
	q.body[q.head] = Entry{}
	q.count--
	if q.count == 0 {
		q.head = 0
	} else {
		q.head = (q.head + 1) % len(q.body)
	}
	return entry, true
}

// release clears references before detaching the sole owned allocation. A
// capture may give the returned body to its successor under the same mutex;
// a close simply drops it. The old owner is zero before either operation ends.
func (q *recorderQueue) release() []Entry {
	clear(q.body)
	body := q.body
	*q = recorderQueue{}
	return body
}
