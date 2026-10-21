package configdocuments

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"sync"

	"github.com/damione1/Homeward/internal/storage"
)

const (
	journalCapacity      = 256
	subscriptionCapacity = 1024
)

type DirtyState uint8

const (
	DirtyUnknown DirtyState = iota
	Clean
	DirtyUnvalidated
	DirtyRejected
	DirtyReconciling
	DirtyUnavailable
)

func validDirty(state DirtyState) bool {
	switch state {
	case Clean, DirtyUnvalidated, DirtyRejected, DirtyReconciling, DirtyUnavailable:
		return true
	case DirtyUnknown:
		return false
	default:
		return false
	}
}

type Change struct {
	id        storage.ConfigDocumentID
	revision  storage.ConfigVersionRevision
	hash      [32]byte
	workspace [32]byte
	dirty     DirtyState
}

func NewChange(id storage.ConfigDocumentID, revision storage.ConfigVersionRevision, source, workspace [32]byte, dirty DirtyState) (Change, error) {
	if !id.Valid() || !revision.Valid() || source == ([32]byte{}) || !validDirty(dirty) || workspace == ([32]byte{}) && dirty != DirtyReconciling && dirty != DirtyUnavailable || dirty == Clean && workspace != source {
		return Change{}, &Error{kind: FailureInvalid}
	}
	return Change{id: id, revision: revision, hash: source, workspace: workspace, dirty: dirty}, nil
}
func (c Change) Document() storage.ConfigDocumentID      { return c.id }
func (c Change) Revision() storage.ConfigVersionRevision { return c.revision }
func (c Change) SourceSHA256() [32]byte                  { return c.hash }
func (c Change) WorkspaceSHA256() [32]byte               { return c.workspace }
func (c Change) Dirty() DirtyState                       { return c.dirty }

type Event struct {
	sequence uint64
	changes  []Change
}

func (e Event) Sequence() uint64  { return e.sequence }
func (e Event) Changes() []Change { return append([]Change(nil), e.changes...) }

// ResyncReason is an explicit terminal result, never an empty successful EOF.
type ResyncReason uint8

const (
	ResyncUnknown ResyncReason = iota
	ResyncEpochChanged
	ResyncTokenInvalid
	ResyncHistoryLost
	ResyncAuthorityChanged
	ResyncActivationSignal
	ResyncClosed
)

type Resync struct{ reason ResyncReason }

func (r *Resync) Reason() ResyncReason {
	if r == nil {
		return ResyncUnknown
	}
	return r.reason
}
func (*Resync) Error() string { return "configuration documents: resynchronization required" }

type Cursor struct {
	epoch     [16]byte
	sequence  uint64
	authority [32]byte
	valid     bool
}

func (c Cursor) Bind(authority [32]byte) (Cursor, error) {
	if !c.valid || authority == ([32]byte{}) {
		return Cursor{}, &Error{kind: FailureInvalid}
	}
	c.authority = authority
	return c, nil
}
func (c Cursor) Authority() [32]byte { return c.authority }

// Journal records only changes this owner actually observes. The activation
// owner's coalesced signal must produce ResyncActivationSignal, never invented
// journal entries or a fabricated complete activation replay.
type Journal struct {
	admitted bool
	mu       sync.Mutex
	epoch    [16]byte
	key      [32]byte
	next     uint64
	ring     []Event
	subs     map[*Subscription]struct{}
	closed   bool
}

func NewJournal() (*Journal, error) {
	j := &Journal{admitted: true, subs: make(map[*Subscription]struct{})}
	if _, err := rand.Read(j.epoch[:]); err != nil {
		return nil, &Error{kind: Unavailable}
	}
	if _, err := rand.Read(j.key[:]); err != nil {
		return nil, &Error{kind: Unavailable}
	}
	if j.epoch == ([16]byte{}) || j.key == ([32]byte{}) {
		return nil, &Error{kind: Unavailable}
	}
	return j, nil
}
func (j *Journal) Append(changes []Change) error {
	if j == nil || !j.admitted || len(changes) < 1 || len(changes) > 256 {
		return &Error{kind: FailureInvalid}
	}
	seen := make(map[storage.ConfigDocumentID]bool, len(changes))
	for _, c := range changes {
		if _, err := NewChange(c.id, c.revision, c.hash, c.workspace, c.dirty); err != nil || seen[c.id] {
			return &Error{kind: FailureInvalid}
		}
		seen[c.id] = true
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.next == ^uint64(0) {
		return &Error{kind: Unavailable}
	}
	j.next++
	if len(j.ring) == journalCapacity {
		copy(j.ring, j.ring[1:])
		j.ring = j.ring[:journalCapacity-1]
	}
	j.ring = append(j.ring, Event{sequence: j.next, changes: append([]Change(nil), changes...)})
	for sub := range j.subs {
		select {
		case sub.signal <- struct{}{}:
		default:
		}
	}
	return nil
}
func (j *Journal) Position() (Cursor, error) {
	if j == nil || !j.admitted {
		return Cursor{}, &Error{kind: FailureInvalid}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return Cursor{}, &Error{kind: Unavailable}
	}
	return Cursor{epoch: j.epoch, sequence: j.next, valid: true}, nil
}
func (c Cursor) Equal(other Cursor) bool {
	return c.valid && other.valid && c.epoch == other.epoch && c.sequence == other.sequence
}

func (j *Journal) Encode(c Cursor) (string, error) {
	if j == nil || !j.admitted || !c.valid || c.epoch != j.epoch || c.authority == ([32]byte{}) {
		return "", &Error{kind: FailureInvalid}
	}
	raw := make([]byte, 56, 88)
	copy(raw, c.epoch[:])
	binary.BigEndian.PutUint64(raw[16:], c.sequence)
	copy(raw[24:], c.authority[:])
	mac := hmac.New(sha256.New, j.key[:])
	_, _ = mac.Write(raw)
	raw = append(raw, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func (j *Journal) Decode(token string) (Cursor, error) {
	if j == nil || !j.admitted || len(token) != 118 {
		return Cursor{}, &Resync{reason: ResyncTokenInvalid}
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 88 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return Cursor{}, &Resync{reason: ResyncTokenInvalid}
	}
	var epoch [16]byte
	copy(epoch[:], raw[:16])
	if epoch != j.epoch {
		return Cursor{}, &Resync{reason: ResyncEpochChanged}
	}
	mac := hmac.New(sha256.New, j.key[:])
	_, _ = mac.Write(raw[:56])
	if !hmac.Equal(raw[56:], mac.Sum(nil)) {
		return Cursor{}, &Resync{reason: ResyncTokenInvalid}
	}
	var authority [32]byte
	copy(authority[:], raw[24:56])
	if authority == ([32]byte{}) {
		return Cursor{}, &Resync{reason: ResyncTokenInvalid}
	}
	return Cursor{epoch: epoch, sequence: binary.BigEndian.Uint64(raw[16:24]), authority: authority, valid: true}, nil
}

type Subscription struct {
	journal *Journal
	signal  chan struct{}
	cursor  Cursor
	closed  bool
}

// Subscribe registers first. Its caller then reads the authoritative SQL and
// workspace cut and compares its read token; an intervening change is replayed
// here or terminates with resync. It must also register the activation signal
// before that authoritative read, because that signal is not this journal.
func (j *Journal) Subscribe(cursor Cursor) (*Subscription, error) {
	if j == nil || !j.admitted || !cursor.valid {
		return nil, &Error{kind: FailureInvalid}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, &Resync{reason: ResyncClosed}
	}
	if cursor.epoch != j.epoch {
		return nil, &Resync{reason: ResyncEpochChanged}
	}
	if len(j.subs) >= subscriptionCapacity {
		return nil, &Error{kind: Unavailable}
	}
	if err := j.retainedLocked(cursor); err != nil {
		return nil, err
	}
	sub := &Subscription{journal: j, signal: make(chan struct{}, 1), cursor: cursor}
	j.subs[sub] = struct{}{}
	return sub, nil
}
func (j *Journal) retainedLocked(c Cursor) error {
	if c.sequence > j.next {
		return &Resync{reason: ResyncTokenInvalid}
	}
	if len(j.ring) > 0 && c.sequence < j.ring[0].sequence-1 {
		return &Resync{reason: ResyncHistoryLost}
	}
	return nil
}
func (s *Subscription) Changes() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.signal
}
func (s *Subscription) Read() ([]Event, Cursor, error) {
	if s == nil || s.journal == nil {
		return nil, Cursor{}, &Error{kind: FailureInvalid}
	}
	j := s.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	if s.closed || j.closed {
		return nil, Cursor{}, &Resync{reason: ResyncClosed}
	}
	if err := j.retainedLocked(s.cursor); err != nil {
		return nil, Cursor{}, err
	}
	events := make([]Event, 0, 1)
	for _, event := range j.ring {
		if event.sequence > s.cursor.sequence {
			events = append(events, Event{sequence: event.sequence, changes: append([]Change(nil), event.changes...)})
			s.cursor.sequence = event.sequence
			break
		}
	}
	return events, s.cursor, nil
}
func (s *Subscription) Close() {
	if s == nil || s.journal == nil {
		return
	}
	j := s.journal
	j.mu.Lock()
	defer j.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	delete(j.subs, s)
	close(s.signal)
}
func (j *Journal) Close() {
	if j == nil || !j.admitted {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	for sub := range j.subs {
		sub.closed = true
		close(sub.signal)
		delete(j.subs, sub)
	}
}

func (s *Subscription) Next(ctx context.Context) ([]Event, Cursor, error) {
	if !usableContext(ctx) {
		return nil, Cursor{}, &Error{kind: FailureInvalid}
	}
	for {
		events, cursor, err := s.Read()
		if err != nil || len(events) > 0 {
			return events, cursor, err
		}
		select {
		case <-ctx.Done():
			return nil, Cursor{}, ctx.Err()
		case _, ok := <-s.signal:
			if !ok {
				return nil, Cursor{}, &Resync{reason: ResyncClosed}
			}
		}
	}
}
