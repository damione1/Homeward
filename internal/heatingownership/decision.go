// Package heatingownership admits the versioned Hilo/Neviweb decision corpus.
// The registry remains the identity authority and the Host remains the live
// command authority; this package neither binds sources nor dispatches actions.
package heatingownership

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

var ErrInvalidDecision = errors.New("invalid heating ownership decision")
var ErrOutsideDecision = errors.New("alias is outside the Hilo/Neviweb decision")

type Provider uint8

const (
	ProviderUnspecified Provider = iota
	ProviderHilo
	ProviderNeviweb
)

type CandidateKind uint8

const (
	CandidateUnspecified CandidateKind = iota
	CandidateThermostat
	CandidateWaterHeater
)

type Relation uint8

const (
	RelationUnspecified Relation = iota
	RelationUnproven
	RelationCorroborated
)

type EnergyRelation uint8

const (
	EnergyUnspecified EnergyRelation = iota
	EnergyIndependent
)

type ActionPolicy uint8

const (
	ActionUnspecified ActionPolicy = iota
	ActionDenied
)

// Pair records a candidate relationship, not a merged registry identity.
type Pair struct {
	hilo       domain.HAAlias
	neviweb    domain.HAAlias
	kind       CandidateKind
	relation   Relation
	proof      *PhysicalProof
	sourceRefs []string
}

func (p Pair) Aliases() (domain.HAAlias, domain.HAAlias) { return p.hilo, p.neviweb }
func (p Pair) Kind() CandidateKind                       { return p.kind }
func (p Pair) Relation() Relation                        { return p.relation }
func (p Pair) SourceRefs() []string                      { return append([]string(nil), p.sourceRefs...) }

// HardwareObservation carries one independently sourced assertion of a
// manufacturer-global serial. Text fields are open IDs admitted by Parse.
type HardwareObservation struct {
	manufacturer string
	model        string
	serial       string
	sourceRef    string
}

func (o HardwareObservation) Identity() (manufacturer, model, serial string) {
	return o.manufacturer, o.model, o.serial
}
func (o HardwareObservation) SourceRef() string { return o.sourceRef }

type PhysicalProof struct {
	hilo    HardwareObservation
	neviweb HardwareObservation
}

func (p PhysicalProof) Observations() (HardwareObservation, HardwareObservation) {
	return p.hilo, p.neviweb
}

func (p Pair) PhysicalProof() (PhysicalProof, bool) {
	if p.proof == nil {
		return PhysicalProof{}, false
	}
	return *p.proof, true
}

// Decision is a validated, immutable view of source evidence and MVP policy.
// A zero Decision grants no candidate admission.
type Decision struct {
	manifest   registry.Manifest
	pairs      []Pair
	byAlias    map[domain.HAAlias]int
	hiloAction ActionPolicy
	heatOwner  Provider
	energy     EnergyRelation
	valid      bool
}

type rawDecision struct {
	Version           int       `json:"version"`
	ManifestIDsSHA256 string    `json:"manifest_ids_sha256"`
	Policy            rawPolicy `json:"policy"`
	Candidates        []rawPair `json:"candidates"`
}

type rawPolicy struct {
	HiloMVPActions string `json:"hilo_mvp_actions"`
	HeatingOwner   string `json:"heating_owner"`
	EnergyRelation string `json:"energy_relation"`
}

type rawPair struct {
	Kind         string      `json:"kind"`
	HiloAlias    string      `json:"hilo_alias"`
	NeviwebAlias string      `json:"neviweb_alias"`
	Relation     rawRelation `json:"relation"`
	SourceRefs   []string    `json:"source_refs"`
}

type rawRelation struct {
	Kind    string       `json:"kind"`
	Reason  string       `json:"reason,omitempty"`
	Hilo    *rawHardware `json:"hilo,omitempty"`
	Neviweb *rawHardware `json:"neviweb,omitempty"`
}

type rawHardware struct {
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Serial       string `json:"serial"`
	SerialScope  string `json:"serial_scope"`
	SourceRef    string `json:"source_ref"`
}

// LoadV1 reads both immutable, separately versioned scope assets. No network,
// provider credentials, SQL, or real equipment is consulted.
func LoadV1() (Decision, error) {
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		return Decision{}, err
	}
	return Parse(scope.HeatingOwnershipV1(), manifest)
}

// Parse requires a manifest with verified source provenance and ID digest.
// The two source names are converted at this boundary to the closed Provider
// role; unrelated manifest sources remain an open set.
func Parse(data []byte, manifest registry.Manifest) (Decision, error) {
	manifestDigest, ok := manifest.Digest()
	if !ok {
		return Decision{}, ErrInvalidDecision
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw rawDecision
	if err := decoder.Decode(&raw); err != nil {
		return Decision{}, ErrInvalidDecision
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Decision{}, ErrInvalidDecision
	}
	digest, err := hex.DecodeString(raw.ManifestIDsSHA256)
	if err != nil || len(digest) != sha256.Size || !bytes.Equal(digest, manifestDigest[:]) || raw.Version != 1 || len(raw.Candidates) != 9 {
		return Decision{}, ErrInvalidDecision
	}
	if raw.Policy.HiloMVPActions != "denied" || raw.Policy.HeatingOwner != "neviweb130" || raw.Policy.EnergyRelation != "independent" {
		return Decision{}, ErrInvalidDecision
	}
	decision := Decision{
		manifest: manifest, pairs: make([]Pair, 0, len(raw.Candidates)),
		byAlias:    make(map[domain.HAAlias]int, len(raw.Candidates)*2),
		hiloAction: ActionDenied, heatOwner: ProviderNeviweb, energy: EnergyIndependent,
	}
	thermostats, waterHeaters := 0, 0
	for _, candidate := range raw.Candidates {
		pair, err := parsePair(candidate, manifest)
		if err != nil {
			return Decision{}, ErrInvalidDecision
		}
		if _, exists := decision.byAlias[pair.hilo]; exists {
			return Decision{}, ErrInvalidDecision
		}
		if _, exists := decision.byAlias[pair.neviweb]; exists {
			return Decision{}, ErrInvalidDecision
		}
		switch pair.kind {
		case CandidateThermostat:
			thermostats++
		case CandidateWaterHeater:
			waterHeaters++
		default:
			return Decision{}, ErrInvalidDecision
		}
		index := len(decision.pairs)
		decision.byAlias[pair.hilo], decision.byAlias[pair.neviweb] = index, index
		decision.pairs = append(decision.pairs, pair)
	}
	if thermostats != 8 || waterHeaters != 1 {
		return Decision{}, ErrInvalidDecision
	}
	decision.valid = true
	return decision, nil
}

func parsePair(raw rawPair, manifest registry.Manifest) (Pair, error) {
	hilo, err := domain.ParseHAAlias(raw.HiloAlias)
	if err != nil {
		return Pair{}, ErrInvalidDecision
	}
	neviweb, err := domain.ParseHAAlias(raw.NeviwebAlias)
	if err != nil || hilo == neviweb || !hasSource(manifest, hilo, "hilo") || !hasSource(manifest, neviweb, "neviweb130") {
		return Pair{}, ErrInvalidDecision
	}
	var kind CandidateKind
	switch raw.Kind {
	case "thermostat":
		kind = CandidateThermostat
		if !strings.HasPrefix(raw.HiloAlias, "climate.") || !strings.HasPrefix(raw.NeviwebAlias, "climate.") {
			return Pair{}, ErrInvalidDecision
		}
	case "water_heater":
		kind = CandidateWaterHeater
		if !strings.HasPrefix(raw.HiloAlias, "switch.") || !strings.HasPrefix(raw.NeviwebAlias, "switch.") {
			return Pair{}, ErrInvalidDecision
		}
	default:
		return Pair{}, ErrInvalidDecision
	}
	relation, proof, err := parseRelation(raw.Relation)
	if err != nil || len(raw.SourceRefs) == 0 {
		return Pair{}, ErrInvalidDecision
	}
	refs := make([]string, 0, len(raw.SourceRefs))
	seen := make(map[string]bool, len(raw.SourceRefs))
	for _, ref := range raw.SourceRefs {
		if !validText(ref) || strings.HasPrefix(ref, "/") || strings.Contains(ref, "..") || seen[ref] {
			return Pair{}, ErrInvalidDecision
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return Pair{hilo: hilo, neviweb: neviweb, kind: kind, relation: relation, proof: proof, sourceRefs: refs}, nil
}

func parseRelation(raw rawRelation) (Relation, *PhysicalProof, error) {
	switch raw.Kind {
	case "unproven":
		if raw.Reason != "neviweb_stable_identity_absent" || raw.Hilo != nil || raw.Neviweb != nil {
			return RelationUnspecified, nil, ErrInvalidDecision
		}
		return RelationUnproven, nil, nil
	case "corroborated_hardware_identity":
		if raw.Reason != "" || raw.Hilo == nil || raw.Neviweb == nil || !validHardware(*raw.Hilo) || !validHardware(*raw.Neviweb) {
			return RelationUnspecified, nil, ErrInvalidDecision
		}
		hilo, neviweb := raw.Hilo, raw.Neviweb
		if hilo.Manufacturer != neviweb.Manufacturer || hilo.Model != neviweb.Model || hilo.Serial != neviweb.Serial || hilo.SourceRef == neviweb.SourceRef {
			return RelationUnspecified, nil, ErrInvalidDecision
		}
		proof := &PhysicalProof{
			hilo:    HardwareObservation{manufacturer: hilo.Manufacturer, model: hilo.Model, serial: hilo.Serial, sourceRef: hilo.SourceRef},
			neviweb: HardwareObservation{manufacturer: neviweb.Manufacturer, model: neviweb.Model, serial: neviweb.Serial, sourceRef: neviweb.SourceRef},
		}
		return RelationCorroborated, proof, nil
	default:
		return RelationUnspecified, nil, ErrInvalidDecision
	}
}

func validText(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

func validHardware(h rawHardware) bool {
	return validText(h.Manufacturer) && validText(h.Model) && validText(h.Serial) && validText(h.SourceRef) &&
		h.SerialScope == "manufacturer_global"
}

func hasSource(manifest registry.Manifest, alias domain.HAAlias, want string) bool {
	source, present := manifest.SourceFor(alias)
	if !present {
		return false
	}
	value, valid := source.Value()
	return valid && value == want
}

func (d Decision) Pairs() []Pair {
	if !d.valid {
		return nil
	}
	pairs := make([]Pair, len(d.pairs))
	for i, pair := range d.pairs {
		pair.sourceRefs = pair.SourceRefs()
		pairs[i] = pair
	}
	return pairs
}

func (d Decision) PairFor(alias domain.HAAlias) (Pair, bool) {
	if !d.valid {
		return Pair{}, false
	}
	index, ok := d.byAlias[alias]
	if !ok {
		return Pair{}, false
	}
	pair := d.pairs[index]
	pair.sourceRefs = pair.SourceRefs()
	return pair, true
}

func (d Decision) HeatingOwner() Provider         { return d.heatOwner }
func (d Decision) HiloActionPolicy() ActionPolicy { return d.hiloAction }
func (d Decision) EnergyRelation() EnergyRelation { return d.energy }

// CandidateFor derives the original alias from the configured SQL binding.
// The caller cannot pair an independently selected alias with a SourceKey. A
// later Host.Discover resolves the same instance and key again, closing a
// retirement race; List and Resolve are separate reads, not one transaction.
func (d Decision) CandidateFor(ctx context.Context, repository *registry.Repository, instance domain.IntegrationInstanceID, key integrations.SourceKey, descriptions []*wireregistry.CapabilityDescription, catalog *contracts.CapabilityCatalog) (integrations.Candidate, error) {
	if !d.valid || repository == nil || ctx == nil || catalog == nil || len(descriptions) == 0 {
		return integrations.Candidate{}, ErrInvalidDecision
	}
	if _, err := instance.Value(); err != nil {
		return integrations.Candidate{}, ErrInvalidDecision
	}
	if _, _, err := key.Components(); err != nil {
		return integrations.Candidate{}, ErrInvalidDecision
	}
	records, err := repository.List(ctx)
	if err != nil {
		return integrations.Candidate{}, err
	}
	var alias domain.HAAlias
	found := false
	for _, record := range records {
		recordInstance, recordKey, err := record.Source()
		if err != nil {
			return integrations.Candidate{}, ErrInvalidDecision
		}
		if recordInstance != instance || recordKey != key {
			continue
		}
		if found {
			return integrations.Candidate{}, ErrInvalidDecision
		}
		if record.Retired() {
			return integrations.Candidate{}, registry.ErrTombstoned
		}
		alias, _, _, err = record.Aliases()
		if err != nil {
			return integrations.Candidate{}, ErrInvalidDecision
		}
		found = true
	}
	if !found {
		return integrations.Candidate{}, registry.ErrUnconfigured
	}
	if err := d.admitDescriptions(alias, descriptions, catalog); err != nil {
		return integrations.Candidate{}, err
	}
	return integrations.NewCandidate(key, descriptions)
}

// admitDescriptions is a pure policy check after an authoritative binding has
// supplied its original alias. Hilo's rule covers every manifest Hilo alias.
func (d Decision) admitDescriptions(alias domain.HAAlias, descriptions []*wireregistry.CapabilityDescription, catalog *contracts.CapabilityCatalog) error {
	if !d.valid || d.hiloAction != ActionDenied || d.heatOwner != ProviderNeviweb || d.energy != EnergyIndependent {
		return ErrInvalidDecision
	}
	source, present := d.manifest.SourceFor(alias)
	if !present {
		return ErrOutsideDecision
	}
	name, valid := source.Value()
	if !valid {
		return ErrInvalidDecision
	}
	var provider Provider
	switch name {
	case "hilo":
		provider = ProviderHilo
	case "neviweb130":
		provider = ProviderNeviweb
	default:
		return ErrOutsideDecision
	}
	if catalog == nil || len(descriptions) == 0 {
		return ErrInvalidDecision
	}
	pair, paired := d.PairFor(alias)
	for _, description := range descriptions {
		readOnlyErr := catalog.AdmitReadOnlyDriverCapability(description)
		if readOnlyErr == nil {
			continue
		}
		if provider == ProviderHilo || !errors.Is(readOnlyErr, contracts.ErrActionableCapability) || !paired || pair.neviweb != alias {
			return readOnlyErr
		}
		if pair.kind == CandidateThermostat && description.SchemaId != contracts.ClimateSchema ||
			pair.kind == CandidateWaterHeater && description.SchemaId != contracts.SwitchSchema {
			return ErrInvalidDecision
		}
	}
	return nil
}
