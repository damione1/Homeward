// Package local is the filesystem adapter for the opaque backups port.
package local

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/storage"
)

// A single versioned envelope avoids a second catalog/sidecar commit. The
// storage header is not the authenticated archive format owned by NHA-155.
const headerBytes = 96
const fileSuffix = ".hwb"

var envelopeMagic = [8]byte{'H', 'W', 'B', 'K', 0, 1, '\r', '\n'}

type Factory struct{}

func (Factory) Kind() backups.ProviderKind { return backups.Local }
func (Factory) Open(ctx context.Context, settings backups.InstanceSettings) (backups.ManagedProvider, error) {
	return open(ctx, settings, productionIO())
}

// Private immutable IO seams let tests inject ENOSPC, short writes and fsync
// failures without consuming the host disk or weakening the production port.
type ioOperations struct {
	write     func(*os.File, []byte) (int, error)
	writeAt   func(*os.File, []byte, int64) (int, error)
	fileSync  func(*os.File) error
	fileClose func(*os.File) error
	link      func(*os.Root, string, string) error
	remove    func(*os.Root, string) error
	dirSync   func(*os.File) error
}

func productionIO() ioOperations {
	return ioOperations{
		write: (*os.File).Write, writeAt: (*os.File).WriteAt,
		fileSync: (*os.File).Sync, fileClose: (*os.File).Close,
		link: (*os.Root).Link, remove: (*os.Root).Remove, dirSync: (*os.File).Sync,
	}
}

type provider struct {
	root     *os.Root
	id       backups.InstanceID
	limits   backups.Limits
	io       ioOperations
	slots    chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	files    map[*os.File]struct{}
	mutation sync.Mutex
}

func (*provider) Format(out fmt.State, _ rune) { _, _ = out.Write([]byte("backup.local(<private>)")) }
func (*provider) String() string               { return "backup.local(<private>)" }
func (p *provider) GoString() string           { return p.String() }

func open(ctx context.Context, settings backups.InstanceSettings, operations ioOperations) (*provider, error) {
	if backups.IsNil(ctx) || !settings.Valid() || !settings.Enabled() {
		return nil, backups.NewError(backups.InvalidInput)
	}
	if ctx.Err() != nil {
		return nil, backups.NewError(backups.Cancelled)
	}
	local, err := settings.Local()
	if err != nil {
		return nil, err
	}
	base, err := storage.OpenPrivateDataDir(local.Root())
	if err != nil {
		return nil, backups.NewError(backups.PermissionDenied)
	}
	defer base.Close()
	if err := base.Mkdir(settings.ID().String(), 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, classify(err)
	}
	info, err := base.Lstat(settings.ID().String())
	if err != nil {
		return nil, classify(err)
	}
	if !privateDirectory(info) {
		return nil, backups.NewError(backups.PermissionDenied)
	}
	root, err := base.OpenRoot(settings.ID().String())
	if err != nil {
		return nil, classify(err)
	}
	dir, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		root.Close()
		return nil, classify(err)
	}
	opened, statErr := dir.Stat()
	if statErr != nil || !privateDirectory(opened) || !os.SameFile(info, opened) {
		dir.Close()
		root.Close()
		return nil, backups.NewError(backups.PermissionDenied)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		root.Close()
		return nil, classify(err)
	}
	if err := dir.Close(); err != nil {
		root.Close()
		return nil, classify(err)
	}
	baseDir, err := base.Open(".")
	if err != nil {
		root.Close()
		return nil, classify(err)
	}
	syncErr, closeErr := baseDir.Sync(), baseDir.Close()
	if syncErr != nil {
		root.Close()
		return nil, classify(syncErr)
	}
	if closeErr != nil {
		root.Close()
		return nil, classify(closeErr)
	}
	if ctx.Err() != nil {
		root.Close()
		return nil, backups.NewError(backups.Cancelled)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &provider{root: root, id: settings.ID(), limits: local.Limits(), io: operations,
		slots: make(chan struct{}, local.Limits().ConcurrentOperations()), ctx: lifetime, cancel: cancel, files: make(map[*os.File]struct{})}, nil
}

func privateOwner(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
func privateDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode().Perm() == 0700 && privateOwner(info)
}
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return backups.NewError(backups.Cancelled)
	case errors.Is(err, os.ErrClosed):
		return backups.NewError(backups.Closed)
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return backups.NewError(backups.PermissionDenied)
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT), errors.Is(err, syscall.EFBIG):
		return backups.NewError(backups.Capacity)
	case errors.Is(err, os.ErrNotExist):
		return backups.NewError(backups.NotFound)
	case errors.Is(err, os.ErrExist):
		return backups.NewError(backups.Conflict)
	default:
		return backups.NewError(backups.Unavailable)
	}
}

func (p *provider) Capabilities() backups.Capabilities {
	capabilities, _ := backups.NewCapabilities(p.limits)
	return capabilities
}

// Close cancels admissions and closes every owned descriptor, including lent
// readers. It never waits on arbitrary producer Reader code. A producer must
// itself honor its context to stop a blocked Read; no detached worker is made.
func (p *provider) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.cancel()
	var result error
	for file := range p.files {
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			result = classify(err)
		}
		delete(p.files, file)
	}
	if err := p.root.Close(); err != nil {
		result = classify(err)
	}
	return result
}

func (p *provider) track(file *os.File) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		file.Close()
		return backups.NewError(backups.Closed)
	}
	p.files[file] = struct{}{}
	return nil
}
func (p *provider) closeFile(file *os.File) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, owned := p.files[file]; !owned {
		return nil
	}
	delete(p.files, file)
	err := p.io.fileClose(file)
	if err != nil {
		_ = file.Close()
	}
	return err
}
func (p *provider) begin(ctx context.Context) (context.Context, func(), error) {
	if backups.IsNil(ctx) {
		return nil, nil, backups.NewError(backups.InvalidInput)
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, nil, backups.NewError(backups.Closed)
	}
	if ctx.Err() != nil {
		return nil, nil, backups.NewError(backups.Cancelled)
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return nil, nil, backups.NewError(backups.Capacity)
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	release := func() { stop(); cancel(); <-p.slots }
	if p.ctx.Err() != nil {
		release()
		return nil, nil, backups.NewError(backups.Closed)
	}
	if err := p.checkDirectory(); err != nil {
		release()
		return nil, nil, err
	}
	return operation, release, nil
}
func (p *provider) checkDirectory() error {
	dir, err := p.root.OpenFile(".", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return classify(err)
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return classify(err)
	}
	if !privateDirectory(info) {
		return backups.NewError(backups.PermissionDenied)
	}
	return nil
}
func (p *provider) syncDirectory() error {
	dir, err := p.root.OpenFile(".", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return classify(err)
	}
	if err := p.track(dir); err != nil {
		return err
	}
	err = p.io.dirSync(dir)
	closeErr := p.closeFile(dir)
	if err != nil {
		return classify(err)
	}
	return classify(closeErr)
}
func operationError(ctx context.Context) error {
	if ctx.Err() != nil {
		return backups.NewError(backups.Cancelled)
	}
	return nil
}
func filename(id backups.ArtifactID) string { return id.String() + fileSuffix }
func writeResult(state backups.PublicationState, id backups.ArtifactID, metadata backups.Metadata) backups.WriteResult {
	result, _ := backups.NewWriteResult(state, id, metadata)
	return result
}

func (p *provider) Write(ctx context.Context, intent backups.WriteIntent, source io.Reader) (backups.WriteResult, error) {
	if !intent.Valid() || backups.IsNil(source) || intent.MaxBytes() > p.limits.ArtifactBytes() {
		return backups.WriteResult{}, backups.NewError(backups.InvalidInput)
	}
	failed := writeResult(backups.NotPublished, intent.ID(), backups.Metadata{})
	operation, release, err := p.begin(ctx)
	if err != nil {
		return failed, err
	}
	defer release()
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return failed, backups.NewError(backups.Unavailable)
	}
	pending := ".pending-" + hex.EncodeToString(token[:])
	file, err := p.root.OpenFile(pending, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return failed, classify(err)
	}
	if err := p.track(file); err != nil {
		return failed, err
	}
	// Only this writer's random pending entry is eligible for cleanup. Failed
	// cleanup is harmless to visibility; startup does not collect other writers.
	defer p.root.Remove(pending)
	defer p.closeFile(file)
	if err := file.Chmod(0600); err != nil {
		return failed, classify(err)
	}
	if err := p.writeChunk(file, make([]byte, headerBytes)); err != nil {
		return failed, err
	}
	digest := sha256.New()
	var size int64
	buffer := make([]byte, 32*1024)
	emptyReads := 0
	for {
		if err := operationError(operation); err != nil {
			return failed, err
		}
		window := int64(len(buffer))
		if remaining := intent.MaxBytes() - size + 1; remaining < window {
			window = remaining
		}
		n, readErr := source.Read(buffer[:int(window)])
		if n < 0 || n > int(window) {
			return failed, backups.NewError(backups.InvalidInput)
		}
		if int64(n) > intent.MaxBytes()-size {
			return failed, backups.NewError(backups.Capacity)
		}
		if n > 0 {
			emptyReads = 0
			if err := operationError(operation); err != nil {
				return failed, err
			}
			if err := p.writeChunk(file, buffer[:n]); err != nil {
				return failed, err
			}
			_, _ = digest.Write(buffer[:n])
			size += int64(n)
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return failed, backups.NewError(backups.Unavailable)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if operation.Err() != nil {
				return failed, backups.NewError(backups.Cancelled)
			}
			return failed, backups.NewError(backups.Unavailable)
		}
	}
	var sum [sha256.Size]byte
	copy(sum[:], digest.Sum(nil))
	metadata, err := backups.NewMetadata(intent.ID(), size, sum, time.Now().UTC())
	if err != nil {
		return failed, err
	}
	header := encodeHeader(metadata)
	n, err := p.io.writeAt(file, header[:], 0)
	if err != nil {
		return failed, classify(err)
	}
	if n != headerBytes {
		return failed, backups.NewError(backups.Unavailable)
	}
	if err := p.io.fileSync(file); err != nil {
		return failed, classify(err)
	}
	if err := p.closeFile(file); err != nil {
		return failed, classify(err)
	}
	// Serialize this instance's publication/deletion, including the directory
	// durability check. Streaming producers do not hold this mutation lock.
	p.mutation.Lock()
	defer p.mutation.Unlock()
	if err := operationError(operation); err != nil {
		return failed, err
	}
	if err := p.io.link(p.root, pending, filename(intent.ID())); err != nil {
		if !errors.Is(err, os.ErrExist) {
			classified := classify(err)
			// An IO failure at the publication syscall may follow an effect.
			if errors.Is(classified, backups.NewError(backups.Unavailable)) || errors.Is(classified, backups.NewError(backups.Closed)) {
				return writeResult(backups.Indeterminate, intent.ID(), backups.Metadata{}), classified
			}
			return failed, classified
		}
		existing, err := p.verify(operation, intent.ID())
		if err != nil {
			return failed, err
		}
		old := existing.Metadata()
		if old.Size() != size || old.SHA256() != sum {
			return failed, backups.NewError(backups.Conflict)
		}
		// A retry after an uncertain publication can establish durability here.
		if err := p.syncDirectory(); err != nil {
			return writeResult(backups.Indeterminate, intent.ID(), backups.Metadata{}), err
		}
		return writeResult(backups.AlreadyPresent, intent.ID(), old), nil
	}
	uncertain := writeResult(backups.Indeterminate, intent.ID(), backups.Metadata{})
	// Once Link succeeds, never compensate by removing the final artifact.
	if err := p.io.remove(p.root, pending); err != nil {
		return uncertain, classify(err)
	}
	if err := p.syncDirectory(); err != nil {
		return uncertain, err
	}
	return writeResult(backups.Published, intent.ID(), metadata), nil
}
func (p *provider) writeChunk(file *os.File, bytes []byte) error {
	n, err := p.io.write(file, bytes)
	if err != nil {
		return classify(err)
	}
	if n != len(bytes) {
		return backups.NewError(backups.Unavailable)
	}
	return nil
}

func encodeHeader(metadata backups.Metadata) [headerBytes]byte {
	var header [headerBytes]byte
	copy(header[:8], envelopeMagic[:])
	id, _ := hex.DecodeString(metadata.ID().String())
	copy(header[8:24], id)
	binary.BigEndian.PutUint64(header[24:32], uint64(metadata.Size()))
	digest := metadata.SHA256()
	copy(header[32:64], digest[:])
	binary.BigEndian.PutUint64(header[64:72], uint64(metadata.PublishedAt().UnixNano()))
	return header
}
func decodeHeader(header [headerBytes]byte, id backups.ArtifactID, fileSize int64, limit int64) (backups.Metadata, error) {
	if string(header[:8]) != string(envelopeMagic[:]) || hex.EncodeToString(header[8:24]) != id.String() {
		return backups.Metadata{}, backups.NewError(backups.Corrupt)
	}
	for _, b := range header[72:] {
		if b != 0 {
			return backups.Metadata{}, backups.NewError(backups.Corrupt)
		}
	}
	size := binary.BigEndian.Uint64(header[24:32])
	if size > uint64(limit) || fileSize != int64(size)+headerBytes {
		return backups.Metadata{}, backups.NewError(backups.Corrupt)
	}
	var digest [sha256.Size]byte
	copy(digest[:], header[32:64])
	metadata, err := backups.NewMetadata(id, int64(size), digest, time.Unix(0, int64(binary.BigEndian.Uint64(header[64:72]))).UTC())
	if err != nil {
		return backups.Metadata{}, backups.NewError(backups.Corrupt)
	}
	return metadata, nil
}
func privateArtifactLeaf(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && privateOwner(info)
}

// Root confines traversal but follows internal symlinks even with O_NOFOLLOW.
// Admit the leaf itself in this private directory, then compare both rooted
// Lstats with the opened descriptor. Open, Verify, retry and Delete share this
// admission; no canonical artifact name may be a symlink.
func (p *provider) openLeaf(id backups.ArtifactID) (*os.File, error) {
	name := filename(id)
	before, err := p.root.Lstat(name)
	if err != nil {
		return nil, classify(err)
	}
	if !privateArtifactLeaf(before) {
		return nil, backups.NewError(backups.PermissionDenied)
	}
	file, err := p.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, classify(err)
	}
	if err := p.track(file); err != nil {
		return nil, err
	}
	failed := func(err error) (*os.File, error) {
		_ = p.closeFile(file)
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return failed(classify(err))
	}
	after, err := p.root.Lstat(name)
	if err != nil {
		return failed(classify(err))
	}
	if !privateArtifactLeaf(opened) || !privateArtifactLeaf(after) || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		return failed(backups.NewError(backups.PermissionDenied))
	}
	return file, nil
}

func (p *provider) openArtifact(id backups.ArtifactID) (*os.File, backups.Metadata, error) {
	file, err := p.openLeaf(id)
	if err != nil {
		return nil, backups.Metadata{}, err
	}
	failed := func(err error) (*os.File, backups.Metadata, error) {
		_ = p.closeFile(file)
		return nil, backups.Metadata{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return failed(classify(err))
	}
	if info.Size() < headerBytes || info.Size()-headerBytes > p.limits.ArtifactBytes() {
		return failed(backups.NewError(backups.Corrupt))
	}
	var header [headerBytes]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return failed(backups.NewError(backups.Corrupt))
		}
		return failed(classify(err))
	}
	metadata, err := decodeHeader(header, id, info.Size(), p.limits.ArtifactBytes())
	if err != nil {
		return failed(err)
	}
	return file, metadata, nil
}

func (p *provider) Open(ctx context.Context, id backups.ArtifactID) (backups.ArtifactReader, error) {
	if !id.Valid() {
		return nil, backups.NewError(backups.InvalidInput)
	}
	operation, release, err := p.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := operationError(operation); err != nil {
		release()
		return nil, err
	}
	reader, err := p.reader(operation, id, release)
	if err != nil {
		release()
		return nil, err
	}
	return reader, nil
}
func (p *provider) reader(ctx context.Context, id backups.ArtifactID, release func()) (*artifactReader, error) {
	file, metadata, err := p.openArtifact(id)
	if err != nil {
		return nil, err
	}
	return &artifactReader{file: file, metadata: metadata, remaining: metadata.Size(), digest: sha256.New(), ctx: ctx, owner: p, release: release}, nil
}

type artifactReader struct {
	mu               sync.Mutex
	file             *os.File
	metadata         backups.Metadata
	remaining        int64
	digest           hash.Hash
	ctx              context.Context
	owner            *provider
	release          func()
	closed, verified bool
	failure          error
}

func (r *artifactReader) Metadata() backups.Metadata { return r.metadata }
func (r *artifactReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, backups.NewError(backups.Closed)
	}
	if r.failure != nil {
		return 0, r.failure
	}
	if err := operationError(r.ctx); err != nil {
		return 0, err
	}
	if r.verified {
		return 0, io.EOF
	}
	if len(buffer) == 0 {
		return 0, nil
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:int(r.remaining)]
	}
	n, err := r.file.Read(buffer)
	if n > 0 {
		_, _ = r.digest.Write(buffer[:n])
		r.remaining -= int64(n)
	}
	if err != nil && err != io.EOF {
		r.failure = classify(err)
		return n, r.failure
	}
	if err == io.EOF && r.remaining != 0 {
		r.failure = backups.NewError(backups.Corrupt)
		return n, r.failure
	}
	if r.remaining == 0 {
		info, statErr := r.file.Stat()
		var digest [sha256.Size]byte
		copy(digest[:], r.digest.Sum(nil))
		if statErr != nil {
			r.failure = classify(statErr)
			return n, r.failure
		}
		if info.Size() != r.metadata.Size()+headerBytes || digest != r.metadata.SHA256() {
			r.failure = backups.NewError(backups.Corrupt)
			return n, r.failure
		}
		r.verified = true
		if n == 0 {
			return 0, io.EOF
		}
	}
	return n, nil
}
func (r *artifactReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.owner.closeFile(r.file)
	r.release()
	return classify(err)
}

func (p *provider) Verify(ctx context.Context, id backups.ArtifactID) (backups.VerifiedArtifact, error) {
	if !id.Valid() {
		return backups.VerifiedArtifact{}, backups.NewError(backups.InvalidInput)
	}
	operation, release, err := p.begin(ctx)
	if err != nil {
		return backups.VerifiedArtifact{}, err
	}
	defer release()
	return p.verify(operation, id)
}
func (p *provider) verify(ctx context.Context, id backups.ArtifactID) (backups.VerifiedArtifact, error) {
	reader, err := p.reader(ctx, id, func() {})
	if err != nil {
		return backups.VerifiedArtifact{}, err
	}
	_, readErr := io.Copy(io.Discard, reader)
	closeErr := reader.Close()
	if readErr != nil {
		return backups.VerifiedArtifact{}, readErr
	}
	if closeErr != nil {
		return backups.VerifiedArtifact{}, closeErr
	}
	return backups.NewVerifiedArtifact(reader.Metadata())
}

func (p *provider) List(ctx context.Context, request backups.ListRequest) (backups.Page, error) {
	if !request.Valid() || request.Limit() > p.limits.PageSize() || request.Cursor().Present() && request.Cursor().Instance() != p.id {
		return backups.Page{}, backups.NewError(backups.InvalidInput)
	}
	operation, release, err := p.begin(ctx)
	if err != nil {
		return backups.Page{}, err
	}
	defer release()
	dir, err := p.root.OpenFile(".", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return backups.Page{}, classify(err)
	}
	if err := p.track(dir); err != nil {
		return backups.Page{}, err
	}
	entries, readErr := dir.ReadDir(p.limits.ScanEntries() + 1)
	closeErr := p.closeFile(dir)
	if readErr != nil && readErr != io.EOF {
		return backups.Page{}, classify(readErr)
	}
	if closeErr != nil {
		return backups.Page{}, classify(closeErr)
	}
	if len(entries) > p.limits.ScanEntries() {
		return backups.Page{}, backups.NewError(backups.Capacity)
	}
	items := make([]backups.Metadata, 0, len(entries))
	for _, entry := range entries {
		if err := operationError(operation); err != nil {
			return backups.Page{}, err
		}
		if !strings.HasSuffix(entry.Name(), fileSuffix) || !entry.Type().IsRegular() {
			continue
		}
		id, err := backups.ParseArtifactID(strings.TrimSuffix(entry.Name(), fileSuffix))
		if err != nil || request.Cursor().Present() && id.String() <= request.Cursor().After().String() {
			continue
		}
		file, metadata, err := p.openArtifact(id)
		if err != nil {
			if errors.Is(err, backups.NewError(backups.Corrupt)) || errors.Is(err, backups.NewError(backups.NotFound)) {
				continue
			}
			return backups.Page{}, err
		}
		if err := p.closeFile(file); err != nil {
			return backups.Page{}, classify(err)
		}
		items = append(items, metadata)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID().String() < items[j].ID().String() })
	var next backups.Cursor
	if len(items) > request.Limit() {
		items = items[:request.Limit()]
		next, _ = backups.NewCursor(p.id, items[len(items)-1].ID())
	}
	return backups.NewPage(items, next)
}

func (p *provider) Delete(ctx context.Context, id backups.ArtifactID) (backups.DeleteResult, error) {
	if !id.Valid() {
		return backups.DeleteResult{}, backups.NewError(backups.InvalidInput)
	}
	result := func(state backups.DeleteState) backups.DeleteResult {
		value, _ := backups.NewDeleteResult(state, id)
		return value
	}
	absent := func() (backups.DeleteResult, error) {
		// Also resolve a retry after a previous deletion whose fsync failed.
		if err := p.syncDirectory(); err != nil {
			return result(backups.DeleteIndeterminate), err
		}
		return result(backups.AlreadyAbsent), nil
	}
	operation, release, err := p.begin(ctx)
	if err != nil {
		return result(backups.DeleteNotApplied), err
	}
	defer release()
	p.mutation.Lock()
	defer p.mutation.Unlock()
	if err := operationError(operation); err != nil {
		return result(backups.DeleteNotApplied), err
	}
	// No recursive removal and no link traversal. A corrupt regular artifact
	// may still be removed deliberately by its admitted identity.
	file, err := p.openLeaf(id)
	if errors.Is(err, backups.NewError(backups.NotFound)) {
		return absent()
	}
	if err != nil {
		return result(backups.DeleteNotApplied), err
	}
	closeErr := p.closeFile(file)
	if closeErr != nil {
		return result(backups.DeleteNotApplied), classify(closeErr)
	}
	if err := operationError(operation); err != nil {
		return result(backups.DeleteNotApplied), err
	}
	if err := p.io.remove(p.root, filename(id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return absent()
		}
		classified := classify(err)
		if errors.Is(classified, backups.NewError(backups.Unavailable)) || errors.Is(classified, backups.NewError(backups.Closed)) {
			return result(backups.DeleteIndeterminate), classified
		}
		return result(backups.DeleteNotApplied), classified
	}
	if err := p.syncDirectory(); err != nil {
		return result(backups.DeleteIndeterminate), err
	}
	return result(backups.Deleted), nil
}

var _ backups.ManagedProvider = (*provider)(nil)
var _ backups.Factory = Factory{}
