package configplans

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io/fs"
	"reflect"
	"strings"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
	"go.yaml.in/yaml/v3"
)

type Service struct {
	documents *configdocuments.Service
	owner     *configactivation.Owner
	replay    *storage.ConfigReplayRepository
	manifest  registry.Manifest
	registry  *registry.Repository
	key       [32]byte
}
type Input struct {
	Documents *configdocuments.Service
	Owner     *configactivation.Owner
	Secrets   *storage.SecretRepository
	Manifest  registry.Manifest
}

func NewService(input Input) (*Service, error) {
	if input.Documents == nil || input.Documents.Store() == nil || input.Owner == nil || input.Secrets == nil || !input.Secrets.Valid() {
		return nil, ErrInvalid
	}
	if _, ok := input.Manifest.Revision(); !ok {
		return nil, ErrInvalid
	}
	replay, e := storage.NewConfigReplayRepository(input.Documents.Store(), input.Secrets)
	if e != nil {
		return nil, ErrUnavailable
	}
	repository, e := registry.NewRepository(input.Documents.Store(), input.Manifest)
	if e != nil {
		return nil, ErrUnavailable
	}
	service := &Service{documents: input.Documents, owner: input.Owner, replay: replay, manifest: input.Manifest, registry: repository}
	if _, e := rand.Read(service.key[:]); e != nil || service.key == ([32]byte{}) {
		return nil, ErrUnavailable
	}
	return service, nil
}
func usableContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !v.IsNil()
	default:
		return true
	}
}
func (s *Service) valid() bool {
	return s != nil && s.documents != nil && s.owner != nil && s.replay != nil && s.registry != nil && s.key != ([32]byte{})
}
func (s *Service) Build(ctx context.Context, candidate Candidate, author storage.ConfigAuthor) (Plan, error) {
	if !s.valid() || !usableContext(ctx) || !candidate.Valid() {
		return Plan{}, ErrInvalid
	}
	name, e := usableAuthor(author)
	if e != nil {
		return Plan{}, e
	}
	before, e := s.capture(ctx, candidate)
	if e != nil {
		return Plan{}, e
	}
	plan := Plan{state: Admitted, author: name, family: candidate.family, identity: candidate.identity, schema: candidate.Schema(), entry: candidate.entry, source: candidate.hash, base: before.base, generation: before.generation, manifest: before.manifest, workspace: before.workspace, catalog: before.catalog, effect: NoEffect}
	next, diagnostic := compile(candidate, s.manifest)
	if diagnostic != DiagnosticInvalid {
		plan.state = Rejected
		plan.effect = familyEffect(candidate.family)
		plan.diagnostics = []Diagnostic{diagnostic}
	} else {
		if candidate.family == Authorization {
			if ok, e := s.policyReferences(ctx, candidate); e != nil {
				return Plan{}, e
			} else if !ok {
				plan.state = Rejected
				plan.effect = RightsEffect
				plan.diagnostics = []Diagnostic{ExcludedReference}
			}
		}
		if plan.state == Admitted {
			prior := image{fields: map[string][32]byte{}, refs: map[string]bool{}, effect: familyEffect(candidate.family)}
			if before.source.Valid() {
				var d Diagnostic
				prior, d = compile(before.source, s.manifest)
				if d != DiagnosticInvalid {
					return Plan{}, ErrUnavailable
				}
			}
			plan.semantic = next.digest
			plan.changes = difference(prior, next)
			for _, change := range plan.changes {
				if change.effect > plan.effect {
					plan.effect = change.effect
				}
			}
		}
	}
	after, e := s.capture(ctx, candidate)
	if e != nil {
		return Plan{}, e
	}
	if !before.equal(after) {
		return Plan{}, ErrStale
	}
	// A final Owner stamp check rejects publication/closure during compilation.
	stamp, e := s.owner.CurrentConfig(ctx)
	if e != nil {
		return Plan{}, ErrUnavailable
	}
	generation, manifest, e := stamp.Components()
	if e != nil || int64(generation) != before.generation || manifest != before.manifest {
		return Plan{}, ErrStale
	}
	plan.receipt, e = s.sign(plan, time.Now().UTC())
	if e != nil {
		return Plan{}, e
	}
	return plan, nil
}
func (s *Service) Revalidate(ctx context.Context, candidate Candidate, receipt string) (Plan, error) {
	if !s.valid() || !usableContext(ctx) || !candidate.Valid() {
		return Plan{}, ErrInvalid
	}
	signed, e := s.verify(receipt, time.Now().UTC())
	if e != nil {
		return Plan{}, e
	}
	if signed.Family != candidate.family || signed.Identity != candidate.identity || signed.Entry != sha256.Sum256([]byte(candidate.entry)) || signed.Source != candidate.hash {
		return Plan{}, ErrInvalid
	}
	author, e := storage.NewConfigAuthor(signed.Author)
	if e != nil {
		return Plan{}, ErrInvalid
	}
	plan, e := s.Build(ctx, candidate, author)
	if e != nil {
		return Plan{}, e
	}
	if !signed.matches(plan) {
		plan.state = Stale
		plan.receipt = ""
	}
	return plan, nil
}

type basis struct {
	generation                   int64
	manifest, workspace, catalog [32]byte
	base                         Base
	source                       Candidate
}

func (b basis) equal(other basis) bool {
	return b.generation == other.generation && b.manifest == other.manifest && b.workspace == other.workspace && b.catalog == other.catalog && b.base == other.base
}
func (s *Service) capture(ctx context.Context, candidate Candidate) (basis, error) {
	id, _ := storage.NewConfigDocumentID("operational.main")
	for attempt := 0; attempt < 2; attempt++ {
		snapshot, e := s.documents.Read(ctx, id)
		if e != nil {
			return basis{}, ErrUnavailable
		}
		workspace, ok := snapshot.Workspace().SHA256()
		if !ok || snapshot.Workspace().ReconciliationPending() {
			return basis{}, ErrUnavailable
		}
		active, present, e := s.documents.Store().ReadActiveConfig(ctx)
		if e != nil || !present || active.Availability() != storage.ConfigGenerationComplete {
			return basis{}, ErrUnavailable
		}
		if active.Revision() != snapshot.Generation() || active.ManifestSHA256() != snapshot.ManifestSHA256() {
			continue
		}
		value := basis{generation: active.Revision().Number(), manifest: active.ManifestSHA256(), workspace: workspace, catalog: snapshot.CatalogCut().SHA256(), base: Base{kind: NoActiveDocument}}
		if candidate.family == Authorization {
			account, e := contracts.ParseAccountID(candidate.identity)
			if e != nil {
				return basis{}, ErrInvalid
			}
			policy, e := s.documents.Store().AuthorizationPolicy(ctx, account)
			if e != nil {
				return basis{}, ErrUnavailable
			}
			source, e := policy.Source()
			if e != nil {
				return basis{}, ErrUnavailable
			}
			version, e := policy.Version()
			if e != nil {
				return basis{}, ErrUnavailable
			}
			revision, hash, e := version.Values()
			if e != nil {
				return basis{}, ErrUnavailable
			}
			raw, e := source.Bytes()
			if e != nil {
				return basis{}, ErrUnavailable
			}
			file, e := newFamilySource(candidate.family, candidate.entry, raw)
			if e != nil {
				return basis{}, ErrUnavailable
			}
			value.source, e = NewCandidate(Authorization, candidate.identity, candidate.entry, []parser.SourceFile{file})
			if e != nil {
				return basis{}, ErrUnavailable
			}
			value.base = Base{kind: ActiveSQL, revision: int64(revision), source: hash}
		} else {
			var selected []parser.SourceFile
			var revision int64
			baseFamily := candidate.family
			baseEntry := candidate.entry
			baseIdentity := candidate.identity
			for _, version := range active.Versions() {
				kind, retained := version.Kind()
				if !retained {
					return basis{}, ErrUnavailable
				}
				if candidate.family == Operational && kind == storage.ConfigDocumentKindOperationalYAML {
					if version.Ref().Document().String() != candidate.identity || version.Path() != candidate.entry {
						return basis{}, ErrInvalid
					}
					protected, e := s.replay.ReadActiveProtectedSource(ctx, active.Revision(), active.ManifestSHA256(), version.Ref())
					if e != nil {
						return basis{}, ErrUnavailable
					}
					file, e := parser.NewSourceFile(version.Path(), protected.Bytes())
					if e != nil {
						return basis{}, ErrUnavailable
					}
					selected = []parser.SourceFile{file}
					revision = version.Ref().Revision().Number()
				}
			}
			if candidate.family == Declarative1 || candidate.family == Declarative2 {
				prior, currentRevision, priorErr := capturedDeclarativeCandidate(candidate, active)
				if priorErr != nil {
					return basis{}, priorErr
				}
				revision = currentRevision
				if prior.Valid() {
					selected, baseFamily, baseIdentity, baseEntry = prior.files, prior.family, prior.identity, prior.entry
				}
			}
			if editableFamily(candidate.family) {
				selected, revision, baseEntry, e = s.capturedEditableSources(ctx, candidate, active)
				if e != nil {
					return basis{}, e
				}
			}
			if len(selected) > 0 {
				var e error
				value.source, e = NewCandidate(baseFamily, baseIdentity, baseEntry, selected)
				if e != nil {
					return basis{}, ErrInvalid
				}
				if revision == 0 {
					return basis{}, ErrInvalid
				}
				value.base = Base{kind: ActiveSQL, revision: revision, source: value.source.hash}
			} else {
				if candidate.family == Operational {
					return basis{}, ErrUnavailable
				}
				immutable, e := immutableSource(candidate)
				if e != nil {
					return basis{}, e
				}
				if immutable.Valid() {
					value.source = immutable
					value.base = Base{kind: ImmutableReference, source: immutable.hash}
				}
			}
		}
		again, e := s.documents.Read(ctx, id)
		if e != nil {
			return basis{}, ErrUnavailable
		}
		hash, observed := again.Workspace().SHA256()
		if !observed || again.Workspace().ReconciliationPending() {
			return basis{}, ErrUnavailable
		}
		if again.Generation() != snapshot.Generation() || again.ManifestSHA256() != snapshot.ManifestSHA256() || again.CatalogCut().SHA256() != snapshot.CatalogCut().SHA256() || hash != workspace {
			continue
		}
		return value, nil
	}
	return basis{}, ErrStale
}
func immutableSource(candidate Candidate) (Candidate, error) {
	var raw []byte
	switch candidate.family {
	case Ownership:
		if candidate.identity != "heating.ownership" {
			return Candidate{}, ErrInvalid
		}
		raw = scope.HeatingOwnershipV1()
	case Automation, Scene:
		selection, e := logicselection.LoadV1()
		if e != nil {
			return Candidate{}, ErrUnavailable
		}
		id, e := domain.ParseHAAlias(candidate.identity)
		if e != nil {
			return Candidate{}, ErrInvalid
		}
		disposition, exists := selection.DispositionFor(id)
		if !exists || disposition != logicselection.DispositionRetained {
			return Candidate{}, ErrInvalid
		}
		for _, definition := range selection.Retained() {
			if definition.ID() == id {
				raw, e = fs.ReadFile(scope.LogicMVPSourcesV1(), strings.TrimPrefix(definition.Path(), "scope/"))
				if e != nil {
					return Candidate{}, ErrUnavailable
				}
				if sha256.Sum256(raw) != definition.Digest() {
					return Candidate{}, ErrUnavailable
				}
				break
			}
		}
	case Dashboard, Script, Declarative1, Declarative2:
		return Candidate{}, nil
	case Operational, Authorization, FamilyInvalid:
		return Candidate{}, ErrInvalid
	default:
		return Candidate{}, ErrInvalid
	}
	if len(raw) == 0 {
		return Candidate{}, ErrUnavailable
	}
	file, e := newFamilySource(candidate.family, candidate.entry, raw)
	if e != nil {
		return Candidate{}, ErrInvalid
	}
	return NewCandidate(candidate.family, candidate.identity, candidate.entry, []parser.SourceFile{file})
}

// Policy objects use Homeward entity IDs. Validate explicit targets through the
// existing registry list; no HA alias/id guess or second identity authority.
func (s *Service) policyReferences(ctx context.Context, candidate Candidate) (bool, error) {
	node, e := admittedYAML(candidate.files[0].Bytes())
	if e != nil {
		return false, nil
	}
	ids := map[string]bool{}
	var visit func(*yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i < len(n.Content); i += 2 {
				if n.Content[i].Value == "entity_id" {
					ids[n.Content[i+1].Value] = true
				}
				visit(n.Content[i+1])
			}
		} else {
			for _, child := range n.Content {
				visit(child)
			}
		}
	}
	visit(node)
	if len(ids) == 0 {
		return true, nil
	}
	records, e := s.registry.List(ctx)
	if e != nil {
		return false, ErrUnavailable
	}
	for _, record := range records {
		if record.Retired() {
			continue
		}
		_, entity, e := record.IDs()
		if e != nil {
			return false, ErrUnavailable
		}
		name, e := entity.Value()
		if e != nil {
			return false, ErrUnavailable
		}
		delete(ids, name)
	}
	return len(ids) == 0, nil
}
