package configdocuments

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/storage"
)

// MemberExpectation distinguishes an absent editable file from a present file
// with an exact digest. Neither a zero hash nor revision zero means absence.
type MemberExpectation struct {
	admitted bool
	absent   bool
	hash     [32]byte
}

func ExpectAbsentMember() MemberExpectation { return MemberExpectation{admitted: true, absent: true} }

func ExpectExistingMember(hash [32]byte) (MemberExpectation, error) {
	if hash == ([32]byte{}) {
		return MemberExpectation{}, &Error{kind: FailureInvalid}
	}
	return MemberExpectation{admitted: true, hash: hash}, nil
}

func (e MemberExpectation) valid() bool {
	return e.admitted && (e.absent && e.hash == ([32]byte{}) || !e.absent && e.hash != ([32]byte{}))
}

// WorkspaceMember is confined to a server-chosen configuration directory below
// the already-pinned workspace root. The RPC logical source name cannot select
// an absolute path, the operational document, bootstrap or another group.
type WorkspaceMember struct {
	binding  storage.ConfigDocumentBinding
	location string
	expected MemberExpectation
	sqlBase  MemberExpectation
	file     parser.SourceFile
}

func NewWorkspaceMember(group storage.ConfigDocumentID, binding storage.ConfigDocumentBinding, expected, admittedSQL MemberExpectation, file parser.SourceFile) (WorkspaceMember, error) {
	if !group.Valid() || !binding.ID().Valid() || !expected.valid() || !admittedSQL.valid() || binding.Path() != file.Path() || !fs.ValidPath(file.Path()) || strings.Contains(file.Path(), "\\") || len(file.Bytes()) < 1 || len(file.Bytes()) > 1<<20 {
		return WorkspaceMember{}, &Error{kind: FailureInvalid}
	}
	owned, err := parser.CloneSourceFile(file)
	if err != nil || owned.SHA256() != file.SHA256() {
		return WorkspaceMember{}, &Error{kind: FailureInvalid}
	}
	groupHash := sha256.Sum256([]byte(group.String()))
	location := path.Join("configuration.d", hex.EncodeToString(groupHash[:]), file.Path())
	return WorkspaceMember{binding: binding, location: location, expected: expected, sqlBase: admittedSQL, file: owned}, nil
}

type setIntentMember struct {
	Document  string   `json:"document"`
	Location  string   `json:"location"`
	Absent    bool     `json:"absent"`
	Base      [32]byte `json:"base"`
	SQLAbsent bool     `json:"sql_absent"`
	SQLBase   [32]byte `json:"sql_base"`
	Candidate [32]byte `json:"candidate"`
}

type setIntentRecord struct {
	Format  uint8             `json:"format"`
	Token   string            `json:"token"`
	Members []setIntentMember `json:"members"`
}

func (r setIntentRecord) staged(index int) string {
	return fmt.Sprintf(".homeward-config-%s-%d.new", r.Token, index)
}
func (r setIntentRecord) displaced(index int) string {
	return fmt.Sprintf(".homeward-config-%s-%d.old", r.Token, index)
}

// checkParents rejects symlink directories as well as symlink leaves. os.Root
// separately confines resolution even against an uncooperative external editor.
func (w *Workspace) checkParents(location string, create bool) error {
	parts := strings.Split(path.Dir(location), "/")
	for index := range parts {
		parent := strings.Join(parts[:index+1], "/")
		info, err := w.root.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := w.root.Mkdir(parent, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return &Error{kind: ReconciliationRequired}
			}
			info, err = w.root.Lstat(parent)
		}
		if errors.Is(err, os.ErrNotExist) && !create {
			return os.ErrNotExist
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return &Error{kind: Unavailable}
		}
	}
	return nil
}

func (w *Workspace) confirmMember(member WorkspaceMember) error {
	parentErr := w.checkParents(member.location, false)
	if errors.Is(parentErr, os.ErrNotExist) && member.expected.absent {
		return nil
	}
	if parentErr != nil {
		return &Error{kind: Conflict}
	}
	info, err := w.root.Lstat(member.location)
	if member.expected.absent {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return &Error{kind: Conflict}
	}
	if err != nil || !info.Mode().IsRegular() {
		return &Error{kind: Conflict}
	}
	current, err := w.read(member.location, 1<<20)
	if err != nil || current.hash != member.expected.hash {
		return &Error{kind: Conflict}
	}
	return nil
}

// SetEdit owns the same mutex/intent as an ordinary operational Edit. All
// members are staged before a generation can be admitted. It never claims an
// atomic filesystem transaction with noncooperating external writers.
type SetEdit struct {
	workspace *Workspace
	record    setIntentRecord
	done      bool
}

func (e *SetEdit) String() string               { return "[REDACTED CONFIG SET EDIT]" }
func (e *SetEdit) GoString() string             { return e.String() }
func (e *SetEdit) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }

func (w *Workspace) BeginSetEdit(members []WorkspaceMember) (*SetEdit, error) {
	return w.beginSetEdit(members, nil)
}

func (w *Workspace) beginSetEdit(members []WorkspaceMember, beforeWrite func() error) (*SetEdit, error) {
	if w == nil || !w.admitted || len(members) < 1 || len(members) > 64 {
		return nil, &Error{kind: FailureInvalid}
	}
	seenID, seenLocation := map[storage.ConfigDocumentID]bool{}, map[string]bool{}
	total := 0
	for _, member := range members {
		if !member.binding.ID().Valid() || !member.expected.valid() || !member.sqlBase.valid() || !fs.ValidPath(member.location) || !strings.HasPrefix(member.location, "configuration.d/") || member.binding.Path() != member.file.Path() || seenID[member.binding.ID()] || seenLocation[member.location] {
			return nil, &Error{kind: FailureInvalid}
		}
		seenID[member.binding.ID()], seenLocation[member.location] = true, true
		total += len(member.file.Bytes())
	}
	if total < 1 || total > 4<<20 {
		return nil, &Error{kind: FailureInvalid}
	}
	w.mu.Lock()
	owned := false
	defer func() {
		if !owned {
			w.mu.Unlock()
		}
	}()
	if w.closed {
		return nil, &Error{kind: Unavailable}
	}
	if _, err := w.root.Lstat(w.intent); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil, &Error{kind: ReconciliationRequired}
	}
	// Every precondition is admitted before the first staging write or mkdir.
	for _, member := range members {
		if err := w.confirmMember(member); err != nil {
			return nil, err
		}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, &Error{kind: Unavailable}
	}
	record := setIntentRecord{Format: 3, Token: hex.EncodeToString(token[:])}
	for _, member := range members {
		record.Members = append(record.Members, setIntentMember{Document: member.binding.ID().String(), Location: member.location, Absent: member.expected.absent, Base: member.expected.hash, SQLAbsent: member.sqlBase.absent, SQLBase: member.sqlBase.hash, Candidate: member.file.SHA256()})
	}
	// Persist the complete intent first. Every later partial failure remains
	// visible to the shared watcher/API as reconciliation required.
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded) > 64<<10 {
		return nil, &Error{kind: FailureInvalid}
	}
	if beforeWrite != nil {
		if err := beforeWrite(); err != nil {
			return nil, err
		}
	}
	if err := w.writeExclusive(w.intent, encoded); err != nil {
		return nil, err
	}
	if err := w.dir.Sync(); err != nil {
		return nil, &Error{kind: ReconciliationRequired}
	}
	for index, member := range members {
		if err := w.writeExclusive(record.staged(index), member.file.Bytes()); err != nil {
			return nil, err
		}
	}
	if err := w.dir.Sync(); err != nil {
		return nil, &Error{kind: ReconciliationRequired}
	}
	for index, member := range members {
		if err := w.confirmMember(member); err != nil {
			return nil, err
		}
		if err := w.checkParents(member.location, true); err != nil {
			return nil, err
		}
		if !member.expected.absent {
			if err := w.root.Rename(member.location, record.displaced(index)); err != nil {
				return nil, &Error{kind: ReconciliationRequired}
			}
			old, err := w.read(record.displaced(index), 1<<20)
			if err != nil || old.hash != member.expected.hash {
				_ = w.root.Link(record.displaced(index), member.location)
				return nil, &Error{kind: ReconciliationRequired}
			}
		}
		if err := w.root.Link(record.staged(index), member.location); err != nil {
			if !member.expected.absent {
				_ = w.root.Link(record.displaced(index), member.location)
			}
			return nil, &Error{kind: ReconciliationRequired}
		}
	}
	if err := w.dir.Sync(); err != nil {
		return nil, &Error{kind: ReconciliationRequired}
	}
	edit := &SetEdit{workspace: w, record: record}
	if err := edit.Confirm(); err != nil {
		return nil, err
	}
	owned = true
	return edit, nil
}

// Confirm is called by the one activation transaction immediately before its
// CAS as well as after staging. It reads directly while this edit owns mu;
// Observe/Read would recursively acquire mu and must never be used here.
func (e *SetEdit) Confirm() error {
	if e == nil || e.workspace == nil || e.done || e.record.Format != 3 {
		return &Error{kind: FailureInvalid}
	}
	for index, member := range e.record.Members {
		if err := e.workspace.checkParents(member.Location, false); err != nil {
			return &Error{kind: ReconciliationRequired}
		}
		current, err := e.workspace.read(member.Location, 1<<20)
		staged, stagedErr := e.workspace.read(e.record.staged(index), 1<<20)
		if err != nil || stagedErr != nil || current.hash != member.Candidate || staged.hash != member.Candidate || !bytes.Equal(current.raw, staged.raw) {
			return &Error{kind: ReconciliationRequired}
		}
		if !member.Absent {
			old, err := e.workspace.read(e.record.displaced(index), 1<<20)
			if err != nil || old.hash != member.Base {
				return &Error{kind: ReconciliationRequired}
			}
		}
	}
	return nil
}

// Finish requires a known SQL outcome and a complete proof that the activated
// source set matches either all candidates or all prior members. No partial
// combination is accepted. A rejected set remains editable/dirty; no inverse
// rename is ever performed over an external editor's newer contents.
func (e *SetEdit) Finish(admitted map[storage.ConfigDocumentID]MemberExpectation, known bool) error {
	if e == nil || e.workspace == nil || e.done {
		return &Error{kind: FailureInvalid}
	}
	e.done = true
	defer e.workspace.mu.Unlock()
	if !known {
		return &Error{kind: ReconciliationRequired}
	}
	allPrior, allCandidate := true, true
	for _, member := range e.record.Members {
		id, err := storage.NewConfigDocumentID(member.Document)
		if err != nil {
			return &Error{kind: ReconciliationRequired}
		}
		actual, ok := admitted[id]
		if !ok || !actual.valid() {
			return &Error{kind: ReconciliationRequired}
		}
		allPrior = allPrior && actual.absent == member.SQLAbsent && actual.hash == member.SQLBase
		allCandidate = allCandidate && !actual.absent && actual.hash == member.Candidate
	}
	if !allPrior && !allCandidate {
		return &Error{kind: ReconciliationRequired}
	}
	// Confirm needs the live edit; its only post-Finish use is this bounded
	// internal check immediately before removing retained staging artifacts.
	e.done = false
	err := e.Confirm()
	e.done = true
	if err != nil {
		return err
	}
	for index, member := range e.record.Members {
		if err := e.workspace.root.Remove(e.record.staged(index)); err != nil {
			return &Error{kind: ReconciliationRequired}
		}
		if !member.Absent {
			if err := e.workspace.root.Remove(e.record.displaced(index)); err != nil {
				return &Error{kind: ReconciliationRequired}
			}
		}
	}
	if err := e.workspace.root.Remove(e.workspace.intent); err != nil || e.workspace.dir.Sync() != nil {
		return &Error{kind: ReconciliationRequired}
	}
	return nil
}
