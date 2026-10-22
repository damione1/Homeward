package configdocuments

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/damione1/Homeward/internal/storage"
)

func (r setIntentRecord) valid() bool {
	if r.Format != 3 || len(r.Token) != 32 || len(r.Members) < 1 || len(r.Members) > 64 {
		return false
	}
	token, err := hex.DecodeString(r.Token)
	if err != nil || len(token) != 16 || hex.EncodeToString(token) != r.Token {
		return false
	}
	ids, locations := map[string]bool{}, map[string]bool{}
	for _, member := range r.Members {
		if _, err := storage.NewConfigDocumentID(member.Document); err != nil {
			return false
		}
		if !fs.ValidPath(member.Location) || !strings.HasPrefix(member.Location, "configuration.d/") || strings.Contains(member.Location, "\\") || ids[member.Document] || locations[member.Location] || member.Candidate == ([32]byte{}) {
			return false
		}
		if member.Absent != (member.Base == ([32]byte{})) || member.SQLAbsent != (member.SQLBase == ([32]byte{})) {
			return false
		}
		ids[member.Document], locations[member.Location] = true, true
	}
	return true
}

func (w *Workspace) readSetIntentLocked() (setIntentRecord, bool, error) {
	if w.closed {
		return setIntentRecord{}, false, &Error{kind: Unavailable}
	}
	if _, err := w.root.Lstat(w.intent); errors.Is(err, os.ErrNotExist) {
		return setIntentRecord{}, false, nil
	}
	raw, err := w.read(w.intent, 64<<10)
	if err != nil {
		return setIntentRecord{}, false, &Error{kind: ReconciliationRequired}
	}
	var header struct {
		Format uint8 `json:"format"`
	}
	if json.Unmarshal(raw.raw, &header) != nil {
		return setIntentRecord{}, false, &Error{kind: ReconciliationRequired}
	}
	if header.Format != 3 {
		return setIntentRecord{}, false, nil
	}
	var record setIntentRecord
	decoder := json.NewDecoder(bytes.NewReader(raw.raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || !record.valid() {
		return setIntentRecord{}, true, &Error{kind: ReconciliationRequired}
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return setIntentRecord{}, true, &Error{kind: ReconciliationRequired}
	}
	return record, true, nil
}

// PendingSetDocuments supplies only typed SQL identities to trusted recovery.
// The persisted filesystem paths never become a public or configurable locator.
func (w *Workspace) PendingSetDocuments() ([]storage.ConfigDocumentID, bool, error) {
	if w == nil || !w.admitted {
		return nil, false, &Error{kind: FailureInvalid}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	record, present, err := w.readSetIntentLocked()
	if err != nil || !present {
		return nil, present, err
	}
	ids := make([]storage.ConfigDocumentID, 0, len(record.Members))
	for _, member := range record.Members {
		id, _ := storage.NewConfigDocumentID(member.Document)
		ids = append(ids, id)
	}
	return ids, true, nil
}

// ReconcileSet never rewrites an editable path. The caller must first resolve
// any uncertain 029 transaction, then supply every current SQL source member.
// A partly published filesystem set remains explicitly unavailable; recovery
// never guesses that an absent include or a mixed SQL cut is fully restored.
func (w *Workspace) ReconcileSet(admitted map[storage.ConfigDocumentID]MemberExpectation) (bool, error) {
	if w == nil || !w.admitted {
		return false, &Error{kind: FailureInvalid}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	record, present, err := w.readSetIntentLocked()
	if err != nil || !present {
		return present, err
	}
	allPrior, allCandidate := true, true
	for index, member := range record.Members {
		id, _ := storage.NewConfigDocumentID(member.Document)
		actual, ok := admitted[id]
		if !ok || !actual.valid() {
			return true, &Error{kind: ReconciliationRequired}
		}
		allPrior = allPrior && actual.absent == member.SQLAbsent && actual.hash == member.SQLBase
		allCandidate = allCandidate && !actual.absent && actual.hash == member.Candidate
		if err := w.checkParents(member.Location, false); err != nil {
			return true, &Error{kind: ReconciliationRequired}
		}
		current, err := w.read(member.Location, 1<<20)
		if err != nil || current.hash != member.Candidate {
			return true, &Error{kind: ReconciliationRequired}
		}
		for _, artifact := range []struct {
			name string
			hash [32]byte
		}{{record.staged(index), member.Candidate}, {record.displaced(index), member.Base}} {
			if artifact.hash == ([32]byte{}) {
				continue
			}
			if _, err := w.root.Lstat(artifact.name); errors.Is(err, os.ErrNotExist) {
				continue
			}
			value, err := w.read(artifact.name, 1<<20)
			if err != nil || value.hash != artifact.hash {
				return true, &Error{kind: ReconciliationRequired}
			}
		}
	}
	if !allPrior && !allCandidate {
		return true, &Error{kind: ReconciliationRequired}
	}
	for index, member := range record.Members {
		names := []string{record.staged(index)}
		if !member.Absent {
			names = append(names, record.displaced(index))
		}
		for _, name := range names {
			if err := w.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return true, &Error{kind: ReconciliationRequired}
			}
		}
	}
	if err := w.root.Remove(w.intent); err != nil || w.dir.Sync() != nil {
		return true, &Error{kind: ReconciliationRequired}
	}
	return true, nil
}
