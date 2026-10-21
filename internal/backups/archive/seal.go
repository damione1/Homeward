package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/damione1/Homeward/internal/backups"
)

const formatVersion = 1
const manifestName = "manifest.json"

type entryManifest struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type manifest struct {
	Version  uint8           `json:"version"`
	Artifact string          `json:"artifact"`
	Entries  []entryManifest `json:"entries"`
}

// Sealed owns only ciphertext. Keep this object until an ambiguous publication
// is resolved, so retrying the same ArtifactID reuses exactly the same bytes.
// Close is explicit: a failed Publish never destroys the caller's retry image.
type Sealed struct {
	mu     sync.Mutex
	root   *os.Root
	file   *os.File
	name   string
	id     backups.ArtifactID
	size   int64
	digest [sha256.Size]byte
	closed bool
}

func (*Sealed) String() string                 { return "backup.Sealed(<ciphertext>)" }
func (s *Sealed) GoString() string             { return s.String() }
func (s *Sealed) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, s.String()) }
func (s *Sealed) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }
func (s *Sealed) ID() backups.ArtifactID {
	if s == nil {
		return backups.ArtifactID{}
	}
	return s.id
}
func (s *Sealed) Size() int64 {
	if s == nil {
		return 0
	}
	return s.size
}
func (s *Sealed) SHA256() [sha256.Size]byte {
	if s == nil {
		return [sha256.Size]byte{}
	}
	return s.digest
}

func Seal(ctx context.Context, directory string, id backups.ArtifactID, public RecoveryPublic, input Input, limits Limits) (*Sealed, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if !id.Valid() || !public.Valid() || !input.valid || !limits.Valid() || len(input.yaml)+2 > limits.entries {
		return nil, fail(Invalid)
	}
	var plainBytes int64
	for _, n := range append([]int64{input.databaseSize, input.key.EncodedSize()}, sourceSizes(input.yaml)...) {
		if n < 0 || n > limits.plaintext-plainBytes {
			return nil, fail(Capacity)
		}
		plainBytes += n
	}
	root, err := privateRoot(directory)
	if err != nil {
		return nil, err
	}
	f, name, err := newPrivateFile(root, ".sealed-")
	if err != nil {
		root.Close()
		return nil, err
	}
	image := &Sealed{root: root, file: f, name: name, id: id}
	ok := false
	defer func() {
		if !ok {
			_ = image.Close()
		}
	}()
	digest := sha256.New()
	bounded := &budgetWriter{target: io.MultiWriter(f, digest), remaining: limits.ciphertext}
	encrypted, err := age.Encrypt(bounded, public.recipient)
	if err != nil {
		return nil, ioError(err)
	}
	tw := tar.NewWriter(encrypted)
	m := manifest{Version: formatVersion, Artifact: id.String()}
	entry, err := writeEntry(ctx, tw, "database.dump", "database.dump", input.databaseSize, input.database)
	if err != nil {
		return nil, err
	}
	m.Entries = append(m.Entries, entry)
	var key bytes.Buffer
	if _, err := input.key.WriteTo(&key); err != nil {
		return nil, fail(Invalid)
	}
	defer clear(key.Bytes())
	entry, err = writeEntry(ctx, tw, ".env", ".env", input.key.EncodedSize(), bytes.NewReader(key.Bytes()))
	if err != nil {
		return nil, err
	}
	m.Entries = append(m.Entries, entry)
	for i, source := range input.yaml {
		name := fmt.Sprintf("yaml/%06d", i)
		entry, err = writeEntry(ctx, tw, name, "yaml/"+source.logical, source.size, source.reader)
		if err != nil {
			return nil, err
		}
		m.Entries = append(m.Entries, entry)
	}
	raw, err := json.Marshal(m)
	if err != nil || int64(len(raw)) > MaxManifestBytes {
		return nil, fail(Capacity)
	}
	if _, err := writeEntry(ctx, tw, manifestName, manifestName, int64(len(raw)), bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, ioError(err)
	}
	if err := encrypted.Close(); err != nil {
		return nil, ioError(err)
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, fail(Unavailable)
	}
	position, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, fail(Unavailable)
	}
	image.size = position
	copy(image.digest[:], digest.Sum(nil))
	if err := syncRoot(root, "."); err != nil {
		return nil, err
	}
	ok = true
	return image, nil
}
func sourceSizes(sources []YAMLSource) []int64 {
	n := make([]int64, len(sources))
	for i := range sources {
		n[i] = sources[i].size
	}
	return n
}
func writeEntry(ctx context.Context, tw *tar.Writer, name, output string, size int64, source io.Reader) (entryManifest, error) {
	h := &tar.Header{Name: name, Mode: 0600, Size: size, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}
	if err := tw.WriteHeader(h); err != nil {
		return entryManifest{}, ioError(err)
	}
	digest := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(tw, digest), io.LimitReader(contextBound(ctx, source), size+1), make([]byte, 32*1024))
	if err != nil {
		return entryManifest{}, ioError(err)
	}
	if n != size {
		return entryManifest{}, fail(Integrity)
	}
	return entryManifest{Name: name, Path: output, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}
func (s *Sealed) WriteTo(dst io.Writer) (int64, error) {
	if s == nil || backups.IsNil(dst) {
		return 0, fail(Invalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.file == nil || s.root == nil || !s.id.Valid() || s.size < 1 {
		return 0, fail(Invalid)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return 0, fail(Unavailable)
	}
	n, err := io.Copy(dst, io.LimitReader(s.file, s.size))
	if err != nil || n != s.size {
		return n, fail(Unavailable)
	}
	return n, nil
}
func (s *Sealed) Publish(ctx context.Context, provider backups.Provider) (backups.WriteResult, error) {
	if s == nil || backups.IsNil(provider) {
		return backups.WriteResult{}, fail(Invalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return backups.WriteResult{}, err
	}
	if s.closed || s.file == nil || s.root == nil || !s.id.Valid() || s.size < 1 || !provider.Capabilities().Valid() || s.size > provider.Capabilities().Limits().ArtifactBytes() {
		return backups.WriteResult{}, fail(Invalid)
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return backups.WriteResult{}, fail(Unavailable)
	}
	intent, err := backups.NewWriteIntent(s.id, s.size)
	if err != nil {
		return backups.WriteResult{}, fail(Invalid)
	}
	result, writeErr := provider.Write(ctx, intent, io.LimitReader(contextBound(ctx, s.file), s.size))
	uncertain, _ := backups.NewWriteResult(backups.Indeterminate, s.id, backups.Metadata{})
	if result.ID() != s.id {
		return uncertain, fail(Unavailable)
	}
	switch result.State() {
	case backups.Published, backups.AlreadyPresent:
		metadata, present := result.Metadata()
		if writeErr != nil || !present || metadata.Size() != s.size || metadata.SHA256() != s.digest {
			return uncertain, fail(Integrity)
		}
		return result, nil
	case backups.NotPublished, backups.Indeterminate:
		if writeErr == nil {
			return result, fail(Unavailable)
		}
		var admitted *backups.Error
		if errors.As(writeErr, &admitted) {
			return result, admitted
		}
		return result, fail(Unavailable)
	default:
		return uncertain, fail(Unavailable)
	}
}
func (s *Sealed) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.file == nil || s.root == nil {
		if s.file != nil {
			_ = s.file.Close()
		}
		if s.root != nil {
			_ = s.root.Close()
		}
		return fail(Invalid)
	}
	closeErr := s.file.Close()
	removeErr := s.root.Remove(s.name)
	rootErr := s.root.Close()
	if closeErr != nil || removeErr != nil || rootErr != nil {
		return fail(Unavailable)
	}
	return nil
}
