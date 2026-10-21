package report

import (
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

type scopeConsumer struct {
	document    scope.EcobeeConsumer
	disposition logicselection.Disposition
}

type scopeSummary struct {
	revision  scope.Revision
	baseline  nha047.Digest
	counts    registry.ManifestCounts
	amendment scope.EcobeeAmendment
	consumers []scopeConsumer
}

func buildScopeSummary(manifest registry.Manifest, selection logicselection.Selection) (scopeSummary, error) {
	revision, ok := manifest.Revision()
	if !ok {
		return scopeSummary{}, ErrInvalidInput
	}
	baseline, ok := manifest.BaselineDigest()
	if !ok {
		return scopeSummary{}, ErrInvalidInput
	}
	counts, ok := manifest.Counts()
	if !ok {
		return scopeSummary{}, ErrInvalidInput
	}
	result := scopeSummary{revision: revision, baseline: nha047.Digest(baseline), counts: counts}
	amendment, present := manifest.ScopeAmendment()
	if !present {
		return result, nil
	}
	result.amendment = amendment
	for _, consumer := range amendment.Consumers() {
		id, err := domain.ParseHAAlias(consumer.EntityID)
		if err != nil {
			return scopeSummary{}, ErrInvalidInput
		}
		disposition, known := selection.DispositionFor(id)
		if !known || (disposition != logicselection.DispositionRetained && disposition != logicselection.DispositionDeferred) {
			return scopeSummary{}, ErrInvalidInput
		}
		for _, via := range consumer.Via {
			alias, err := domain.ParseHAAlias(via)
			if err != nil {
				return scopeSummary{}, ErrInvalidInput
			}
			if _, known := selection.DispositionFor(alias); !known {
				return scopeSummary{}, ErrInvalidInput
			}
		}
		result.consumers = append(result.consumers, scopeConsumer{consumer, disposition})
	}
	return result, nil
}

func (r Report) ScopeRevision() (scope.Revision, bool) {
	_, err := r.scopeSummary.revision.Value()
	return r.scopeSummary.revision, r.valid && err == nil
}
func (r Report) ScopeCounts() (registry.ManifestCounts, bool) {
	_, ok := r.ScopeRevision()
	return r.scopeSummary.counts, ok
}

type scopeConsumerWire struct {
	ID                   string                  `json:"entity_id"`
	Via                  []string                `json:"via"`
	Disposition          string                  `json:"disposition"`
	Status               string                  `json:"status"`
	Active               bool                    `json:"active"`
	DisabledActionInPath bool                    `json:"disabled_action_in_path"`
	Source               scope.DocumentarySource `json:"source"`
}

type scopeTechnicalWire struct {
	ID     string                  `json:"entity_id"`
	Via    []string                `json:"via"`
	Ticket string                  `json:"ticket"`
	Status string                  `json:"status"`
	Source scope.DocumentarySource `json:"source"`
}

type scopeWire struct {
	Revision               string                     `json:"revision"`
	BaselineRevision       string                     `json:"baseline_revision"`
	BaselineManifestSHA256 string                     `json:"baseline_manifest_sha256"`
	BaselineIDsSHA256      string                     `json:"baseline_ids_sha256"`
	EffectiveIDsSHA256     string                     `json:"effective_ids_sha256"`
	Entities               int                        `json:"entity_count"`
	Sources                int                        `json:"source_count"`
	Diagnostics            int                        `json:"diagnostic_count"`
	Added                  []string                   `json:"added_ids"`
	Removed                []string                   `json:"removed_ids"`
	Aliases                []string                   `json:"additional_aliases"`
	AmendmentSHA256        string                     `json:"amendment_sha256,omitempty"`
	Decision               *scope.DocumentarySource   `json:"decision,omitempty"`
	Qualification          *scope.EcobeeQualification `json:"qualification,omitempty"`
	ThermostatService      string                     `json:"thermostat_service,omitempty"`
	LinkedFanServices      []string                   `json:"linked_fan_services"`
	Fields                 []scope.EcobeeField        `json:"qualified_hap_fields"`
	Limits                 []string                   `json:"profile_limits"`
	Consumers              []scopeConsumerWire        `json:"logic_consumers"`
	Technical              []scopeTechnicalWire       `json:"technical_consumers"`
}

func (r Report) scopeWire() (scopeWire, error) {
	s := r.scopeSummary
	revision, err := s.revision.Value()
	if err != nil {
		return scopeWire{}, ErrInvalidReport
	}
	w := scopeWire{Revision: revision, BaselineRevision: "v1", BaselineManifestSHA256: scope.BaselineManifestSHA256, BaselineIDsSHA256: s.baseline.String(), EffectiveIDsSHA256: r.manifest.String(), Entities: s.counts.Entities, Sources: s.counts.Sources, Diagnostics: s.counts.Diagnostics, Added: []string{}, Removed: []string{}, Aliases: []string{}, LinkedFanServices: []string{}, Fields: []scope.EcobeeField{}, Limits: []string{}, Consumers: []scopeConsumerWire{}, Technical: []scopeTechnicalWire{}}
	if !s.amendment.Valid() {
		return w, nil
	}
	digest, ok := s.amendment.Digest()
	if !ok {
		return scopeWire{}, ErrInvalidReport
	}
	w.AmendmentSHA256 = nha047.Digest(digest).String()
	decision, qualification := s.amendment.Decision(), s.amendment.Qualification()
	w.Decision, w.Qualification = &decision, &qualification
	if s.revision == scope.EcobeeMVPV1() {
		w.Added, w.Removed, w.Aliases = s.amendment.AddedIDs(), s.amendment.RemovedIDs(), s.amendment.AdditionalAliases()
		w.Fields = s.amendment.Fields()
		w.ThermostatService, _ = s.amendment.ThermostatService()
		w.LinkedFanServices = s.amendment.LinkedFanServices()
	}
	w.Limits = s.amendment.Limits()
	for _, consumer := range s.consumers {
		disposition := ""
		switch consumer.disposition {
		case logicselection.DispositionRetained:
			disposition = "retained"
		case logicselection.DispositionDeferred:
			disposition = "deferred"
		case logicselection.DispositionUnspecified:
			return scopeWire{}, ErrInvalidReport
		default:
			return scopeWire{}, ErrInvalidReport
		}
		doc := consumer.document
		w.Consumers = append(w.Consumers, scopeConsumerWire{ID: doc.EntityID, Via: doc.Via, Disposition: disposition, Status: "requires_remapping_and_qualification", Active: false, DisabledActionInPath: doc.DisabledActionInPath, Source: doc.Source})
	}
	for _, consumer := range s.amendment.TechnicalConsumers() {
		w.Technical = append(w.Technical, scopeTechnicalWire{ID: consumer.EntityID, Via: consumer.Via, Ticket: consumer.Ticket, Status: "requires_remapping_and_qualification", Source: consumer.Source})
	}
	return w, nil
}
