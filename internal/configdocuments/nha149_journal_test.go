package configdocuments

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/storage"
)

func nha149Change(t *testing.T, n int64) Change {
	t.Helper()
	id, _ := storage.NewConfigDocumentID("operational.main")
	revision, _ := storage.NewConfigVersionRevision(n)
	change, err := NewChange(id, revision, sha256.Sum256([]byte("admitted")), sha256.Sum256([]byte("dirty")), DirtyRejected)
	if err != nil {
		t.Fatal(err)
	}
	return change
}
func nha149Cursor(t *testing.T, j *Journal) Cursor {
	t.Helper()
	cursor, err := j.Position()
	if err != nil {
		t.Fatal(err)
	}
	cursor, err = cursor.Bind(sha256.Sum256([]byte("authority")))
	if err != nil {
		t.Fatal(err)
	}
	return cursor
}
func TestNHA149JournalRegisterThenReadRetainsInterveningChangeAndCopies(t *testing.T) {
	j, err := NewJournal()
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	cursor := nha149Cursor(t, j)
	sub, err := j.Subscribe(cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	change := nha149Change(t, 1)
	if err := j.Append([]Change{change}); err != nil {
		t.Fatal(err)
	}
	events, through, err := sub.Read()
	if err != nil || len(events) != 1 || len(events[0].Changes()) != 1 || events[0].Changes()[0] != change || cursor.Equal(through) {
		t.Fatal("intervening change disappeared", err)
	}
	copyOfChanges := events[0].Changes()
	copyOfChanges[0] = Change{}
	token, err := j.Encode(through)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := j.Decode(token)
	if err != nil || !decoded.Equal(through) || decoded.Authority() != cursor.Authority() {
		t.Fatal("resume token lost admitted authority", err)
	}
	resumed, err := j.Subscribe(cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	retained, _, err := resumed.Read()
	if err != nil || retained[0].Changes()[0] != change {
		t.Fatal("caller mutated retained event", err)
	}
}
func TestNHA149JournalSlowClientEpochAndTamperingRequireTypedResync(t *testing.T) {
	j, err := NewJournal()
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	cursor := nha149Cursor(t, j)
	token, err := j.Encode(cursor)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := j.Subscribe(cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for n := int64(1); n <= journalCapacity+1; n++ {
		if err := j.Append([]Change{nha149Change(t, n)}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err = sub.Read()
	var resync *Resync
	if !errors.As(err, &resync) || resync.Reason() != ResyncHistoryLost {
		t.Fatal("slow client received false complete history", err)
	}
	other, err := NewJournal()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, err = other.Decode(token)
	if !errors.As(err, &resync) || resync.Reason() != ResyncEpochChanged {
		t.Fatal("restart was not explicit", err)
	}
	tampered := []byte(token)
	if tampered[40] == 'A' {
		tampered[40] = 'B'
	} else {
		tampered[40] = 'A'
	}
	_, err = j.Decode(string(tampered))
	if !errors.As(err, &resync) || resync.Reason() != ResyncTokenInvalid {
		t.Fatal("tampered token admitted", err)
	}
}
func TestNHA149JournalDoesNotAdvanceResumePastUnsentEvents(t *testing.T) {
	j, err := NewJournal()
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	cursor := nha149Cursor(t, j)
	sub, err := j.Subscribe(cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for n := int64(1); n <= 3; n++ {
		if err := j.Append([]Change{nha149Change(t, n)}); err != nil {
			t.Fatal(err)
		}
	}
	first, through, err := sub.Read()
	if err != nil || len(first) != 1 || through.sequence != 1 {
		t.Fatal("first emitted batch advanced over un-emitted entries", err)
	}
	resumed, err := j.Subscribe(through)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	second, _, err := resumed.Read()
	if err != nil || len(second) != 1 || second[0].Sequence() != 2 {
		t.Fatal("resume skipped the next un-emitted event", err)
	}
}

type nha149NilContext struct{ context.Context }

func TestNHA149ZeroOwnersAndTypedNilContextFailClosed(t *testing.T) {
	var journal Journal
	if _, err := journal.Position(); err == nil {
		t.Fatal("zero journal acquired valid epoch")
	}
	if err := journal.Append([]Change{nha149Change(t, 1)}); err == nil {
		t.Fatal("zero journal admitted observation")
	}
	var workspace Workspace
	if _, err := workspace.Observe(); err == nil {
		t.Fatal("zero workspace observed a file")
	}
	if _, err := workspace.BeginEdit(sha256.Sum256([]byte("base")), sha256.Sum256([]byte("sql")), []byte("new")); err == nil {
		t.Fatal("zero workspace staged source")
	}
	var service Service
	if _, err := service.Read(context.Background(), operationalID()); err == nil {
		t.Fatal("zero service reached its uninitialized owners")
	}
	if _, err := service.acquireMutation(context.Background()); err == nil {
		t.Fatal("zero service reserved a mutation")
	}
	var nilContext *nha149NilContext
	if usableContext(nilContext) {
		t.Fatal("typed nil context admitted")
	}
	journal.Close()
	workspace.Close()
	service.Close()
	(&Watch{}).Close()
}
