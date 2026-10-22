// Package configactivation coordinates exact configuration sources and SQL
// activation before any runtime composition is allowed to serve the house.
package configactivation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrInvalidStartup = errors.New("config activation: invalid startup admission")
var ErrStartupIntegrity = errors.New("config activation: active SQL generation is unavailable or inconsistent")
var ErrCommitIndeterminate = errors.New("config activation: commit outcome remains indeterminate")
var ErrStartupConflict = errors.New("config activation: active generation changed during startup")

const operationalDocumentID = "operational.main"
const bootstrapDocumentID = "bootstrap.launch"

type StartupDisposition uint8

const (
	StartupDispositionUnknown StartupDisposition = iota
	StartupInitialized
	StartupUpgraded
	StartupReplayed
	StartupBootstrapUpdated
)

// AdmittedGeneration cannot be constructed outside this package. Its source
// came from the current SQL pointer, with the protected bytes authenticated
// and the same parser used at initial admission.
type AdmittedGeneration struct {
	revision           storage.ConfigGenerationRevision
	manifest           [sha256.Size]byte
	sourceHash         [sha256.Size]byte
	operational        config.Config
	launch             config.Config
	legacy             bundle.Candidate
	compilationContext configcompilation.Context
	extraGroups        []SourceGroup
	hasLegacy          bool
	secrets            *storage.SecretRepository
	admitted           bool
}

func (g AdmittedGeneration) Valid() bool {
	return g.admitted && g.revision.Valid() && g.manifest != ([sha256.Size]byte{}) &&
		g.sourceHash != ([sha256.Size]byte{}) && g.secrets != nil && g.secrets.Valid()
}
func (g AdmittedGeneration) Revision() storage.ConfigGenerationRevision { return g.revision }
func (g AdmittedGeneration) ManifestSHA256() [sha256.Size]byte          { return g.manifest }
func (g AdmittedGeneration) SourceSHA256() [sha256.Size]byte            { return g.sourceHash }
func (g AdmittedGeneration) OperationalConfig() (config.Config, error) {
	if !g.Valid() {
		return config.Config{}, ErrInvalidStartup
	}
	return g.operational, nil
}
func (g AdmittedGeneration) LaunchConfig() (config.Config, error) {
	if !g.Valid() {
		return config.Config{}, ErrInvalidStartup
	}
	return g.launch, nil
}
func (g AdmittedGeneration) LegacyCandidate() (bundle.Candidate, bool) {
	return g.legacy, g.Valid() && g.hasLegacy
}
func (g AdmittedGeneration) Secrets() (*storage.SecretRepository, error) {
	if !g.Valid() {
		return nil, ErrInvalidStartup
	}
	return g.secrets, nil
}
func (AdmittedGeneration) String() string                 { return "[REDACTED ADMITTED GENERATION]" }
func (g AdmittedGeneration) GoString() string             { return g.String() }
func (g AdmittedGeneration) MarshalJSON() ([]byte, error) { return json.Marshal(g.String()) }

func usableContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return false
		}
	}
	return ctx.Err() == nil
}

func startupMetadata(sourceText string) (storage.ConfigVersionMetadata, error) {
	author, err := storage.NewConfigAuthor("homeward:startup")
	if err != nil {
		return storage.ConfigVersionMetadata{}, err
	}
	source, err := storage.NewConfigSource(sourceText)
	if err != nil {
		return storage.ConfigVersionMetadata{}, err
	}
	return storage.NewConfigVersionMetadata(author, source)
}

func sourceBinding(document, path string) (storage.ConfigDocumentBinding, error) {
	id, err := storage.NewConfigDocumentID(document)
	if err != nil {
		return storage.ConfigDocumentBinding{}, err
	}
	return storage.NewConfigDocumentBinding(id, path)
}

// ReconcileStartup runs after Store.Migrate and BootstrapMFAPolicy, and before
// any factory, pilot, listener or ready callback. A conflict or unknown COMMIT
// never returns an old operational plan as a fallback.
func ReconcileStartup(ctx context.Context, store *storage.Store, startup config.StartupInputs) (AdmittedGeneration, StartupDisposition, error) {
	if !usableContext(ctx) || store == nil || !startup.Valid() {
		return AdmittedGeneration{}, StartupDispositionUnknown, ErrInvalidStartup
	}
	launch, err := startup.LaunchConfig()
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	dataDir, err := launch.DataDir()
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	bootstrapSource, err := startup.BootstrapSource()
	if err != nil || !bootstrapSource.Valid() {
		return AdmittedGeneration{}, StartupDispositionUnknown, ErrInvalidStartup
	}
	state, err := store.ProbeStartupSecretState(ctx, dataDir)
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	var admission storage.SecretStartupAdmission
	var captured config.ExactWorkspaceSource
	switch state {
	case storage.SecretStartupFreshPair:
		// A discovered default sidecar is not evidence of a fresh installation.
		// The existing combined or explicit source must be admitted first.
		if startup.Origin() != config.BootstrapOriginLegacyCombined &&
			startup.Origin() != config.BootstrapOriginExplicitSidecar {
			return AdmittedGeneration{}, StartupDispositionUnknown, ErrStartupIntegrity
		}
		captured, err = startup.CaptureWorkspace()
		if err != nil {
			return AdmittedGeneration{}, StartupDispositionUnknown, err
		}
		origin := storage.SecretStartupLegacyCombined
		if startup.Origin() == config.BootstrapOriginExplicitSidecar {
			origin = storage.SecretStartupExplicitBootstrap
		}
		admission, err = storage.FreshSecretStartup(captured.SHA256(), origin)
		if err != nil {
			return AdmittedGeneration{}, StartupDispositionUnknown, err
		}
	case storage.SecretStartupEstablishedPair:
		admission = storage.RequireEstablishedSecretStartup()
	default:
		return AdmittedGeneration{}, StartupDispositionUnknown, ErrStartupIntegrity
	}
	secrets, err := store.BootstrapSecretsForStartup(ctx, dataDir, admission)
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	replay, err := storage.NewConfigReplayRepository(store, secrets)
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	active, hasActive, err := store.ReadActiveConfig(ctx)
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	bootstrapActive, hasBootstrap, err := store.ReadActiveBootstrap(ctx)
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	if hasBootstrap && bootstrapActive.Path() != bootstrapSource.Path() {
		return AdmittedGeneration{}, StartupDispositionUnknown, ErrStartupIntegrity
	}
	if !hasActive || active.ManifestFormat() == storage.ConfigManifestLegacyV1 {
		if !captured.Valid() {
			captured, err = startup.CaptureWorkspace()
			if err != nil {
				return AdmittedGeneration{}, StartupDispositionUnknown, err
			}
		}
		if err := startup.PublishDefaultBootstrapIfNeeded(); err != nil {
			return AdmittedGeneration{}, StartupDispositionUnknown, err
		}
		return activateInitialOrUpgrade(ctx, store, replay, secrets, startup, bootstrapSource,
			bootstrapActive, hasBootstrap, active, hasActive, captured, launch)
	}
	if active.ManifestFormat() != storage.ConfigManifestCompiledV2 || !hasBootstrap {
		return AdmittedGeneration{}, StartupDispositionUnknown, ErrStartupIntegrity
	}
	disposition := StartupReplayed
	if bootstrapActive.SHA256() != bootstrapSource.SHA256() {
		if err := updateBootstrapOnly(ctx, store, startup, bootstrapSource, bootstrapActive, launch); err != nil {
			return AdmittedGeneration{}, StartupDispositionUnknown, err
		}
		disposition = StartupBootstrapUpdated
	}
	admitted, err := readAdmitted(ctx, store, replay, active.Revision(), active.ManifestSHA256(), launch, secrets)
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	if err := startup.ConfirmBootstrapSource(); err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	return admitted, disposition, nil
}

func updateBootstrapOnly(ctx context.Context, store *storage.Store, startup config.StartupInputs, source config.BootstrapSourceSnapshot,
	previous storage.ConfigBootstrapSnapshot, launch config.Config) error {
	expected, err := storage.ExpectBootstrapActive(previous.Ref(), previous.SHA256())
	if err != nil {
		return err
	}
	binding, err := sourceBinding(bootstrapDocumentID, source.Path())
	if err != nil {
		return err
	}
	metadata, err := startupMetadata("startup:bootstrap-restart")
	if err != nil {
		return err
	}
	var target storage.ConfigVersionRef
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		if err := startup.ConfirmBootstrapSource(); err != nil {
			return err
		}
		var err error
		target, err = tx.SaveBootstrapLaunchSource(binding, source.Bytes(), source.SHA256(), metadata)
		if err != nil {
			return err
		}
		return tx.ActivateBootstrapSource(expected, target)
	})
	if outcome == storage.TransactionIndeterminate {
		if !target.Valid() {
			return ErrCommitIndeterminate
		}
		reconcileCtx, cancel := context.WithTimeout(context.Background(), startupReconcileBudget(launch))
		defer cancel()
		current, found, readErr := store.ReadActiveBootstrapAfterWriter(reconcileCtx)
		if readErr != nil || !found || current.Ref() != target ||
			current.SHA256() != source.SHA256() || current.Path() != source.Path() {
			return ErrCommitIndeterminate
		}
		if ctx.Err() != nil {
			return ErrCommitIndeterminate
		}
		return nil
	}
	if outcome != storage.TransactionCommitted || err != nil {
		return fmt.Errorf("%w: bootstrap CAS: %v", ErrStartupConflict, err)
	}
	current, found, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !found || current.Ref() != target ||
		current.SHA256() != source.SHA256() || current.Path() != source.Path() {
		return ErrStartupIntegrity
	}
	return nil
}

func startupReconcileBudget(launch config.Config) time.Duration {
	if public, err := launch.PublicTransport(); err == nil && public.ShutdownTimeout() > 0 {
		return public.ShutdownTimeout()
	}
	return 6 * time.Second
}

func activateInitialOrUpgrade(ctx context.Context, store *storage.Store, replay *storage.ConfigReplayRepository,
	secrets *storage.SecretRepository, startup config.StartupInputs, bootstrapSource config.BootstrapSourceSnapshot,
	previousBootstrap storage.ConfigBootstrapSnapshot, hasBootstrap bool,
	previous storage.ConfigGenerationSnapshot, hasPrevious bool,
	source config.ExactWorkspaceSource, launch config.Config) (AdmittedGeneration, StartupDisposition, error) {
	if !source.Valid() {
		return AdmittedGeneration{}, StartupDispositionUnknown, ErrInvalidStartup
	}
	var legacy bundle.Candidate
	var legacyRefs []storage.ConfigVersionRef
	var expected storage.ExpectedConfigActive
	disposition := StartupInitialized
	if hasPrevious {
		if previous.ManifestFormat() != storage.ConfigManifestLegacyV1 ||
			previous.Availability() != storage.ConfigGenerationComplete {
			return AdmittedGeneration{}, StartupDispositionUnknown, ErrStartupIntegrity
		}
		var err error
		legacy, err = previous.Restore()
		if err != nil {
			return AdmittedGeneration{}, StartupDispositionUnknown, err
		}
		for _, version := range previous.Versions() {
			legacyRefs = append(legacyRefs, version.Ref())
		}
		expected, err = storage.ExpectConfigActive(previous.Revision(), previous.ManifestSHA256())
		if err != nil {
			return AdmittedGeneration{}, StartupDispositionUnknown, err
		}
		disposition = StartupUpgraded
	} else {
		expected = storage.ExpectNoConfigActive()
	}
	bootstrapExpected := storage.ExpectNoBootstrapActive()
	if hasBootstrap {
		var err error
		bootstrapExpected, err = storage.ExpectBootstrapActive(previousBootstrap.Ref(), previousBootstrap.SHA256())
		if err != nil {
			return AdmittedGeneration{}, StartupDispositionUnknown, err
		}
	}
	metadata, err := startupMetadata("startup:operational")
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	bootstrapBinding, err := sourceBinding(bootstrapDocumentID, bootstrapSource.Path())
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	operationalBinding, err := sourceBinding(operationalDocumentID, source.LogicalPath())
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	compiled := compiledOperationalHash(source.SHA256(), legacy)
	entry := source.LogicalPath()
	if legacy.Valid() {
		entry = legacy.Entry()
	}
	attemptID, err := storage.RandomActivationAttemptID()
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	var target storage.ConfigGenerationRevision
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		if err := startup.ConfirmBootstrapSource(); err != nil {
			return err
		}
		if _, physical := source.PhysicalPath(); physical {
			if err := source.ConfirmPhysicalSource(); err != nil {
				return err
			}
		}
		if !hasBootstrap || previousBootstrap.SHA256() != bootstrapSource.SHA256() {
			bootstrapRef, err := tx.SaveBootstrapLaunchSource(bootstrapBinding, bootstrapSource.Bytes(),
				bootstrapSource.SHA256(), metadata)
			if err != nil {
				return err
			}
			if err := tx.ActivateBootstrapSource(bootstrapExpected, bootstrapRef); err != nil {
				return err
			}
		}
		operationalRef, err := tx.SaveProtectedOperationalSource(replay, operationalBinding,
			source.Bytes(), source.SHA256(), metadata)
		if err != nil {
			return err
		}
		refs := append(append([]storage.ConfigVersionRef(nil), legacyRefs...), operationalRef)
		target, err = tx.RegisterCompiledGeneration(entry, compiled, refs)
		if err != nil {
			return err
		}
		if err := tx.ActivateGeneration(expected, target); err != nil {
			return err
		}
		return tx.RecordActivationAttempt(attemptID, metadata)
	})
	if outcome == storage.TransactionIndeterminate {
		// The independent reader waits for the writer's SQL lock. Even a proven
		// absence is not authorization to replay this activation or dispatch.
		reconcileCtx, cancel := context.WithTimeout(context.Background(), startupReconcileBudget(launch))
		defer cancel()
		receipt, found, readErr := store.ReadActivationAttempt(reconcileCtx, attemptID)
		if readErr != nil || !found || receipt.Target() != target {
			return AdmittedGeneration{}, StartupDispositionUnknown, ErrCommitIndeterminate
		}
		active, present, readErr := store.ReadActiveConfig(reconcileCtx)
		if readErr != nil || !present || active.Revision() != target ||
			active.ManifestSHA256() != receipt.ManifestSHA256() {
			return AdmittedGeneration{}, StartupDispositionUnknown, ErrCommitIndeterminate
		}
		if ctx.Err() != nil {
			return AdmittedGeneration{}, StartupDispositionUnknown, ErrCommitIndeterminate
		}
	} else if outcome != storage.TransactionCommitted || err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, fmt.Errorf("%w: %v", ErrStartupConflict, err)
	}
	admitted, err := readAdmitted(ctx, store, replay, target, [sha256.Size]byte{}, launch, secrets)
	if err != nil {
		return AdmittedGeneration{}, StartupDispositionUnknown, err
	}
	return admitted, disposition, nil
}

func compiledOperationalHash(source [sha256.Size]byte, legacy bundle.Candidate) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("homeward.compiled-operational/1\x00"))
	_, _ = h.Write(source[:])
	if legacy.Valid() {
		_, _ = h.Write([]byte{1})
		legacyHash := legacy.SHA256()
		_, _ = h.Write(legacyHash[:])
	} else {
		_, _ = h.Write([]byte{0})
	}
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}

func readAdmitted(ctx context.Context, store *storage.Store, replay *storage.ConfigReplayRepository,
	revision storage.ConfigGenerationRevision, wantedManifest [sha256.Size]byte, launch config.Config,
	secrets *storage.SecretRepository) (AdmittedGeneration, error) {
	active, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || active.ManifestFormat() != storage.ConfigManifestCompiledV2 ||
		active.Availability() != storage.ConfigGenerationComplete || active.Revision() != revision ||
		(wantedManifest != ([sha256.Size]byte{}) && active.ManifestSHA256() != wantedManifest) {
		return AdmittedGeneration{}, ErrStartupIntegrity
	}
	var operationalRef storage.ConfigVersionRef
	var operationalHash [sha256.Size]byte
	var legacySources []parser.SourceFile
	var legacyProfile parser.SyntaxProfile
	hasEditableSources := false
	for _, version := range active.Versions() {
		kind, present := version.Kind()
		if !present {
			return AdmittedGeneration{}, ErrStartupIntegrity
		}
		switch kind {
		case storage.ConfigDocumentKindOperationalYAML:
			if operationalRef.Valid() {
				return AdmittedGeneration{}, ErrStartupIntegrity
			}
			operationalRef, operationalHash = version.Ref(), version.SHA256()
		case storage.ConfigDocumentKindDashboardSource, storage.ConfigDocumentKindAutomationSource, storage.ConfigDocumentKindScriptSource, storage.ConfigDocumentKindSceneSource, storage.ConfigDocumentKindOwnershipSource:
			hasEditableSources = true
		case storage.ConfigDocumentKindYAMLSource:
			profile, ok := version.Profile()
			raw, hasBytes := version.Bytes()
			if !ok || !hasBytes || (legacyProfile != parser.SyntaxProfileInvalid && legacyProfile != profile) {
				return AdmittedGeneration{}, ErrStartupIntegrity
			}
			legacyProfile = profile
			file, err := parser.NewSourceFile(version.Path(), raw)
			if err != nil || file.SHA256() != version.SHA256() {
				return AdmittedGeneration{}, ErrStartupIntegrity
			}
			legacySources = append(legacySources, file)
		default:
			return AdmittedGeneration{}, ErrStartupIntegrity
		}
	}
	if !operationalRef.Valid() {
		return AdmittedGeneration{}, ErrStartupIntegrity
	}
	protected, err := replay.ReadActiveProtectedSource(ctx, active.Revision(), active.ManifestSHA256(), operationalRef)
	if err != nil || protected.SHA256() != operationalHash {
		return AdmittedGeneration{}, ErrStartupIntegrity
	}
	operational, err := config.ParseOperationalWithEnv(protected.Bytes(), func(string) (string, bool) { return "", false })
	if err != nil {
		return AdmittedGeneration{}, ErrStartupIntegrity
	}
	var legacy bundle.Candidate
	if len(legacySources) > 0 {
		switch legacyProfile {
		case parser.SyntaxProfileDeclarative:
			legacy, err = bundle.CompileSources(active.Entry(), legacySources)
		case parser.SyntaxProfileDeclarative2:
			legacy, err = bundle.CompileSourcesV2(active.Entry(), legacySources)
		default:
			return AdmittedGeneration{}, ErrStartupIntegrity
		}
		if err != nil || !legacy.Valid() {
			return AdmittedGeneration{}, ErrStartupIntegrity
		}
	}
	var compilationContext configcompilation.Context
	var extraGroups []SourceGroup
	if hasEditableSources {
		sources, err := store.ReadEditableSources(ctx, active.Revision(), active.ManifestSHA256())
		if err != nil {
			return AdmittedGeneration{}, ErrStartupIntegrity
		}
		compilationContext, err = store.ReadConfigCompilationContext(ctx, active.Revision(), active.ManifestSHA256(), active.CandidateSHA256())
		if err != nil {
			return AdmittedGeneration{}, ErrStartupIntegrity
		}
		extraGroups, err = sourceGroupsFromSQL(sources)
		if err != nil {
			return AdmittedGeneration{}, ErrStartupIntegrity
		}
	}
	if active.CandidateSHA256() != compiledSourceGroupsHash(compiledOperationalHash(protected.SHA256(), legacy), extraGroups) {
		return AdmittedGeneration{}, ErrStartupIntegrity
	}
	return AdmittedGeneration{revision: active.Revision(), manifest: active.ManifestSHA256(),
		sourceHash: protected.SHA256(), operational: operational, launch: launch,
		legacy: legacy, compilationContext: compilationContext, extraGroups: extraGroups, hasLegacy: legacy.Valid(), secrets: secrets, admitted: true}, nil
}
