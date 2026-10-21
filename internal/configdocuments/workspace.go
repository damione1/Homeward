// Package configdocuments owns controlled edits and dirty observations of the
// editable workspace. It does not own configuration activation or a scheduler.
package configdocuments

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/damione1/Homeward/internal/config"
)

type Failure uint8

const (
	FailureInvalid Failure = iota
	Conflict
	Unavailable
	ReconciliationRequired
	IncompleteHistory
)

type Error struct{ kind Failure }

func (e *Error) Kind() Failure {
	if e == nil {
		return FailureInvalid
	}
	return e.kind
}
func (e *Error) Error() string {
	if e == nil {
		return "configuration documents: invalid failure"
	}
	switch e.kind {
	case Conflict:
		return "configuration documents: version conflict"
	case Unavailable:
		return "configuration documents: unavailable"
	case ReconciliationRequired:
		return "configuration documents: workspace reconciliation required"
	case IncompleteHistory:
		return "configuration documents: historical generation partially purged"
	case FailureInvalid:
		return "configuration documents: invalid input"
	default:
		return "configuration documents: invalid failure"
	}
}

// ExactSource is deliberately redacted by every default formatter. Its bytes
// are trusted composition input for the one activation pipeline, never a page.
type ExactSource struct {
	raw     []byte
	hash    [32]byte
	updated time.Time
}

func (s ExactSource) SHA256() [32]byte             { return s.hash }
func (s ExactSource) UpdatedAt() time.Time         { return s.updated }
func (s ExactSource) Bytes() []byte                { return bytes.Clone(s.raw) }
func (s ExactSource) String() string               { return "[REDACTED CONFIG SOURCE]" }
func (s ExactSource) GoString() string             { return s.String() }
func (s ExactSource) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

type Observation struct {
	hash             [32]byte
	updated          time.Time
	present, pending bool
}

func (o Observation) SHA256() ([32]byte, bool)     { return o.hash, o.present }
func (o Observation) UpdatedAt() (time.Time, bool) { return o.updated, o.present }
func (o Observation) ReconciliationPending() bool  { return o.pending }

// Workspace pins the selected parent directory. Every participant, including
// watcher150, must share this owner instead of running another writer.
type Workspace struct {
	admitted              bool
	mu                    sync.Mutex
	root                  *os.Root
	dir                   *os.File
	name, logical, intent string
	closed                bool
}

func NewWorkspace(startup config.StartupInputs) (*Workspace, error) {
	physical, logical, ok := startup.WorkspaceDocumentLocation()
	if !ok {
		return nil, &Error{kind: FailureInvalid}
	}
	return OpenWorkspace(physical, logical)
}

// OpenWorkspace accepts a trusted server-selected path, never an RPC path.
func OpenWorkspace(physical, logical string) (*Workspace, error) {
	if !filepath.IsAbs(physical) || filepath.Clean(physical) != physical || logical == "" || strings.ContainsRune(logical, '\x00') || len(logical) > 4096 {
		return nil, &Error{kind: FailureInvalid}
	}
	parent := filepath.Dir(physical)
	before, err := os.Stat(parent)
	if err != nil || !before.IsDir() {
		return nil, &Error{kind: Unavailable}
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	dir, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, &Error{kind: Unavailable}
	}
	after, err := dir.Stat()
	if err != nil || !os.SameFile(before, after) {
		dir.Close()
		root.Close()
		return nil, &Error{kind: Unavailable}
	}
	id := sha256.Sum256([]byte(filepath.Base(physical)))
	w := &Workspace{admitted: true, root: root, dir: dir, name: filepath.Base(physical), logical: logical, intent: ".homeward-config-" + hex.EncodeToString(id[:8]) + ".pending"}
	return w, nil
}

func (w *Workspace) Close() error {
	if w == nil || !w.admitted {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return errors.Join(w.dir.Close(), w.root.Close())
}

func (w *Workspace) read(name string, limit int64) (ExactSource, error) {
	// os.Root resolves internal symlinks even with O_NOFOLLOW in the caller's
	// flags. Admit the leaf itself, then compare the opened descriptor with both
	// path observations; the flag alone is not a leaf-admission proof.
	leaf, err := w.root.Lstat(name)
	if err != nil || !leaf.Mode().IsRegular() || leaf.Size() < 1 || leaf.Size() > limit {
		return ExactSource{}, &Error{kind: Unavailable}
	}
	f, err := w.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ExactSource{}, &Error{kind: Unavailable}
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(leaf, before) || before.Size() < 1 || before.Size() > limit {
		return ExactSource{}, &Error{kind: Unavailable}
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	after, statErr := f.Stat()
	pathInfo, pathErr := w.root.Lstat(name)
	if err != nil || statErr != nil || pathErr != nil || len(raw) < 1 || int64(len(raw)) > limit ||
		!os.SameFile(before, after) || !os.SameFile(after, pathInfo) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return ExactSource{}, &Error{kind: Conflict}
	}
	return ExactSource{raw: raw, hash: sha256.Sum256(raw), updated: after.ModTime().UTC()}, nil
}

func (w *Workspace) Observe() (Observation, error) {
	if w == nil || !w.admitted {
		return Observation{}, &Error{kind: FailureInvalid}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return Observation{}, &Error{kind: Unavailable}
	}
	s, err := w.read(w.name, 1<<20)
	if err != nil {
		return Observation{}, err
	}
	_, pendingErr := w.root.Lstat(w.intent)
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return Observation{}, &Error{kind: Unavailable}
	}
	return Observation{hash: s.hash, updated: s.updated, present: true, pending: pendingErr == nil}, nil
}

type intentRecord struct {
	Format    uint8    `json:"format"`
	Token     string   `json:"token"`
	Base      [32]byte `json:"base"`
	SQLBase   [32]byte `json:"sql_base"`
	Candidate [32]byte `json:"candidate"`
}

func (r intentRecord) valid() bool {
	if r.Format != 2 || len(r.Token) != 32 || r.Base == ([32]byte{}) || r.SQLBase == ([32]byte{}) || r.Candidate == ([32]byte{}) {
		return false
	}
	b, err := hex.DecodeString(r.Token)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == r.Token
}
func (r intentRecord) staged() string    { return ".homeward-config-" + r.Token + ".new" }
func (r intentRecord) displaced() string { return ".homeward-config-" + r.Token + ".old" }

func (w *Workspace) writeExclusive(name string, raw []byte) error {
	f, err := w.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return &Error{kind: ReconciliationRequired}
	}
	n, err := f.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return &Error{kind: ReconciliationRequired}
	}
	return nil
}

// Edit holds the shared writer mutex through the caller's activation. An intent
// survives every uncertain boundary. Failed attempts never restore by rename
// over an external editor's file. There is no universal filesystem CAS claim:
// noncooperating writers are checked before displacement, after displacement,
// after publication and by 029 twice at the SQL CAS boundary.
type Edit struct {
	workspace *Workspace
	record    intentRecord
	source    ExactSource
	done      bool
}

func (e *Edit) SourceSHA256() [32]byte {
	if e == nil {
		return [32]byte{}
	}
	return e.source.hash
}
func (e *Edit) String() string               { return "[REDACTED CONFIG EDIT]" }
func (e *Edit) GoString() string             { return e.String() }
func (e *Edit) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }

// BeginEdit takes independent exact hashes for the editable workspace and the
// currently admitted SQL source. The caller must admit document revision/hash
// before staging; a prior rejected workspace may differ from SQL legitimately.
func (w *Workspace) BeginEdit(expected, admittedSQL [32]byte, raw []byte) (*Edit, error) {
	if w == nil || !w.admitted || expected == ([32]byte{}) || admittedSQL == ([32]byte{}) || len(raw) < 1 || len(raw) > 1<<20 {
		return nil, &Error{kind: FailureInvalid}
	}
	w.mu.Lock()
	unlock := true
	defer func() {
		if unlock {
			w.mu.Unlock()
		}
	}()
	if w.closed {
		return nil, &Error{kind: Unavailable}
	}
	if _, err := w.root.Lstat(w.intent); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil, &Error{kind: ReconciliationRequired}
	}
	base, err := w.read(w.name, 1<<20)
	if err != nil {
		return nil, err
	}
	if base.hash != expected {
		return nil, &Error{kind: Conflict}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, &Error{kind: Unavailable}
	}
	r := intentRecord{Format: 2, Token: hex.EncodeToString(token[:]), Base: base.hash, SQLBase: admittedSQL, Candidate: sha256.Sum256(raw)}
	if err := w.writeExclusive(r.staged(), raw); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	if err := w.writeExclusive(w.intent, encoded); err != nil {
		return nil, err
	}
	if err := w.dir.Sync(); err != nil {
		return nil, &Error{kind: ReconciliationRequired}
	}
	confirmed, err := w.read(w.name, 1<<20)
	if err != nil || confirmed.hash != base.hash {
		return nil, &Error{kind: Conflict}
	}
	if err := w.root.Rename(w.name, r.displaced()); err != nil {
		return nil, &Error{kind: ReconciliationRequired}
	}
	if err := w.dir.Sync(); err != nil {
		return nil, &Error{kind: ReconciliationRequired}
	}
	displaced, err := w.read(r.displaced(), 1<<20)
	if err != nil || displaced.hash != base.hash {
		// Link refuses to overwrite a concurrently recreated path.
		_ = w.root.Link(r.displaced(), w.name)
		_ = w.dir.Sync()
		return nil, &Error{kind: ReconciliationRequired}
	}
	if err := w.root.Link(r.staged(), w.name); err != nil {
		_ = w.root.Link(r.displaced(), w.name)
		_ = w.dir.Sync()
		return nil, &Error{kind: ReconciliationRequired}
	}
	if err := w.dir.Sync(); err != nil {
		return nil, &Error{kind: ReconciliationRequired}
	}
	next, err := w.read(w.name, 1<<20)
	old, oldErr := w.read(r.displaced(), 1<<20)
	if err != nil || oldErr != nil || next.hash != r.Candidate || old.hash != r.Base {
		return nil, &Error{kind: ReconciliationRequired}
	}
	unlock = false
	return &Edit{workspace: w, record: r, source: next}, nil
}

// Finish clears staging only when SQL's exact source is known to be either the
// candidate or the admitted SQL base captured before staging. The physical Base
// may already be dirty and is used only to verify the displaced artifact. When
// SQL remains at its admitted base, the candidate remains an
// editable dirty file; failure never rolls it back over an external editor.
// A caller facing CommitUnknown/CommittedReconciling must pass false and retain
// the intent; later attempts remain blocked until explicit reconciliation.
func (e *Edit) Finish(sqlSource [32]byte, known bool) error {
	if e == nil || e.workspace == nil || e.done {
		return &Error{kind: FailureInvalid}
	}
	e.done = true
	w := e.workspace
	defer w.mu.Unlock()
	if !known || sqlSource != e.record.Candidate && sqlSource != e.record.SQLBase {
		return &Error{kind: ReconciliationRequired}
	}
	current, err := w.read(w.name, 1<<20)
	old, oldErr := w.read(e.record.displaced(), 1<<20)
	if err != nil || oldErr != nil || current.hash != e.record.Candidate || old.hash != e.record.Base {
		return &Error{kind: ReconciliationRequired}
	}
	for _, name := range []string{e.record.staged(), e.record.displaced(), w.intent} {
		if err := w.root.Remove(name); err != nil {
			return &Error{kind: ReconciliationRequired}
		}
	}
	if err := w.dir.Sync(); err != nil {
		return &Error{kind: ReconciliationRequired}
	}
	return nil
}

// Reconcile never writes the editable path. It may retire an interrupted intent
// only after a trusted SQL reader supplies the admitted exact source digest and
// every retained artifact still matches that intent. The caller must first
// resolve any uncertain 029 transaction; observing a SQL pointer alone is not
// proof that an in-flight COMMIT cannot still complete. A resolved rollback
// leaves the exact candidate editable and dirty, just like a known rejection.
// Legacy intents without the separate admitted SQL base cannot prove this and
// remain explicit, preserving external edits for manual/watcher reconciliation.
func (w *Workspace) Reconcile(sqlSource [32]byte) (bool, error) {
	if w == nil || !w.admitted || sqlSource == ([32]byte{}) {
		return false, &Error{kind: FailureInvalid}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false, &Error{kind: Unavailable}
	}
	if _, err := w.root.Lstat(w.intent); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	intent, err := w.read(w.intent, 4096)
	if err != nil {
		return true, &Error{kind: ReconciliationRequired}
	}
	var r intentRecord
	d := json.NewDecoder(bytes.NewReader(intent.raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || !r.valid() {
		return true, &Error{kind: ReconciliationRequired}
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return true, &Error{kind: ReconciliationRequired}
	}
	current, err := w.read(w.name, 1<<20)
	if err != nil || current.hash != r.Candidate {
		return true, &Error{kind: ReconciliationRequired}
	}
	if sqlSource != r.SQLBase && sqlSource != r.Candidate {
		return true, &Error{kind: ReconciliationRequired}
	}
	for _, artifact := range []struct {
		name string
		hash [32]byte
	}{{r.staged(), r.Candidate}, {r.displaced(), r.Base}} {
		source, err := w.read(artifact.name, 1<<20)
		if err != nil {
			if _, statErr := w.root.Lstat(artifact.name); errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return true, &Error{kind: ReconciliationRequired}
		}
		if source.hash != artifact.hash {
			return true, &Error{kind: ReconciliationRequired}
		}
	}
	for _, name := range []string{r.staged(), r.displaced(), w.intent} {
		if err := w.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return true, &Error{kind: ReconciliationRequired}
		}
	}
	if err := w.dir.Sync(); err != nil {
		return true, &Error{kind: ReconciliationRequired}
	}
	return true, nil
}
