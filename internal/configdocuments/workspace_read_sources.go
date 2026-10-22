package configdocuments

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/damione1/Homeward/internal/storage"
	"golang.org/x/sys/unix"
)

// SourceObservation distinguishes an absent member from a present empty or
// invalid candidate. Neither absence nor any path is an SQL authority.
type SourceObservation struct {
	binding     storage.ConfigDocumentBinding
	observation Observation
	raw         []byte
	absent      bool
}

func (s SourceObservation) Binding() storage.ConfigDocumentBinding { return s.binding }
func (s SourceObservation) Observation() Observation               { return s.observation }
func (s SourceObservation) Present() bool                          { return !s.absent && s.observation.present }
func (s SourceObservation) Bytes() []byte                          { return bytes.Clone(s.raw) }
func (SourceObservation) String() string                           { return "[REDACTED SOURCE OBSERVATION]" }
func (s SourceObservation) GoString() string                       { return s.String() }
func (s SourceObservation) MarshalJSON() ([]byte, error)           { return json.Marshal(s.String()) }

// SourceWorkspaceGroup binds the closed source kind as well as its open
// identity. An open dashboard ID cannot occupy a script/scene/ownership root.
// Entry paths are excluded so editing an object's entry preserves its root.
func SourceWorkspaceGroup(descriptor storage.SourceDescriptor) (storage.ConfigDocumentID, error) {
	if !descriptor.Valid() {
		return storage.ConfigDocumentID{}, &Error{kind: FailureInvalid}
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(descriptor.Kind().String()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(descriptor.Identity()))
	return storage.NewConfigDocumentID("source." + hex.EncodeToString(hash.Sum(nil)))
}

// These group namespaces are server-owned physical locators. Declarative
// client group IDs cannot occupy them, whether or not a member already exists.
func reservedSourceWorkspaceGroup(group storage.ConfigDocumentID) bool {
	return strings.HasPrefix(group.String(), "source.") || strings.HasPrefix(group.String(), "authorization:")
}

func sourceMemberLocation(group storage.ConfigDocumentID, binding storage.ConfigDocumentBinding) (string, error) {
	if !group.Valid() || !binding.ID().Valid() || !fs.ValidPath(binding.Path()) || strings.Contains(binding.Path(), "\\") {
		return "", &Error{kind: FailureInvalid}
	}
	digest := sha256.Sum256([]byte(group.String()))
	return path.Join("configuration.d", hex.EncodeToString(digest[:]), binding.Path()), nil
}

// openSourceMember walks every component below the already pinned149 root.
// Each directory and leaf rejects symlinks at openat itself, closing the
// checkParents/open race. No client path, filesystem scan or os.Root symlink
// following can redirect an admitted member outside its configured group.
func (w *Workspace) openSourceMember(location string) (*os.File, error) {
	if !fs.ValidPath(location) || !strings.HasPrefix(location, "configuration.d/") || strings.Contains(location, "\\") {
		return nil, &Error{kind: FailureInvalid}
	}
	fd, err := unix.Openat(int(w.dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	parts := strings.Split(location, "/")
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if errors.Is(openErr, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		if openErr != nil {
			return nil, &Error{kind: Unavailable}
		}
		fd = next
	}
	leaf, err := unix.Openat(fd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	_ = unix.Close(fd)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	return os.NewFile(uintptr(leaf), location), nil
}

// ReadSources captures a bounded complete member set under Workspace.mu, then
// repeats path admission for every descriptor before returning. Missing members
// remain explicit observations; they never become an implicit delete or grant.
func (w *Workspace) ReadSources(group storage.ConfigDocumentID, bindings []storage.ConfigDocumentBinding) ([]SourceObservation, error) {
	return w.readSources(group, bindings, 4<<20)
}

func (w *Workspace) readSources(group storage.ConfigDocumentID, bindings []storage.ConfigDocumentBinding, byteBudget int) ([]SourceObservation, error) {
	if w == nil || !w.admitted || !group.Valid() || len(bindings) < 1 || len(bindings) > 64 || byteBudget < 0 || byteBudget > 4<<20 {
		return nil, &Error{kind: FailureInvalid}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, &Error{kind: Unavailable}
	}
	_, intentErr := w.root.Lstat(w.intent)
	if intentErr != nil && !errors.Is(intentErr, os.ErrNotExist) {
		return nil, &Error{kind: Unavailable}
	}
	pending := intentErr == nil
	seenIDs, seenPaths := map[storage.ConfigDocumentID]bool{}, map[string]bool{}
	result := make([]SourceObservation, 0, len(bindings))
	total := 0
	for _, binding := range bindings {
		location, err := sourceMemberLocation(group, binding)
		if err != nil || seenIDs[binding.ID()] || seenPaths[binding.Path()] {
			return nil, &Error{kind: FailureInvalid}
		}
		seenIDs[binding.ID()], seenPaths[binding.Path()] = true, true
		observed := SourceObservation{binding: binding, absent: true, observation: Observation{pending: pending}}
		file, err := w.openSourceMember(location)
		if errors.Is(err, os.ErrNotExist) {
			result = append(result, observed)
			continue
		}
		if err != nil {
			return nil, err
		}
		before, statErr := file.Stat()
		if statErr != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > 1<<20 || before.Size() > int64(byteBudget-total) {
			file.Close()
			return nil, &Error{kind: Unavailable}
		}
		limit := byteBudget - total
		if limit > 1<<20 {
			limit = 1 << 20
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, int64(limit)+1))
		after, afterErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || afterErr != nil || closeErr != nil || len(raw) > limit || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return nil, &Error{kind: Conflict}
		}
		total += len(raw)
		if total > byteBudget {
			return nil, &Error{kind: Unavailable}
		}
		observed.absent = false
		observed.raw = raw
		observed.observation = Observation{hash: sha256.Sum256(raw), updated: after.ModTime().UTC(), present: true, pending: pending}
		result = append(result, observed)
	}
	// Reopen from the pinned root after every read. A renamed directory/member
	// or disappeared/reappeared include invalidates the whole group observation.
	for _, observed := range result {
		location, _ := sourceMemberLocation(group, observed.binding)
		file, err := w.openSourceMember(location)
		if observed.absent {
			if file != nil {
				file.Close()
			}
			if !errors.Is(err, os.ErrNotExist) {
				return nil, &Error{kind: Conflict}
			}
			continue
		}
		if err != nil {
			return nil, &Error{kind: Conflict}
		}
		info, statErr := file.Stat()
		raw, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		closeErr := file.Close()
		if statErr != nil || readErr != nil || closeErr != nil || !info.Mode().IsRegular() || len(raw) > 1<<20 || sha256.Sum256(raw) != observed.observation.hash || !info.ModTime().UTC().Equal(observed.observation.updated) {
			return nil, &Error{kind: Conflict}
		}
	}
	return result, nil
}
