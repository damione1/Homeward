package installation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/storage"
)

func privateName(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", failure(Unavailable)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

type boundedDump struct {
	file    *os.File
	written int64
}

func (b *boundedDump) Write(p []byte) (int, error) {
	if int64(len(p)) > backups.MaxArtifactBytes-b.written {
		return 0, failure(Invalid)
	}
	n, err := b.file.Write(p)
	b.written += int64(n)
	return n, err
}

// Create returns the same Sealed155 retry object used by providers154. The
// caller keeps it across an ambiguous publish and closes it explicitly.
func (s *Service) Create(ctx context.Context, tools storage.BackupPGTools, id backups.ArtifactID) (*backuparchive.Sealed, error) {
	if !contextValid(ctx) || s == nil || s.store == nil || !tools.Valid() || !id.Valid() {
		return nil, failure(Invalid)
	}
	snapshot, err := s.store.BeginBackupSnapshot(ctx, s.secrets)
	if err != nil {
		return nil, failure(Integrity)
	}
	return s.CreateFromSnapshot(ctx, snapshot, tools, id)
}

// CreateFromSnapshot consumes an admitted snapshot owned by this exact Store.
// It closes that transaction on every path; callers may Close idempotently.
func (s *Service) CreateFromSnapshot(ctx context.Context, snapshot *storage.BackupSnapshot, tools storage.BackupPGTools, id backups.ArtifactID) (*backuparchive.Sealed, error) {
	if !contextValid(ctx) || s == nil || s.store == nil || snapshot == nil || !tools.Valid() || !id.Valid() {
		if snapshot != nil {
			_ = snapshot.Close()
		}
		return nil, failure(Invalid)
	}
	defer snapshot.Close()
	root, err := storage.OpenPrivateDataDir(s.directory)
	if err != nil {
		return nil, failure(Unavailable)
	}
	defer root.Close()
	name, err := privateName(".snapshot-")
	if err != nil {
		return nil, err
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, failure(Unavailable)
	}
	defer file.Close()
	defer root.Remove(name)
	writer := &boundedDump{file: file}
	if err = s.store.DumpBackupSnapshot(ctx, snapshot, tools, writer); err != nil {
		_ = snapshot.Close()
		return nil, failure(Unavailable)
	}
	m, err := newManifest(snapshot)
	if err != nil {
		_ = snapshot.Close()
		return nil, err
	}
	sources := snapshot.Sources()
	if err = snapshot.Close(); err != nil {
		return nil, failure(Unavailable)
	}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > maxManifest {
		return nil, failure(Invalid)
	}
	yaml := make([]backuparchive.YAMLSource, 0, len(sources)+1)
	item, err := backuparchive.NewYAMLSource(manifestPath, int64(len(raw)), bytes.NewReader(raw))
	if err != nil {
		return nil, failure(Invalid)
	}
	yaml = append(yaml, item)
	for index, source := range sources {
		raw := source.Bytes()
		item, err := backuparchive.NewYAMLSource(sourceName(index), int64(len(raw)), bytes.NewReader(raw))
		if err != nil {
			return nil, failure(Invalid)
		}
		yaml = append(yaml, item)
	}
	if writer.written == 0 {
		return nil, failure(Integrity)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, failure(Unavailable)
	}
	input, err := backuparchive.NewInput(file, writer.written, yaml, s.key)
	if err != nil {
		return nil, failure(Invalid)
	}
	return backuparchive.Seal(ctx, s.directory, id, s.public, input, backuparchive.DefaultLimits())
}
