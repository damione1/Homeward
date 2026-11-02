package configactivation

import (
	"context"
	"sort"
	"time"

	"github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/view"
)

// CurrentDashboards captures the exact published source generation. Callers
// recheck its stamp after authorization/projection; editable workspace bytes
// are never opened by a dashboard read.
func (o *Owner) CurrentDashboards(ctx context.Context) ([]dashboard.Definition, view.ConfigStamp, error) {
	if o == nil || !usableContext(ctx) {
		return nil, view.ConfigStamp{}, ErrInvalidStartup
	}
	o.mu.RLock()
	current, phase := o.current, o.phase
	o.mu.RUnlock()
	if phase != ownerOpen || !current.Valid() {
		return nil, view.ConfigStamp{}, ErrActivationUnavailable
	}
	stamp, err := view.NewConfigStamp(uint64(current.Revision().Number()), current.ManifestSHA256())
	if err != nil {
		return nil, view.ConfigStamp{}, err
	}
	if len(current.extraGroups) == 0 {
		return nil, stamp, nil
	}
	if !current.compilationContext.Valid() {
		return nil, view.ConfigStamp{}, ErrActivationUnavailable
	}
	compiler, err := o.sourceCompiler(ctx, current.compilationContext, current.operational)
	if err != nil {
		return nil, view.ConfigStamp{}, err
	}
	compiled, err := CompileSourceGroups(current.extraGroups, compiler.manifest, compiler.catalogue)
	if err != nil {
		return nil, view.ConfigStamp{}, err
	}
	definitions := make([]dashboard.Definition, 0, len(compiled.dashboards))
	for _, definition := range compiled.dashboards {
		definitions = append(definitions, definition)
	}
	sort.Slice(definitions, func(i, j int) bool {
		left, _ := definitions[i].Source().ID().Value()
		right, _ := definitions[j].Source().ID().Value()
		return left < right
	})
	return definitions, stamp, nil
}

type DashboardSourceVersion struct {
	Revision     int64
	SourceSHA256 [32]byte
	UpdatedAt    time.Time
}

// DashboardSourceVersions reads exact entry-document versions from the same
// retained generation, rather than inventing a source revision or timestamp.
func (o *Owner) DashboardSourceVersions(ctx context.Context, stamp view.ConfigStamp) (map[authorization.DashboardID]DashboardSourceVersion, error) {
	if o == nil || !usableContext(ctx) || o.input.Store == nil {
		return nil, ErrInvalidStartup
	}
	o.mu.RLock()
	current := o.current
	o.mu.RUnlock()
	number, hash, err := stamp.Components()
	if err != nil {
		return nil, err
	}
	if uint64(current.Revision().Number()) != number || current.ManifestSHA256() != hash {
		return nil, ErrActivationUnavailable
	}
	revision, err := storage.NewConfigGenerationRevision(int64(number))
	if err != nil {
		return nil, err
	}
	generation, err := o.input.Store.ReadConfigGeneration(ctx, revision)
	if err != nil {
		return nil, err
	}
	if generation.ManifestSHA256() != hash {
		return nil, ErrActivationUnavailable
	}
	versions := make(map[storage.ConfigDocumentID]storage.ConfigVersionSnapshot)
	for _, version := range generation.Versions() {
		versions[version.Ref().Document()] = version
	}
	result := make(map[authorization.DashboardID]DashboardSourceVersion)
	for _, group := range current.extraGroups {
		if group.descriptor.Kind() != storage.ConfigDocumentKindDashboardSource {
			continue
		}
		id, err := authorization.ParseDashboardID(group.descriptor.Identity())
		if err != nil {
			return nil, err
		}
		for _, binding := range group.bindings {
			if binding.Path() != group.descriptor.Entry() {
				continue
			}
			version, present := versions[binding.ID()]
			if !present || !version.Present() {
				return nil, ErrActivationUnavailable
			}
			updated, present := version.AcceptedAt()
			if !present {
				return nil, ErrActivationUnavailable
			}
			result[id] = DashboardSourceVersion{Revision: version.Ref().Revision().Number(), SourceSHA256: version.SHA256(), UpdatedAt: updated}
		}
	}
	return result, nil
}
