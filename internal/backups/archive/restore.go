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
	"sort"
	"strings"
	"syscall"

	"filippo.io/age"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/storage"
)

type RestoreState uint8

const (
	RestoreNotPublished RestoreState = iota + 1
	Restored
	RestoreIndeterminate
)

type RestoreResult struct{ state RestoreState }

func (r RestoreResult) State() RestoreState { return r.state }

// Private, per-operation ports permit deterministic tests after real effects.
// Production calls always use the native exclusive rename and directory sync.
type restoreIO struct {
	rename func(*os.Root, string, string) error
	sync   func(*os.Root, string) error
}

func productionRestoreIO() restoreIO { return restoreIO{rename: renameExclusive, sync: syncRoot} }

// RecoverProvider freezes and checks the complete provider stream before any
// plaintext extraction. Closing a provider reader early is never verification.
func RecoverProvider(ctx context.Context, provider backups.Provider, id backups.ArtifactID, identity RecoveryPrivate, parent, destination string, limits Limits) (RestoreResult, error) {
	if backups.IsNil(provider) || !id.Valid() {
		return RestoreResult{RestoreNotPublished}, fail(Invalid)
	}
	reader, err := provider.Open(ctx, id)
	if err != nil {
		return RestoreResult{RestoreNotPublished}, fail(Unavailable)
	}
	if backups.IsNil(reader) {
		return RestoreResult{RestoreNotPublished}, fail(Invalid)
	}
	metadata := reader.Metadata()
	if !metadata.Valid() || metadata.ID() != id {
		_ = reader.Close()
		return RestoreResult{RestoreNotPublished}, fail(Integrity)
	}
	return recoverArchive(ctx, reader, reader.Close, &metadata, id, identity, parent, destination, limits, productionRestoreIO())
}

// Recover reads an exported raw age payload. It owns neither source nor a
// database, startup configuration, Host or integration. The destination is new.
func Recover(ctx context.Context, source io.Reader, id backups.ArtifactID, identity RecoveryPrivate, parent, destination string, limits Limits) (RestoreResult, error) {
	return recoverArchive(ctx, source, nil, nil, id, identity, parent, destination, limits, productionRestoreIO())
}

func recoverArchive(ctx context.Context, source io.Reader, closeSource func() error, metadata *backups.Metadata, id backups.ArtifactID, identity RecoveryPrivate, parent, destination string, limits Limits, operations restoreIO) (RestoreResult, error) {
	failed := RestoreResult{RestoreNotPublished}
	closed := false
	defer func() {
		if closeSource != nil && !closed {
			_ = closeSource()
		}
	}()
	if err := contextError(ctx); err != nil {
		return failed, err
	}
	if backups.IsNil(source) || !id.Valid() || !identity.Valid() || !limits.Valid() || !validDestination(destination) || operations.rename == nil || operations.sync == nil {
		return failed, fail(Invalid)
	}
	root, err := privateRoot(parent)
	if err != nil {
		return failed, err
	}
	defer root.Close()
	if _, err := root.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return failed, fail(Conflict)
		}
		return failed, fail(Unavailable)
	}
	ciphertext, pendingCipher, err := newPrivateFile(root, ".recovery-cipher-")
	if err != nil {
		return failed, err
	}
	defer ciphertext.Close()
	defer root.Remove(pendingCipher)
	digest := sha256.New()
	budget := &budgetWriter{target: io.MultiWriter(ciphertext, digest), remaining: limits.ciphertext}
	n, copyErr := io.CopyBuffer(budget, contextBound(ctx, source), make([]byte, 32*1024))
	var closeErr error
	if closeSource != nil {
		closeErr = closeSource()
		closed = true
	}
	if copyErr != nil {
		return failed, ioError(copyErr)
	}
	if closeErr != nil {
		return failed, fail(Unavailable)
	}
	if n == 0 {
		return failed, fail(Integrity)
	}
	if metadata != nil {
		var actual [sha256.Size]byte
		copy(actual[:], digest.Sum(nil))
		if metadata.Size() != n || metadata.SHA256() != actual {
			return failed, fail(Integrity)
		}
	}
	// Remove the only pathname to the frozen ciphertext before either pass.
	// This operation keeps its own descriptor, never an external mutable reader.
	if err := root.Remove(pendingCipher); err != nil {
		return failed, fail(Unavailable)
	}
	m, err := inspect(ctx, ciphertext, id, identity, limits, nil, nil)
	if err != nil {
		return failed, err
	}
	stage, err := token(".recovery-stage-")
	if err != nil {
		return failed, err
	}
	if err := root.Mkdir(stage, 0700); err != nil {
		return failed, fail(Unavailable)
	}
	defer root.RemoveAll(stage)
	output, err := root.OpenRoot(stage)
	if err != nil {
		return failed, fail(Unavailable)
	}
	_, extractErr := inspect(ctx, ciphertext, id, identity, limits, output, &m)
	if extractErr == nil {
		extractErr = syncOutputDirectories(output, m)
	}
	outputCloseErr := output.Close()
	if extractErr != nil {
		return failed, extractErr
	}
	if outputCloseErr != nil {
		return failed, fail(Unavailable)
	}
	if err := contextError(ctx); err != nil {
		return failed, err
	}
	if err := operations.rename(root, stage, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return failed, fail(Conflict)
		}
		// The syscall may have succeeded before an IO error was reported.
		return RestoreResult{RestoreIndeterminate}, fail(Unavailable)
	}
	if err := operations.sync(root, "."); err != nil {
		return RestoreResult{RestoreIndeterminate}, fail(Unavailable)
	}
	return RestoreResult{Restored}, nil
}

func validDestination(name string) bool {
	if len(name) < 1 || len(name) > 80 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func inspect(ctx context.Context, ciphertext *os.File, id backups.ArtifactID, identity RecoveryPrivate, limits Limits, output *os.Root, expected *manifest) (manifest, error) {
	if _, err := ciphertext.Seek(0, io.SeekStart); err != nil {
		return manifest{}, fail(Unavailable)
	}
	plain, err := age.Decrypt(contextBound(ctx, ciphertext), identity.identity)
	if err != nil {
		return manifest{}, fail(Integrity)
	}
	checked := &footerReader{source: contextBound(ctx, plain)}
	tr := tar.NewReader(checked)
	actual := make([]entryManifest, 0)
	var result manifest
	manifestSeen := false
	var total int64
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return manifest{}, fail(Integrity)
		}
		if manifestSeen || header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.Linkname != "" || len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 || header.Mode != 0600 || header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" || header.Size < 0 {
			return manifest{}, fail(Integrity)
		}
		if header.Name == manifestName {
			if header.Size > MaxManifestBytes {
				return manifest{}, fail(Capacity)
			}
			raw, readErr := io.ReadAll(io.LimitReader(tr, MaxManifestBytes+1))
			if readErr != nil || int64(len(raw)) != header.Size {
				return manifest{}, fail(Integrity)
			}
			result, err = admitManifest(raw, id, actual)
			if err != nil {
				return manifest{}, err
			}
			if expected != nil {
				prior, _ := json.Marshal(expected)
				if !bytes.Equal(prior, raw) {
					return manifest{}, fail(Integrity)
				}
			}
			manifestSeen = true
			continue
		}
		index := len(actual)
		if index >= limits.entries || header.Size > limits.plaintext-total {
			return manifest{}, fail(Capacity)
		}
		name := "database.dump"
		if index == 1 {
			name = ".env"
		} else if index > 1 {
			name = fmt.Sprintf("yaml/%06d", index-2)
		}
		if header.Name != name || index == 0 && header.Size == 0 || index == 1 && header.Size > 256 {
			return manifest{}, fail(Integrity)
		}
		total += header.Size
		digest := sha256.New()
		var key bytes.Buffer
		var dst io.Writer = digest
		if index == 1 {
			dst = io.MultiWriter(dst, &key)
		}
		var file *os.File
		if output != nil {
			if expected == nil || index >= len(expected.Entries) {
				return manifest{}, fail(Integrity)
			}
			file, err = createOutput(output, expected.Entries[index].Path)
			if err != nil {
				return manifest{}, err
			}
			dst = io.MultiWriter(dst, file)
		}
		n, readErr := io.CopyBuffer(dst, contextBound(ctx, tr), make([]byte, 32*1024))
		if file != nil {
			syncErr, closeErr := file.Sync(), file.Close()
			if readErr == nil && (syncErr != nil || closeErr != nil) {
				readErr = fail(Unavailable)
			}
		}
		if readErr != nil || n != header.Size {
			clear(key.Bytes())
			return manifest{}, fail(Integrity)
		}
		if index == 1 {
			_, keyErr := storage.ParseBackupServerKey(key.Bytes())
			clear(key.Bytes())
			if keyErr != nil {
				return manifest{}, fail(Integrity)
			}
		}
		actual = append(actual, entryManifest{Name: name, Size: n, SHA256: hex.EncodeToString(digest.Sum(nil))})
	}
	if !manifestSeen {
		return manifest{}, fail(Integrity)
	}
	// tar EOF alone may leave unread age chunks, including an invalid last tag.
	// Canonical USTAR ends here: reject additional plaintext and demand age EOF.
	var extra [1]byte
	if n, err := checked.Read(extra[:]); n != 0 || err != io.EOF {
		return manifest{}, fail(Integrity)
	}
	if checked.total%512 != 0 || checked.used != len(checked.tail) || checked.tail != [1024]byte{} {
		return manifest{}, fail(Integrity)
	}
	return result, nil
}

// archive/tar accepts an EOF without the canonical two zero blocks. Retain only
// this fixed-size tail so a valid AEAD message cannot hide a truncated tar.
type footerReader struct {
	source io.Reader
	total  int64
	tail   [1024]byte
	used   int
}

func (r *footerReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n > 0 {
		r.total += int64(n)
		if n >= len(r.tail) {
			copy(r.tail[:], p[n-len(r.tail):n])
			r.used = len(r.tail)
		} else {
			if r.used+n > len(r.tail) {
				shift := r.used + n - len(r.tail)
				copy(r.tail[:], r.tail[shift:r.used])
				r.used -= shift
			}
			copy(r.tail[r.used:], p[:n])
			r.used += n
		}
	}
	return n, err
}

func admitManifest(raw []byte, id backups.ArtifactID, actual []entryManifest) (manifest, error) {
	var m manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return manifest{}, fail(Integrity)
	}
	canonical, err := json.Marshal(m)
	if err != nil || !bytes.Equal(canonical, raw) {
		return manifest{}, fail(Integrity)
	}
	if m.Version != formatVersion {
		return manifest{}, fail(Unsupported)
	}
	if m.Artifact != id.String() || len(m.Entries) != len(actual) || len(actual) < 3 {
		return manifest{}, fail(Integrity)
	}
	seen := make(map[string]bool, len(actual))
	for i, entry := range m.Entries {
		if entry.Name != actual[i].Name || entry.Size != actual[i].Size || entry.SHA256 != actual[i].SHA256 || pathCollision(seen, entry.Path) {
			return manifest{}, fail(Integrity)
		}
		switch i {
		case 0:
			if entry.Path != "database.dump" {
				return manifest{}, fail(Integrity)
			}
		case 1:
			if entry.Path != ".env" {
				return manifest{}, fail(Integrity)
			}
		default:
			if !strings.HasPrefix(entry.Path, "yaml/") || !validLogicalPath(strings.TrimPrefix(entry.Path, "yaml/")) {
				return manifest{}, fail(Integrity)
			}
		}
		seen[entry.Path] = true
	}
	return m, nil
}

func createOutput(root *os.Root, name string) (*os.File, error) {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		directory := strings.Join(parts[:i], "/")
		if err := root.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fail(Unavailable)
		}
		info, err := root.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return nil, fail(Integrity)
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fail(Unavailable)
	}
	return f, nil
}

func syncOutputDirectories(root *os.Root, m manifest) error {
	seen := map[string]bool{".": true}
	for _, entry := range m.Entries {
		parts := strings.Split(entry.Path, "/")
		for i := 1; i < len(parts); i++ {
			seen[strings.Join(parts[:i], "/")] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for name := range seen {
		paths = append(paths, name)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, name := range paths {
		if err := syncRoot(root, name); err != nil {
			return err
		}
	}
	return nil
}
