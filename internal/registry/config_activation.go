package registry

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

// ConfigWriter exposes only the existing registry mutations to one borrowed
// ConfigTx. The callback has no SQL transaction handle, Commit or Rollback.
// Every returned error poisons the enclosing ConfigTx even if its caller
// discards it; an escaped writer is invalid after ApplyInConfigTx returns.
type ConfigWriter struct {
	repo    *Repository
	sql     registryStatements
	live    bool
	failure error
}

func (w *ConfigWriter) valid() error {
	if w == nil || !w.live || w.repo == nil || w.sql == nil {
		return ErrInvalidInput
	}
	return w.failure
}

func (w *ConfigWriter) remember(err error) error {
	if w != nil && err != nil && w.failure == nil {
		w.failure = err
	}
	return err
}

func (w *ConfigWriter) ConfigureBinding(instance domain.IntegrationInstanceID, key integrations.SourceKey, originalAlias string) (integrations.ResolvedIdentity, error) {
	var result integrations.ResolvedIdentity
	if err := w.valid(); err != nil {
		return result, err
	}
	instanceText, sourceDevice, sourceEntity, err := sourceParts(instance, key)
	alias, aliasErr := ParseHAAlias(originalAlias)
	if err != nil || aliasErr != nil {
		return result, w.remember(ErrInvalidInput)
	}
	if !w.repo.manifest.permits(alias) {
		return result, w.remember(ErrOutsideManifest)
	}
	err = configureBindingSQL(w.sql, instanceText, sourceDevice, sourceEntity, originalAlias, &result)
	return result, w.remember(err)
}

func (w *ConfigWriter) Rename(entity domain.EntityID, alias string) error {
	if err := w.valid(); err != nil {
		return err
	}
	entityText, err := entity.Value()
	parsedAlias, aliasErr := ParseHAAlias(alias)
	if err != nil || aliasErr != nil {
		return w.remember(ErrInvalidInput)
	}
	return w.remember(renameSQL(w.sql, w.repo.manifest, entityText, parsedAlias, alias))
}

func (w *ConfigWriter) Tombstone(entity domain.EntityID) error {
	if err := w.valid(); err != nil {
		return err
	}
	entityText, err := entity.Value()
	if err != nil {
		return w.remember(ErrInvalidInput)
	}
	return w.remember(tombstoneSQL(w.sql, entityText))
}

// ApplyInConfigTx runs behind BeginActivation's repository exclusion and the
// same Store's SQL locks 151 then 15. It makes no publication before COMMIT.
func (g *ActivationGate) ApplyInConfigTx(config *storage.ConfigTx, fn func(*ConfigWriter) error) error {
	if g == nil || g.repo == nil || config == nil || fn == nil ||
		!g.phase.CompareAndSwap(uint32(activationHeld), uint32(activationApplying)) {
		return ErrInvalidInput
	}
	// Even if the callback fails or panics, only a definite transaction outcome
	// can resolve the pending projection obligation.
	defer g.phase.Store(uint32(activationPending))
	return classify(config.WithRegistryBorrow(g.repo.store, func(borrow *storage.RegistryBorrow) error {
		writer := &ConfigWriter{repo: g.repo, sql: borrow, live: true}
		defer func() { writer.live = false }()
		if err := fn(writer); err != nil {
			return err
		}
		return writer.failure
	}))
}

// PublishCommitted loads the committed registry image behind the same gate.
// A failed load or sink publication leaves its projection explicitly dirty;
// the activation owner must retain the gate and reconcile before Release.
func (g *ActivationGate) PublishCommitted(ctx context.Context, target storage.ConfigGenerationRevision, manifest [sha256.Size]byte) error {
	if g == nil || g.repo == nil || nilContext(ctx) || !target.Valid() || manifest == ([sha256.Size]byte{}) ||
		!g.phase.CompareAndSwap(uint32(activationPending), uint32(activationPublishing)) {
		return ErrInvalidInput
	}
	defer g.phase.CompareAndSwap(uint32(activationPublishing), uint32(activationPending))
	r := g.repo
	r.mu.RLock()
	sink := r.projection
	r.mu.RUnlock()
	if sink == nil {
		return ErrInvalidInput
	}
	background, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	active, present, err := r.store.ReadActiveConfig(background)
	if err != nil || !present || active.Revision() != target || active.ManifestSHA256() != manifest ||
		active.Availability() != storage.ConfigGenerationComplete {
		r.mu.Lock()
		r.projectionDirty = true
		r.mu.Unlock()
		sink.InvalidateRegistry()
		return ErrProjectionUnavailable
	}
	records, err := r.listUnlocked(background)
	if err == nil {
		err = sink.PublishRegistry(background, records)
	}
	if err != nil {
		r.mu.Lock()
		r.projectionDirty = true
		r.mu.Unlock()
		sink.InvalidateRegistry()
		return err
	}
	r.mu.Lock()
	r.projectionDirty = false
	r.mu.Unlock()
	g.phase.Store(uint32(activationResolved))
	return nil
}

// CommittedRecords reads the exact target behind the retained activation
// gate, before selective transfer or projection. It does not publish or
// resolve the gate and cannot be used to reopen an unknown SQL outcome.
func (g *ActivationGate) CommittedRecords(ctx context.Context, target storage.ConfigGenerationRevision, manifest [sha256.Size]byte) ([]integrations.RegistryRecord, error) {
	if g == nil || g.repo == nil || nilContext(ctx) || !target.Valid() || manifest == ([sha256.Size]byte{}) {
		return nil, ErrInvalidInput
	}
	phase := activationPhase(g.phase.Load())
	if phase != activationPending && phase != activationResolved {
		return nil, ErrInvalidInput
	}
	active, present, err := g.repo.store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != target || active.ManifestSHA256() != manifest || active.Availability() != storage.ConfigGenerationComplete {
		return nil, ErrProjectionUnavailable
	}
	return g.repo.listUnlocked(ctx)
}

// RetargetCommitted keeps the same exclusion while reconciliation follows a
// newer SQL winner. It performs no mutation and cannot release old authority.
func (g *ActivationGate) RetargetCommitted(ctx context.Context, target storage.ConfigGenerationRevision, manifest [sha256.Size]byte) error {
	if g == nil || g.repo == nil || nilContext(ctx) || !target.Valid() || manifest == ([sha256.Size]byte{}) {
		return ErrInvalidInput
	}
	active, present, err := g.repo.store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Revision() != target || active.ManifestSHA256() != manifest || active.Availability() != storage.ConfigGenerationComplete {
		return ErrProjectionUnavailable
	}
	phase := activationPhase(g.phase.Load())
	if phase == activationPending {
		return nil
	}
	if phase != activationResolved || !g.phase.CompareAndSwap(uint32(activationResolved), uint32(activationPending)) {
		return ErrInvalidInput
	}
	return nil
}
