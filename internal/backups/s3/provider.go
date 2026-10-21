package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/storage"
)

const suffix = ".age"

type provider struct {
	client    *awss3.Client
	transport *http.Transport
	settings  backups.S3Settings
	id        backups.InstanceID
	namespace string
	root      *os.Root
	ctx       context.Context
	cancel    context.CancelFunc
	slots     chan struct{}
	mu        sync.Mutex
	closed    bool
	files     map[*os.File]struct{}
}

func open(ctx context.Context, instance backups.InstanceSettings, secrets SecretReader, directory string, transport *http.Transport) (*provider, error) {
	if backups.IsNil(ctx) || ctx.Err() != nil || !instance.Valid() || !instance.Enabled() || backups.IsNil(secrets) || !filepath.IsAbs(directory) {
		return nil, backups.NewError(backups.InvalidInput)
	}
	settings, err := instance.S3()
	if err != nil {
		return nil, err
	}
	if _, err := (credentials{reader: secrets, ref: settings.CredentialRef()}).Retrieve(ctx); err != nil {
		return nil, classify(err)
	}
	base, err := storage.OpenPrivateDataDir(directory)
	if err != nil {
		return nil, backups.NewError(backups.PermissionDenied)
	}
	if err := base.Close(); err != nil {
		return nil, backups.NewError(backups.Unavailable)
	}
	root, err := storage.OpenPrivateDataDir(filepath.Join(directory, instance.ID().String()))
	if err != nil {
		return nil, backups.NewError(backups.PermissionDenied)
	}
	client, transport, err := newClient(settings, secrets, transport)
	if err != nil {
		root.Close()
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &provider{client: client, transport: transport, settings: settings, id: instance.ID(), namespace: settings.Prefix() + "/" + instance.ID().String() + "/", root: root, ctx: lifetime, cancel: cancel, slots: make(chan struct{}, settings.Limits().ConcurrentOperations()), files: make(map[*os.File]struct{})}, nil
}
func (*provider) String() string                 { return "backup.S3(<private>)" }
func (p *provider) GoString() string             { return p.String() }
func (p *provider) Format(s fmt.State, _ rune)   { _, _ = s.Write([]byte(p.String())) }
func (p *provider) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }
func (p *provider) Capabilities() backups.Capabilities {
	if p == nil {
		return backups.Capabilities{}
	}
	v, _ := backups.NewCapabilities(p.settings.Limits())
	return v
}
func (p *provider) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.cancel()
	var files []*os.File
	for f := range p.files {
		files = append(files, f)
	}
	p.files = make(map[*os.File]struct{})
	p.mu.Unlock()
	for _, f := range files {
		_ = f.Close()
	}
	p.transport.CloseIdleConnections()
	return classify(p.root.Close())
}
func (p *provider) begin(ctx context.Context) (context.Context, func(), error) {
	if p == nil || backups.IsNil(ctx) {
		return nil, nil, backups.NewError(backups.InvalidInput)
	}
	if ctx.Err() != nil {
		return nil, nil, backups.NewError(backups.Cancelled)
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, nil, backups.NewError(backups.Closed)
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return nil, nil, backups.NewError(backups.Capacity)
	}
	op, cancel := context.WithTimeout(ctx, 5*time.Minute)
	stop := context.AfterFunc(p.ctx, cancel)
	var once sync.Once
	done := func() { once.Do(func() { stop(); cancel(); <-p.slots }) }
	if p.ctx.Err() != nil {
		done()
		return nil, nil, backups.NewError(backups.Closed)
	}
	return op, done, nil
}
func (p *provider) key(id backups.ArtifactID) string { return p.namespace + id.String() + suffix }
func result(state backups.PublicationState, id backups.ArtifactID, m backups.Metadata) backups.WriteResult {
	r, _ := backups.NewWriteResult(state, id, m)
	return r
}
func deleted(state backups.DeleteState, id backups.ArtifactID) backups.DeleteResult {
	r, _ := backups.NewDeleteResult(state, id)
	return r
}

// Only a complete local ciphertext image is sent. This does not create a second
// archive format, decrypt plaintext or persist credentials in staging.
func (p *provider) spool(ctx context.Context, intent backups.WriteIntent, source io.Reader) (*os.File, func(), backups.Metadata, error) {
	id, err := backups.NewArtifactID()
	if err != nil {
		return nil, nil, backups.Metadata{}, err
	}
	name := ".pending-" + id.String()
	f, err := p.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, nil, backups.Metadata{}, backups.NewError(backups.Unavailable)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		f.Close()
		p.root.Remove(name)
		return nil, nil, backups.Metadata{}, backups.NewError(backups.Closed)
	}
	p.files[f] = struct{}{}
	p.mu.Unlock()
	cleanup := func() { p.mu.Lock(); delete(p.files, f); p.mu.Unlock(); _ = f.Close(); _ = p.root.Remove(name) }
	fail := func(err error) (*os.File, func(), backups.Metadata, error) {
		cleanup()
		return nil, nil, backups.Metadata{}, err
	}
	if err := f.Chmod(0600); err != nil {
		return fail(backups.NewError(backups.PermissionDenied))
	}
	h := sha256.New()
	var size int64
	empty := 0
	buf := make([]byte, 32<<10)
	for {
		if ctx.Err() != nil {
			return fail(backups.NewError(backups.Cancelled))
		}
		window := min(int64(len(buf)), intent.MaxBytes()-size+1)
		n, readErr := source.Read(buf[:window])
		if n < 0 || int64(n) > window {
			return fail(backups.NewError(backups.InvalidInput))
		}
		if int64(n) > intent.MaxBytes()-size {
			return fail(backups.NewError(backups.Capacity))
		}
		if n > 0 {
			empty = 0
			written, err := io.MultiWriter(f, h).Write(buf[:n])
			if err != nil || written != n {
				return fail(backups.NewError(backups.Unavailable))
			}
			size += int64(n)
		} else {
			empty++
			if empty > 100 {
				return fail(backups.NewError(backups.Unavailable))
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return fail(backups.NewError(backups.Unavailable))
			}
			break
		}
	}
	if ctx.Err() != nil {
		return fail(backups.NewError(backups.Cancelled))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(backups.NewError(backups.Unavailable))
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	metadata, err := backups.NewMetadata(intent.ID(), size, digest, time.Now().UTC())
	if err != nil {
		return fail(err)
	}
	return f, cleanup, metadata, nil
}
func (p *provider) Write(ctx context.Context, intent backups.WriteIntent, source io.Reader) (backups.WriteResult, error) {
	if p == nil || !intent.Valid() || backups.IsNil(source) || intent.MaxBytes() > p.settings.Limits().ArtifactBytes() {
		return backups.WriteResult{}, backups.NewError(backups.InvalidInput)
	}
	failed := result(backups.NotPublished, intent.ID(), backups.Metadata{})
	op, done, err := p.begin(ctx)
	if err != nil {
		return failed, err
	}
	defer done()
	f, cleanup, metadata, err := p.spool(op, intent, source)
	if err != nil {
		return failed, err
	}
	defer cleanup()
	if op.Err() != nil {
		return failed, backups.NewError(backups.Cancelled)
	}
	digest := metadata.SHA256()
	fields := map[string]string{"homeward-version": "1", "homeward-instance": p.id.String(), "homeward-artifact": intent.ID().String(), "homeward-sha256": hex.EncodeToString(digest[:]), "homeward-published": strconv.FormatInt(metadata.PublishedAt().UnixNano(), 10)}
	_, writeErr := p.client.PutObject(op, &awss3.PutObjectInput{Bucket: aws.String(p.settings.Bucket()), Key: aws.String(p.key(intent.ID())), Body: f, ContentLength: aws.Int64(metadata.Size()), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(digest[:])), ContentType: aws.String("application/octet-stream"), IfNoneMatch: aws.String("*"), Metadata: fields})
	uncertain := result(backups.Indeterminate, intent.ID(), backups.Metadata{})
	if writeErr != nil && !errorIs(writeErr, backups.Conflict) {
		return uncertain, classify(writeErr)
	}
	// A 409 remains a failure, even if a concurrent object happens to match. Only
	// the definite precondition 412 can represent AlreadyPresent.
	if writeErr != nil && !isPrecondition(writeErr) {
		return uncertain, classify(writeErr)
	}
	verified, err := p.verifyWithin(op, intent.ID())
	if err != nil {
		return uncertain, err
	}
	m := verified.Metadata()
	if m.Size() != metadata.Size() || m.SHA256() != metadata.SHA256() {
		if writeErr != nil {
			return failed, backups.NewError(backups.Conflict)
		}
		return uncertain, backups.NewError(backups.Corrupt)
	}
	if op.Err() != nil {
		return uncertain, backups.NewError(backups.Cancelled)
	}
	if writeErr != nil {
		return result(backups.AlreadyPresent, intent.ID(), m), nil
	}
	return result(backups.Published, intent.ID(), m), nil
}
func isPrecondition(err error) bool {
	var api interface{ ErrorCode() string }
	return errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed"
}

func (p *provider) metadata(id backups.ArtifactID, size *int64, fields map[string]string) (backups.Metadata, error) {
	if !id.Valid() || size == nil || *size < 0 || *size > p.settings.Limits().ArtifactBytes() || !fixedMetadataKeys(fields) || fields["homeward-version"] != "1" || fields["homeward-instance"] != p.id.String() || fields["homeward-artifact"] != id.String() {
		return backups.Metadata{}, backups.NewError(backups.Corrupt)
	}
	raw, err := hex.DecodeString(fields["homeward-sha256"])
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != fields["homeward-sha256"] {
		return backups.Metadata{}, backups.NewError(backups.Corrupt)
	}
	nanos, err := strconv.ParseInt(fields["homeward-published"], 10, 64)
	if err != nil {
		return backups.Metadata{}, backups.NewError(backups.Corrupt)
	}
	var digest [32]byte
	copy(digest[:], raw)
	m, err := backups.NewMetadata(id, *size, digest, time.Unix(0, nanos).UTC())
	if err != nil {
		return backups.Metadata{}, backups.NewError(backups.Corrupt)
	}
	return m, nil
}
func (p *provider) openWithin(ctx context.Context, id backups.ArtifactID, done func()) (*reader, error) {
	response, err := p.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(p.settings.Bucket()), Key: aws.String(p.key(id))})
	if err != nil {
		return nil, classify(err)
	}
	if response == nil || backups.IsNil(response.Body) {
		return nil, backups.NewError(backups.Corrupt)
	}
	m, err := p.metadata(id, response.ContentLength, response.Metadata)
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	return &reader{body: response.Body, metadata: m, digest: sha256.New(), done: done, ctx: ctx}, nil
}
func (p *provider) Open(ctx context.Context, id backups.ArtifactID) (backups.ArtifactReader, error) {
	if !id.Valid() {
		return nil, backups.NewError(backups.InvalidInput)
	}
	op, done, err := p.begin(ctx)
	if err != nil {
		return nil, err
	}
	readCtx, abort := context.WithCancel(op)
	r, err := p.openWithin(readCtx, id, func() { abort(); done() })
	if err != nil {
		abort()
		done()
		return nil, err
	}
	r.abort = abort
	return r, nil
}
func (p *provider) verifyWithin(ctx context.Context, id backups.ArtifactID) (backups.VerifiedArtifact, error) {
	r, err := p.openWithin(ctx, id, func() {})
	if err != nil {
		return backups.VerifiedArtifact{}, err
	}
	_, readErr := io.Copy(io.Discard, r)
	closeErr := r.Close()
	if readErr != nil {
		return backups.VerifiedArtifact{}, classify(readErr)
	}
	if closeErr != nil {
		return backups.VerifiedArtifact{}, closeErr
	}
	return backups.NewVerifiedArtifact(r.metadata)
}
func (p *provider) Verify(ctx context.Context, id backups.ArtifactID) (backups.VerifiedArtifact, error) {
	if !id.Valid() {
		return backups.VerifiedArtifact{}, backups.NewError(backups.InvalidInput)
	}
	op, done, err := p.begin(ctx)
	if err != nil {
		return backups.VerifiedArtifact{}, err
	}
	defer done()
	return p.verifyWithin(op, id)
}
func (p *provider) head(ctx context.Context, id backups.ArtifactID) (backups.Metadata, string, error) {
	r, err := p.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(p.settings.Bucket()), Key: aws.String(p.key(id))})
	if err != nil {
		return backups.Metadata{}, "", classify(err)
	}
	if r == nil || r.ETag == nil || len(*r.ETag) < 2 || len(*r.ETag) > 256 || strings.ContainsAny(*r.ETag, "\r\n") {
		return backups.Metadata{}, "", backups.NewError(backups.Corrupt)
	}
	m, err := p.metadata(id, r.ContentLength, r.Metadata)
	return m, *r.ETag, err
}
func (p *provider) Delete(ctx context.Context, id backups.ArtifactID) (backups.DeleteResult, error) {
	if !id.Valid() {
		return backups.DeleteResult{}, backups.NewError(backups.InvalidInput)
	}
	failed := deleted(backups.DeleteNotApplied, id)
	op, done, err := p.begin(ctx)
	if err != nil {
		return failed, err
	}
	defer done()
	_, etag, err := p.head(op, id)
	if errorIs(err, backups.NotFound) {
		return deleted(backups.AlreadyAbsent, id), nil
	}
	if err != nil {
		return failed, err
	}
	_, err = p.client.DeleteObject(op, &awss3.DeleteObjectInput{Bucket: aws.String(p.settings.Bucket()), Key: aws.String(p.key(id)), IfMatch: aws.String(etag)})
	if err != nil {
		return deleted(backups.DeleteIndeterminate, id), classify(err)
	}
	if op.Err() != nil {
		return deleted(backups.DeleteIndeterminate, id), backups.NewError(backups.Cancelled)
	}
	return deleted(backups.Deleted, id), nil
}
func (p *provider) List(ctx context.Context, request backups.ListRequest) (backups.Page, error) {
	if p == nil || !request.Valid() || request.Limit() > p.settings.Limits().PageSize() || request.Cursor().Present() && request.Cursor().Instance() != p.id {
		return backups.Page{}, backups.NewError(backups.InvalidInput)
	}
	op, done, err := p.begin(ctx)
	if err != nil {
		return backups.Page{}, err
	}
	defer done()
	params := &awss3.ListObjectsV2Input{Bucket: aws.String(p.settings.Bucket()), Prefix: aws.String(p.namespace), MaxKeys: aws.Int32(int32(min(request.Limit()+1, p.settings.Limits().PageSize())))}
	last := ""
	if request.Cursor().Present() {
		last = p.key(request.Cursor().After())
		params.StartAfter = aws.String(last)
	}
	items := make([]backups.Metadata, 0, request.Limit()+1)
	seenTokens := map[string]bool{}
	scanned := 0
	for {
		r, err := p.client.ListObjectsV2(op, params)
		if err != nil {
			return backups.Page{}, classify(err)
		}
		if r == nil || r.IsTruncated == nil {
			return backups.Page{}, backups.NewError(backups.Corrupt)
		}
		for _, object := range r.Contents {
			scanned++
			if scanned > p.settings.Limits().ScanEntries() {
				return backups.Page{}, backups.NewError(backups.Capacity)
			}
			if object.Key == nil {
				return backups.Page{}, backups.NewError(backups.Corrupt)
			}
			key := *object.Key
			if key <= last {
				return backups.Page{}, backups.NewError(backups.Corrupt)
			}
			last = key
			if !strings.HasPrefix(key, p.namespace) || !strings.HasSuffix(key, suffix) {
				continue
			}
			id, err := backups.ParseArtifactID(strings.TrimSuffix(strings.TrimPrefix(key, p.namespace), suffix))
			if err != nil || p.key(id) != key {
				continue
			}
			m, _, err := p.head(op, id)
			if errorIs(err, backups.NotFound) || errorIs(err, backups.Corrupt) {
				continue
			}
			if err != nil {
				return backups.Page{}, err
			}
			items = append(items, m)
			if len(items) > request.Limit() {
				cursor, _ := backups.NewCursor(p.id, items[request.Limit()-1].ID())
				return backups.NewPage(items[:request.Limit()], cursor)
			}
		}
		if !*r.IsTruncated {
			return backups.NewPage(items, backups.Cursor{})
		}
		if r.NextContinuationToken == nil || len(*r.NextContinuationToken) == 0 || len(*r.NextContinuationToken) > 4096 || seenTokens[*r.NextContinuationToken] || len(r.Contents) == 0 {
			return backups.Page{}, backups.NewError(backups.Corrupt)
		}
		seenTokens[*r.NextContinuationToken] = true
		params.ContinuationToken = r.NextContinuationToken
		params.StartAfter = nil
	}
}

type reader struct {
	mu          sync.Mutex
	body        io.ReadCloser
	metadata    backups.Metadata
	digest      hash.Hash
	size        int64
	ctx         context.Context
	done        func()
	abort       context.CancelFunc
	closed, eof bool
	terminal    error
	closeOnce   sync.Once
	closeErr    error
}

func (r *reader) Metadata() backups.Metadata { return r.metadata }
func (r *reader) Read(buf []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, backups.NewError(backups.Closed)
	}
	if r.terminal != nil {
		return 0, r.terminal
	}
	if r.eof {
		return 0, io.EOF
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if r.ctx.Err() != nil {
		return 0, backups.NewError(backups.Cancelled)
	}
	window := min(int64(len(buf)), r.metadata.Size()-r.size+1)
	n, err := r.body.Read(buf[:window])
	if n < 0 || int64(n) > window {
		r.terminal = backups.NewError(backups.Corrupt)
		return 0, r.terminal
	}
	if int64(n) > r.metadata.Size()-r.size {
		r.terminal = backups.NewError(backups.Corrupt)
		return 0, r.terminal
	}
	if n > 0 {
		_, _ = r.digest.Write(buf[:n])
		r.size += int64(n)
	}
	if err != nil {
		if err != io.EOF {
			r.terminal = classify(err)
			return n, r.terminal
		}
		var digest [32]byte
		copy(digest[:], r.digest.Sum(nil))
		if r.size != r.metadata.Size() || digest != r.metadata.SHA256() {
			r.terminal = backups.NewError(backups.Corrupt)
			return n, r.terminal
		}
		r.eof = true
	}
	return n, err
}
func (r *reader) Close() error {
	r.closeOnce.Do(func() {
		if r.abort != nil {
			r.abort()
		}
		err := r.body.Close()
		r.mu.Lock()
		r.closed = true
		r.closeErr = classify(err)
		r.mu.Unlock()
		r.done()
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeErr
}

var _ backups.ManagedProvider = (*provider)(nil)
