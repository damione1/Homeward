package configdocuments

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

type Service struct {
	admitted  bool
	store     *storage.Store
	owner     *configactivation.Owner
	replay    *storage.ConfigReplayRepository
	workspace *Workspace
	journal   *Journal
	mutation  chan struct{}
	key       [32]byte
	dirtyMu   sync.Mutex
	rejected  [32]byte
	last      Change
}
type Input struct {
	Store   *storage.Store
	Owner   *configactivation.Owner
	Startup config.StartupInputs
	Secrets *storage.SecretRepository
}

// NewService composes the actual delivered owners. There is no injected fake
// activation callback or second publisher behind the API.
func NewService(input Input) (*Service, error) {
	if input.Store == nil || input.Owner == nil || !input.Startup.Valid() || input.Secrets == nil || !input.Secrets.Valid() {
		return nil, &Error{kind: FailureInvalid}
	}
	replay, err := storage.NewConfigReplayRepository(input.Store, input.Secrets)
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	workspace, err := NewWorkspace(input.Startup)
	if err != nil {
		return nil, err
	}
	journal, err := NewJournal()
	if err != nil {
		workspace.Close()
		return nil, err
	}
	s := &Service{admitted: true, store: input.Store, owner: input.Owner, replay: replay, workspace: workspace, journal: journal, mutation: make(chan struct{}, 1)}
	if _, err := rand.Read(s.key[:]); err != nil || s.key == ([32]byte{}) {
		workspace.Close()
		journal.Close()
		return nil, &Error{kind: Unavailable}
	}
	return s, nil
}
func (s *Service) Close() error {
	if s == nil || !s.admitted {
		return nil
	}
	s.journal.Close()
	return s.workspace.Close()
}
func (s *Service) Store() *storage.Store {
	if s == nil || !s.admitted {
		return nil
	}
	return s.store
}

type Snapshot struct {
	document    storage.ConfigDocumentMetadata
	generation  storage.ConfigGenerationRevision
	manifest    [32]byte
	catalog     storage.ConfigDocumentCut
	observation Observation
	dirty       DirtyState
	position    Cursor
	token       string
}

func (s Snapshot) Document() storage.ConfigDocumentMetadata     { return s.document }
func (s Snapshot) Generation() storage.ConfigGenerationRevision { return s.generation }
func (s Snapshot) ManifestSHA256() [32]byte                     { return s.manifest }
func (s Snapshot) CatalogCut() storage.ConfigDocumentCut        { return s.catalog }
func (s Snapshot) Workspace() Observation                       { return s.observation }
func (s Snapshot) Dirty() DirtyState                            { return s.dirty }
func (s Snapshot) ResumeToken() string                          { return s.token }

func operationalID() storage.ConfigDocumentID {
	id, _ := storage.NewConfigDocumentID("operational.main")
	return id
}

// Read captures any admitted versioned document through the real metadata
// reader. SQL active membership is not guessed from kind or a physical path.
// Only the operational document has a workspace locator in this owner; other
// kinds retain an explicit unobserved workspace state, never invented clean.
// The signed resume binding is global: catalogue cut plus selected operational
// observation between equal owner/journal stamps, independent of requested ID.
func (s *Service) Read(ctx context.Context, id storage.ConfigDocumentID) (Snapshot, error) {
	if s == nil || !s.admitted || !usableContext(ctx) || !id.Valid() {
		return Snapshot{}, &Error{kind: FailureInvalid}
	}
	for attempt := 0; attempt < 2; attempt++ {
		position, err := s.journal.Position()
		if err != nil {
			return Snapshot{}, err
		}
		before, err := s.owner.CurrentConfig(ctx)
		if err != nil {
			return Snapshot{}, &Error{kind: Unavailable}
		}
		state, err := s.store.ReadConfigDocumentState(ctx, id, operationalID())
		if err != nil {
			return Snapshot{}, err
		}
		observation, observationErr := s.workspace.Observe()
		after, err := s.owner.CurrentConfig(ctx)
		if err != nil {
			return Snapshot{}, &Error{kind: Unavailable}
		}
		through, err := s.journal.Position()
		if err != nil {
			return Snapshot{}, err
		}
		if before != after || !position.Equal(through) {
			continue
		}
		generation, manifest, err := after.Components()
		if err != nil || int64(generation) != state.Generation().Number() || manifest != state.ManifestSHA256() {
			return Snapshot{}, &Error{kind: Unavailable}
		}
		dirty := DirtyUnavailable
		observed, _ := observation.SHA256()
		if observationErr == nil {
			dirty = DirtyUnvalidated
			if observed == state.Operational().SourceSHA256() && state.Operational().Active() {
				dirty = Clean
			}
			s.dirtyMu.Lock()
			if dirty != Clean && s.rejected == observed {
				dirty = DirtyRejected
			}
			s.dirtyMu.Unlock()
			if observation.ReconciliationPending() {
				dirty = DirtyReconciling
			}
		}
		h := sha256.New()
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], generation)
		_, _ = h.Write(n[:])
		_, _ = h.Write(manifest[:])
		catalogue := state.Cut().SHA256()
		_, _ = h.Write(catalogue[:])
		_, _ = h.Write(observed[:])
		_, _ = h.Write([]byte{byte(dirty)})
		var cut [32]byte
		copy(cut[:], h.Sum(nil))
		bound, err := through.Bind(cut)
		if err != nil {
			return Snapshot{}, err
		}
		token, err := s.journal.Encode(bound)
		if err != nil {
			return Snapshot{}, err
		}
		if id != operationalID() {
			observation = Observation{}
			dirty = DirtyUnknown
		}
		return Snapshot{document: state.Document(), generation: state.Generation(), manifest: manifest, catalog: state.Cut(), observation: observation, dirty: dirty, position: bound, token: token}, nil
	}
	return Snapshot{}, &Error{kind: Unavailable}
}

// ProtectedReference is a keyed opaque public handle. The source's logical path,
// secret names and exact bytes cannot be recovered from this representation.
func (s *Service) ProtectedReference(document storage.ConfigDocumentMetadata) []byte {
	if s == nil || !s.admitted || !document.Ref().Valid() {
		return nil
	}
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write([]byte("homeward/config-protected-reference/1\x00" + document.Ref().Document().String()))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(document.Ref().Revision().Number()))
	_, _ = mac.Write(n[:])
	digest := document.SourceSHA256()
	_, _ = mac.Write(digest[:])
	return mac.Sum(nil)
}

type ExpectedDocument struct {
	id                storage.ConfigDocumentID
	revision          storage.ConfigVersionRevision
	source, workspace [32]byte
}

func NewExpectedDocument(id storage.ConfigDocumentID, revision storage.ConfigVersionRevision, source, workspace [32]byte) (ExpectedDocument, error) {
	if id != operationalID() || !revision.Valid() || source == ([32]byte{}) || workspace == ([32]byte{}) {
		return ExpectedDocument{}, &Error{kind: FailureInvalid}
	}
	return ExpectedDocument{id: id, revision: revision, source: source, workspace: workspace}, nil
}
func (e ExpectedDocument) valid() bool {
	_, err := NewExpectedDocument(e.id, e.revision, e.source, e.workspace)
	return err == nil
}

func (s *Service) acquireMutation(ctx context.Context) (func(), error) {
	if s == nil || !s.admitted || !usableContext(ctx) {
		return nil, &Error{kind: FailureInvalid}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s.mutation <- struct{}{}:
		return func() { <-s.mutation }, nil
	}
}

func (s *Service) Write(ctx context.Context, expected ExpectedDocument, raw []byte, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !expected.valid() || !author.Valid() || len(raw) < 1 || len(raw) > 1<<20 {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return configactivation.Result{}, err
	}
	defer release()
	return s.writeLocked(ctx, expected, raw, author)
}
func (s *Service) writeLocked(ctx context.Context, expected ExpectedDocument, raw []byte, author storage.ConfigAuthor) (configactivation.Result, error) {
	current, err := s.Read(ctx, expected.id)
	if err != nil {
		return configactivation.Result{}, err
	}
	observed, _ := current.observation.SHA256()
	if current.document.Ref().Revision() != expected.revision || current.document.SourceSHA256() != expected.source || observed != expected.workspace {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	source, err := storage.NewConfigSource("api.configdocuments")
	if err != nil {
		return configactivation.Result{}, &Error{kind: Unavailable}
	}
	request, err := configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, nil, nil)
	if err != nil {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	precondition, err := configactivation.NewWorkspacePrecondition(current.generation, current.manifest, sha256.Sum256(raw))
	if err != nil {
		return configactivation.Result{}, err
	}
	request, err = request.WithWorkspacePrecondition(precondition)
	if err != nil {
		return configactivation.Result{}, err
	}
	edit, err := s.workspace.BeginEdit(expected.workspace, current.document.SourceSHA256(), raw)
	if err != nil {
		return configactivation.Result{}, err
	}
	result, activationErr := s.owner.Activate(ctx, request)
	known := false
	switch result.Outcome() {
	case configactivation.Published, configactivation.RejectedBeforeCommit, configactivation.NotCommitted, configactivation.RestartRequired:
		known = true
	case configactivation.CommitUnknown, configactivation.CommittedReconciling, configactivation.OutcomeInvalid:
	}
	var sqlSource [32]byte
	// Query on a detached bounded context: request cancellation cannot turn a
	// known SQL result into silent deletion of its reconciliation evidence.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	active, present, readErr := s.store.ReadActiveConfig(readCtx)
	cancel()
	if readErr == nil && present {
		for _, version := range active.Versions() {
			if version.Ref().Document() == operationalID() {
				sqlSource = version.SHA256()
			}
		}
	}
	finishErr := edit.Finish(sqlSource, known && readErr == nil)
	if result.Outcome() == configactivation.RejectedBeforeCommit {
		s.dirtyMu.Lock()
		s.rejected = sha256.Sum256(raw)
		s.dirtyMu.Unlock()
	}
	if finishErr != nil {
		change, err := NewChange(expected.id, expected.revision, expected.source, [32]byte{}, DirtyReconciling)
		if err == nil {
			_ = s.journal.Append([]Change{change})
		}
		return result, errors.Join(activationErr, finishErr)
	}
	// Observations, including rejected dirty candidates, are journaled through
	// the same port the future watcher uses. No second watcher is started here.
	refreshCtx, refreshCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	_, _ = s.Refresh(refreshCtx)
	refreshCancel()
	return result, activationErr
}

// RestoreDocument selects a retained operational member from a complete sealed
// generation. It creates a new revision via Write/029; it never rewinds SQL or
// selects a historical launch scope/Manifest. Current definition domains remain
// owned by their specialized APIs.
func (s *Service) RestoreDocument(ctx context.Context, expected ExpectedDocument, generation storage.ConfigGenerationRevision, manifest [32]byte, author storage.ConfigAuthor) (configactivation.Result, error) {
	if s == nil || !s.admitted || !expected.valid() || !generation.Valid() || manifest == ([32]byte{}) || !author.Valid() {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return configactivation.Result{}, err
	}
	defer release()
	historical, err := s.store.ReadConfigGeneration(ctx, generation)
	if err != nil {
		return configactivation.Result{}, err
	}
	if historical.ManifestSHA256() != manifest {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	if historical.Availability() != storage.ConfigGenerationComplete {
		return configactivation.Result{}, &Error{kind: IncompleteHistory}
	}
	var ref storage.ConfigVersionRef
	for _, version := range historical.Versions() {
		if version.Ref().Document() == expected.id {
			ref = version.Ref()
		}
	}
	if !ref.Valid() {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	protected, err := s.replay.ReadHistoricalProtectedSource(ctx, generation, manifest, ref)
	if err != nil {
		return configactivation.Result{}, err
	}
	return s.writeLocked(ctx, expected, protected.Bytes(), author)
}

// Refresh is the single dirty-observation port reused by watcher150. It records
// admitted metadata plus exact rejected-source hash, never parser error text.
func (s *Service) Refresh(ctx context.Context) (Snapshot, error) {
	snapshot, err := s.Read(ctx, operationalID())
	if err != nil {
		return Snapshot{}, err
	}
	hash, _ := snapshot.observation.SHA256()
	change, err := NewChange(operationalID(), snapshot.document.Ref().Revision(), snapshot.document.SourceSHA256(), hash, snapshot.dirty)
	if err != nil {
		return Snapshot{}, err
	}
	s.dirtyMu.Lock()
	defer s.dirtyMu.Unlock()
	if change != s.last {
		if err := s.journal.Append([]Change{change}); err != nil {
			return Snapshot{}, err
		}
		s.last = change
	}
	return snapshot, nil
}

// ReconcileWorkspace is trusted recovery composition, shared with watcher150.
// It never changes the editable file. A crash before SQL COMMIT remains a
// visible dirty/reconciling state until 029 resolves its transaction. Read
// requires the real Owner and SQL authority to agree before retiring artifacts;
// a resolved rollback preserves the exact editable candidate as dirty.
func (s *Service) ReconcileWorkspace(ctx context.Context) (bool, error) {
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	current, err := s.Read(ctx, operationalID())
	if err != nil {
		return false, err
	}
	return s.workspace.Reconcile(current.document.SourceSHA256())
}

// ActivateObserved is the one future watcher admission port. It uses the same
// workspace serialization and the same 029 owner as API writes. Its author is
// fixed audit metadata for a local filesystem observation, never a principal,
// session, routine permission or configurable execution authority.
func (s *Service) ActivateObserved(ctx context.Context, expected ExpectedDocument) (configactivation.Result, error) {
	if !expected.valid() {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return configactivation.Result{}, err
	}
	defer release()
	current, err := s.Read(ctx, expected.id)
	if err != nil {
		return configactivation.Result{}, err
	}
	observed, _ := current.observation.SHA256()
	if current.document.Ref().Revision() != expected.revision || current.document.SourceSHA256() != expected.source || observed != expected.workspace {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	if current.observation.ReconciliationPending() {
		return configactivation.Result{}, &Error{kind: ReconciliationRequired}
	}
	author, _ := storage.NewConfigAuthor("workspace.local")
	source, _ := storage.NewConfigSource("workspace.observed")
	request, err := configactivation.NewWorkspaceRequest(configactivation.RetainDefinitions(), author, source, nil, nil, nil)
	if err != nil {
		return configactivation.Result{}, err
	}
	precondition, err := configactivation.NewWorkspacePrecondition(current.generation, current.manifest, observed)
	if err != nil {
		return configactivation.Result{}, err
	}
	request, err = request.WithWorkspacePrecondition(precondition)
	if err != nil {
		return configactivation.Result{}, err
	}
	result, err := s.owner.Activate(ctx, request)
	if result.Outcome() == configactivation.RejectedBeforeCommit {
		s.dirtyMu.Lock()
		s.rejected = observed
		s.dirtyMu.Unlock()
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	_, _ = s.Refresh(refreshCtx)
	cancel()
	return result, err
}

type Watch struct {
	service      *Service
	subscription *Subscription
	activation   <-chan struct{}
	stop         func()
	snapshot     Snapshot
}

// Subscribe registers both sources before reading authority. Resume is valid
// only when that fresh authoritative cut matches the signed read token. A
// signal from 029 always forces explicit resync because coalescing loses history.
func (s *Service) Subscribe(ctx context.Context, token string) (*Watch, error) {
	if s == nil || !s.admitted || !usableContext(ctx) {
		return nil, &Error{kind: FailureInvalid}
	}
	cursor, err := s.journal.Decode(token)
	if err != nil {
		return nil, err
	}
	activation, stop, err := s.owner.SubscribeConfigChanges(ctx)
	if err != nil {
		return nil, &Error{kind: Unavailable}
	}
	sub, err := s.journal.Subscribe(cursor)
	if err != nil {
		stop()
		return nil, err
	}
	current, err := s.Read(ctx, operationalID())
	if err != nil {
		sub.Close()
		stop()
		return nil, err
	}
	if current.position.Authority() != cursor.Authority() {
		sub.Close()
		stop()
		return nil, &Resync{reason: ResyncAuthorityChanged}
	}
	select {
	case <-activation:
		sub.Close()
		stop()
		return nil, &Resync{reason: ResyncActivationSignal}
	default:
	}
	return &Watch{service: s, subscription: sub, activation: activation, stop: stop, snapshot: current}, nil
}
func (w *Watch) Close() {
	if w == nil {
		return
	}
	w.subscription.Close()
	if w.stop != nil {
		w.stop()
	}
}
func (w *Watch) Next(ctx context.Context) ([]Event, string, error) {
	if w == nil || !usableContext(ctx) {
		return nil, "", &Error{kind: FailureInvalid}
	}
	for {
		select {
		case <-w.activation:
			return nil, "", &Resync{reason: ResyncActivationSignal}
		default:
		}
		events, cursor, err := w.subscription.Read()
		if err != nil {
			return nil, "", err
		}
		if len(events) > 0 {
			current, err := w.service.Read(ctx, operationalID())
			if err != nil {
				return nil, "", err
			}
			// Publication may occur after the first select or while Read is
			// capturing authority. Never bind an old documentary event to a
			// generation whose coalesced activation history was not emitted.
			select {
			case <-w.activation:
				return nil, "", &Resync{reason: ResyncActivationSignal}
			default:
			}
			if current.generation != w.snapshot.generation || current.manifest != w.snapshot.manifest || current.catalog.SHA256() != w.snapshot.catalog.SHA256() {
				return nil, "", &Resync{reason: ResyncAuthorityChanged}
			}
			cursor, err = cursor.Bind(current.position.Authority())
			if err != nil {
				return nil, "", err
			}
			token, err := w.service.journal.Encode(cursor)
			return events, token, err
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-w.activation:
			return nil, "", &Resync{reason: ResyncActivationSignal}
		case _, ok := <-w.subscription.Changes():
			if !ok {
				return nil, "", &Resync{reason: ResyncClosed}
			}
		}
	}
}
