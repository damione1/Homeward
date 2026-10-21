package backups_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/backups/local"
)

type memoryArtifact struct {
	metadata backups.Metadata
	bytes    []byte
}
type memoryProvider struct {
	mu        sync.Mutex
	id        backups.InstanceID
	closed    bool
	artifacts map[backups.ArtifactID]memoryArtifact
}

func (m *memoryProvider) Capabilities() backups.Capabilities {
	c, _ := backups.NewCapabilities(backups.DefaultLimits())
	return c
}
func (m *memoryProvider) Close() error { m.mu.Lock(); defer m.mu.Unlock(); m.closed = true; return nil }
func (m *memoryProvider) Write(ctx context.Context, intent backups.WriteIntent, reader io.Reader) (backups.WriteResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return backups.WriteResult{}, backups.NewError(backups.Closed)
	}
	if ctx.Err() != nil {
		return backups.WriteResult{}, backups.NewError(backups.Cancelled)
	}
	payload, err := io.ReadAll(io.LimitReader(reader, intent.MaxBytes()+1))
	if err != nil {
		return backups.WriteResult{}, backups.NewError(backups.Unavailable)
	}
	if int64(len(payload)) > intent.MaxBytes() {
		return backups.WriteResult{}, backups.NewError(backups.Capacity)
	}
	if old, found := m.artifacts[intent.ID()]; found {
		if !bytes.Equal(old.bytes, payload) {
			return backups.WriteResult{}, backups.NewError(backups.Conflict)
		}
		return backups.NewWriteResult(backups.AlreadyPresent, intent.ID(), old.metadata)
	}
	metadata, err := backups.NewMetadata(intent.ID(), int64(len(payload)), sha256.Sum256(payload), time.Now().UTC())
	if err != nil {
		return backups.WriteResult{}, err
	}
	m.artifacts[intent.ID()] = memoryArtifact{metadata, bytes.Clone(payload)}
	return backups.NewWriteResult(backups.Published, intent.ID(), metadata)
}
func (m *memoryProvider) List(ctx context.Context, request backups.ListRequest) (backups.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return backups.Page{}, backups.NewError(backups.Closed)
	}
	if ctx.Err() != nil {
		return backups.Page{}, backups.NewError(backups.Cancelled)
	}
	items := make([]backups.Metadata, 0, len(m.artifacts))
	for _, artifact := range m.artifacts {
		if !request.Cursor().Present() || artifact.metadata.ID().String() > request.Cursor().After().String() {
			items = append(items, artifact.metadata)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID().String() < items[j].ID().String() })
	var next backups.Cursor
	if len(items) > request.Limit() {
		items = items[:request.Limit()]
		next, _ = backups.NewCursor(m.id, items[len(items)-1].ID())
	}
	return backups.NewPage(items, next)
}

type memoryReader struct {
	*bytes.Reader
	metadata backups.Metadata
}

func (r *memoryReader) Metadata() backups.Metadata { return r.metadata }
func (*memoryReader) Close() error                 { return nil }
func (m *memoryProvider) Open(ctx context.Context, id backups.ArtifactID) (backups.ArtifactReader, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, backups.NewError(backups.Closed)
	}
	if ctx.Err() != nil {
		return nil, backups.NewError(backups.Cancelled)
	}
	artifact, found := m.artifacts[id]
	if !found {
		return nil, backups.NewError(backups.NotFound)
	}
	return &memoryReader{bytes.NewReader(bytes.Clone(artifact.bytes)), artifact.metadata}, nil
}
func (m *memoryProvider) Verify(ctx context.Context, id backups.ArtifactID) (backups.VerifiedArtifact, error) {
	reader, err := m.Open(ctx, id)
	if err != nil {
		return backups.VerifiedArtifact{}, err
	}
	defer reader.Close()
	payload, err := io.ReadAll(reader)
	if err != nil || sha256.Sum256(payload) != reader.Metadata().SHA256() {
		return backups.VerifiedArtifact{}, backups.NewError(backups.Corrupt)
	}
	return backups.NewVerifiedArtifact(reader.Metadata())
}
func (m *memoryProvider) Delete(ctx context.Context, id backups.ArtifactID) (backups.DeleteResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return backups.DeleteResult{}, backups.NewError(backups.Closed)
	}
	if ctx.Err() != nil {
		return backups.DeleteResult{}, backups.NewError(backups.Cancelled)
	}
	state := backups.AlreadyAbsent
	if _, found := m.artifacts[id]; found {
		delete(m.artifacts, id)
		state = backups.Deleted
	}
	return backups.NewDeleteResult(state, id)
}

type memoryFactory struct {
	kind         backups.ProviderKind
	calls        int
	providers    []*memoryProvider
	result       backups.ManagedProvider
	err          error
	returnResult bool
}

func (f *memoryFactory) Kind() backups.ProviderKind { return f.kind }
func (f *memoryFactory) Open(_ context.Context, settings backups.InstanceSettings) (backups.ManagedProvider, error) {
	f.calls++
	if f.returnResult {
		return f.result, f.err
	}
	p := &memoryProvider{id: settings.ID(), artifacts: make(map[backups.ArtifactID]memoryArtifact)}
	f.providers = append(f.providers, p)
	return p, nil
}
func registryConfiguration(t *testing.T) backups.Configuration {
	t.Helper()
	id, _ := backups.ParseInstanceID("local")
	settings, err := backups.NewLocalSettings(id, true, filepath.Join(t.TempDir(), "opaque-private-store"), backups.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := backups.NewConfiguration([]backups.InstanceSettings{settings})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}
func expectKind(t *testing.T, err error, kind backups.ErrorKind) {
	t.Helper()
	var typed *backups.Error
	if !errors.As(err, &typed) || typed.Kind() != kind {
		t.Fatalf("want %s got %v", kind, err)
	}
}
func TestNHA154RegistryAndConfigAdmission(t *testing.T) {
	configuration := registryConfiguration(t)
	id := configuration.Instances()[0].ID()
	if _, err := backups.NewConfiguration([]backups.InstanceSettings{{}}); err == nil {
		t.Fatal("zero union admitted")
	}
	duplicate := append(configuration.Instances(), configuration.Instances()...)
	if _, err := backups.NewConfiguration(duplicate); err == nil {
		t.Fatal("duplicate ID admitted")
	}
	for _, kind := range []string{"s3", "LOCAL", "unknown", ""} {
		if _, err := backups.ParseProviderKind(kind); err == nil {
			t.Fatalf("unavailable kind %q admitted", kind)
		}
	}
	for _, root := range []string{"relative", "/tmp/../store", "/", "/tmp/\x00private"} {
		if _, err := backups.NewLocalSettings(id, true, root, backups.DefaultLimits()); err == nil {
			t.Fatalf("invalid root %q admitted", root)
		}
	}
	if _, err := backups.NewLocalSettings(id, true, "/tmp/private", backups.Limits{}); err == nil {
		t.Fatal("zero limits admitted")
	}
	if _, err := backups.NewWriteIntent(backups.ArtifactID{}, 0); err == nil {
		t.Fatal("zero artifact admitted")
	}
	if _, err := backups.NewMetadata(backups.ArtifactID{}, 0, sha256.Sum256(nil), time.Now()); err == nil {
		t.Fatal("zero metadata identity admitted")
	}
	factory := &memoryFactory{kind: backups.Local}
	var typedNil *memoryFactory
	for _, test := range []struct {
		name      string
		config    backups.Configuration
		factories []backups.Factory
		kind      backups.ErrorKind
	}{
		{"zero-configuration", backups.Configuration{}, []backups.Factory{factory}, backups.InvalidInput},
		{"nil-factory", configuration, []backups.Factory{nil}, backups.InvalidInput},
		{"typed-nil-factory", configuration, []backups.Factory{typedNil}, backups.InvalidInput},
		{"missing-factory", configuration, nil, backups.Unsupported},
		{"invalid-kind", configuration, []backups.Factory{&memoryFactory{kind: 99}}, backups.Unsupported},
		{"duplicate-kind", configuration, []backups.Factory{factory, factory}, backups.Conflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := backups.NewRegistry(context.Background(), test.config, test.factories)
			expectKind(t, err, test.kind)
			if factory.calls != 0 {
				t.Fatal("invalid plan performed IO")
			}
		})
	}
	var nilProvider *memoryProvider
	for _, provided := range []backups.ManagedProvider{nil, nilProvider} {
		bad := &memoryFactory{kind: backups.Local, returnResult: true, result: provided}
		_, err := backups.NewRegistry(context.Background(), configuration, []backups.Factory{bad})
		expectKind(t, err, backups.Unavailable)
	}
	bad := &memoryFactory{kind: backups.Local, returnResult: true, err: fmt.Errorf("NHA154_PRIVATE_PATH /private/canary")}
	_, err := backups.NewRegistry(context.Background(), configuration, []backups.Factory{bad})
	expectKind(t, err, backups.Unavailable)
	if strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("factory error leaked")
	}
	registry, err := backups.NewRegistry(context.Background(), configuration, []backups.Factory{factory})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := registry.Instance(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, owner := provider.(backups.ManagedProvider); owner {
		t.Fatal("borrower can close shared storage")
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if !factory.providers[0].closed {
		t.Fatal("owner failed to close instance")
	}
	_, err = registry.Instance(id)
	expectKind(t, err, backups.Closed)
	instances := configuration.Instances()
	instances[0] = backups.InstanceSettings{}
	if !configuration.Instances()[0].Valid() {
		t.Fatal("mutable configuration backing slice")
	}
	privateRoot, _ := configuration.Instances()[0].Local()
	jsonBytes, _ := json.Marshal(configuration)
	for _, rendered := range []string{fmt.Sprintf("%+v %#v", configuration, configuration), fmt.Sprintf("%+v %#v", privateRoot, privateRoot), string(jsonBytes)} {
		if strings.Contains(rendered, privateRoot.Root()) {
			t.Fatal("private root escaped formatting")
		}
	}
}
func TestNHA154OpaqueConsumerPort(t *testing.T) {
	for _, mode := range []string{"memory", "local"} {
		t.Run(mode, func(t *testing.T) {
			configuration := registryConfiguration(t)
			var factory backups.Factory = &memoryFactory{kind: backups.Local}
			if mode == "local" {
				factory = local.Factory{}
			}
			registry, err := backups.NewRegistry(context.Background(), configuration, []backups.Factory{factory})
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			provider, err := registry.Instance(configuration.Instances()[0].ID())
			if err != nil {
				t.Fatal(err)
			}
			id, _ := backups.ParseArtifactID("0123456789abcdef0123456789abcdef")
			if err := consumeOpaqueArtifact(context.Background(), provider, id, []byte("opaque consumer fixture\x00\xff")); err != nil {
				t.Fatal(err)
			}
			request, _ := backups.NewListRequest(2, backups.Cursor{})
			page, err := provider.List(context.Background(), request)
			if err != nil || len(page.Items()) != 1 {
				t.Fatalf("list shared port: %v", err)
			}
			result, err := provider.Delete(context.Background(), id)
			if err != nil || result.State() != backups.Deleted {
				t.Fatalf("delete shared port: %v", err)
			}
		})
	}
}

type nilContext struct{ context.Context }
type secondOpenFailure struct {
	first *memoryProvider
	calls int
}

func (*secondOpenFailure) Kind() backups.ProviderKind { return backups.Local }
func (f *secondOpenFailure) Open(_ context.Context, settings backups.InstanceSettings) (backups.ManagedProvider, error) {
	f.calls++
	if f.calls == 2 {
		return nil, backups.NewError(backups.Unavailable)
	}
	f.first = &memoryProvider{id: settings.ID(), artifacts: make(map[backups.ArtifactID]memoryArtifact)}
	return f.first, nil
}
func TestNHA154RegistryRollbackAndDisabledInstances(t *testing.T) {
	configuration := registryConfiguration(t)
	first := configuration.Instances()[0]
	localSettings, _ := first.Local()
	secondID, _ := backups.ParseInstanceID("secondary")
	second, _ := backups.NewLocalSettings(secondID, true, localSettings.Root(), backups.DefaultLimits())
	configuration, err := backups.NewConfiguration([]backups.InstanceSettings{first, second})
	if err != nil {
		t.Fatal(err)
	}
	factory := &secondOpenFailure{}
	_, err = backups.NewRegistry(context.Background(), configuration, []backups.Factory{factory})
	expectKind(t, err, backups.Unavailable)
	if factory.calls != 2 || factory.first == nil || !factory.first.closed {
		t.Fatal("failed registry left its previous owner open")
	}
	disabled, _ := backups.NewLocalSettings(first.ID(), false, localSettings.Root(), backups.DefaultLimits())
	configuration, _ = backups.NewConfiguration([]backups.InstanceSettings{disabled})
	registry, err := backups.NewRegistry(context.Background(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Instance(first.ID())
	expectKind(t, err, backups.NotFound)
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	configuration, _ = backups.NewConfiguration(nil)
	registry, err = backups.NewRegistry(context.Background(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = registry.Close()
	var typedNil *nilContext
	_, err = backups.NewRegistry(typedNil, configuration, nil)
	expectKind(t, err, backups.InvalidInput)
	_, err = backups.NewRegistry(nil, configuration, nil)
	expectKind(t, err, backups.InvalidInput)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = backups.NewRegistry(ctx, configuration, nil)
	expectKind(t, err, backups.Cancelled)
}
