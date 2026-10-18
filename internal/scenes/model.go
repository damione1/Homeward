package scenes

import (
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	hascene "github.com/damione1/Homeward/compat/ha/scene"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

var ErrSceneUnavailable = errors.New("scene is unavailable or changed")
var ErrSceneCapacity = errors.New("scene memory capacity exhausted")
var ErrSceneConflict = errors.New("scene version or invocation conflict")
var ErrSceneClosed = errors.New("scene service is closed")

type SceneKind uint8

const (
	SceneInvalid SceneKind = iota
	SceneDeclarative
	SceneTemporary
)

type SceneRef struct {
	id      domain.HAAlias
	kind    SceneKind
	version uint64
	digest  [32]byte
}

func (r SceneRef) ID() (domain.HAAlias, error) {
	if r.kind != SceneDeclarative && r.kind != SceneTemporary {
		return domain.HAAlias{}, ErrInvalidScene
	}
	if _, err := r.id.Value(); err != nil {
		return domain.HAAlias{}, err
	}
	if r.kind == SceneTemporary && r.version == 0 {
		return domain.HAAlias{}, ErrInvalidScene
	}
	if r.kind == SceneDeclarative && r.digest == ([32]byte{}) {
		return domain.HAAlias{}, ErrInvalidScene
	}
	return r.id, nil
}
func (r SceneRef) Kind() SceneKind { return r.kind }
func (r SceneRef) Version() uint64 { return r.version }

type MemberOutcome uint8

const (
	OutcomeInvalid MemberOutcome = iota
	OutcomePrepared
	OutcomeAlreadySatisfied
	OutcomeRefused
	OutcomeAccepted
	OutcomeDispatched
	OutcomeAcknowledged
	OutcomeConfirmed
	OutcomeIndeterminate
)

type Aggregate uint8

const (
	AggregateInvalid Aggregate = iota
	AggregateComplete
	AggregatePartial
	AggregateNone
)

type MemberResult struct {
	alias         domain.HAAlias
	source        parser.Position
	code          RefusalCode
	field         string
	reason        string
	outcome       MemberOutcome
	response      *actions.CommandResponse
	proofRevision uint64
}

type RefusalCode uint8

const (
	RefusalNone RefusalCode = iota
	RefusalSource
	RefusalQuality
	RefusalCapability
	RefusalRoute
	RefusalSceneChanged
	RefusalCommand
)

func (r MemberResult) Alias() domain.HAAlias   { return r.alias }
func (r MemberResult) Source() parser.Position { return r.source }
func (r MemberResult) Code() RefusalCode       { return r.code }
func (r MemberResult) Field() string           { return r.field }
func (r MemberResult) Reason() string          { return r.reason }
func (r MemberResult) Outcome() MemberOutcome  { return r.outcome }
func (r MemberResult) ProofRevision() (uint64, bool) {
	return r.proofRevision, r.outcome == OutcomeAlreadySatisfied && r.proofRevision != 0
}
func (r MemberResult) Response() *actions.CommandResponse {
	if r.response == nil {
		return nil
	}
	return proto.Clone(r.response).(*actions.CommandResponse)
}

type ApplyResult struct {
	members   []MemberResult
	aggregate Aggregate
}

func (r ApplyResult) Members() []MemberResult {
	out := make([]MemberResult, len(r.members))
	for i, member := range r.members {
		out[i] = member
		out[i].response = member.Response()
	}
	return out
}
func (r ApplyResult) Aggregate() Aggregate { return r.aggregate }

type preparedKind uint8

const (
	preparedInvalid preparedKind = iota
	preparedLight
	preparedFan
	preparedDirect
	preparedRefused
)

type preparedMember struct {
	kind      preparedKind
	alias     domain.HAAlias
	source    parser.Position
	code      RefusalCode
	field     string
	reason    string
	light     integrations.PreparedLight
	fan       integrations.PreparedFan
	direct    integrations.PreparedDirect
	satisfied *satisfactionSlot
}

type satisfactionSlot struct {
	mu       sync.Mutex
	proven   bool
	revision uint64
}
type scenePlan struct {
	service    *Service
	ref        SceneRef
	actor      domain.ActorID
	invocation domain.CommandID
	expiresAt  time.Time
	members    []preparedMember
}

// PreparedScene is a private plan handle. Its members and wire envelopes
// cannot be modified by callers; losing it cannot reconstruct a preparation.
type PreparedScene struct{ plan *scenePlan }

func (p PreparedScene) Ref() (SceneRef, error) {
	if p.plan == nil {
		return SceneRef{}, ErrInvalidScene
	}
	return p.plan.ref, nil
}
func (p PreparedScene) Members() []MemberResult {
	if p.plan == nil {
		return nil
	}
	out := make([]MemberResult, len(p.plan.members))
	for i, member := range p.plan.members {
		out[i] = MemberResult{alias: member.alias, source: member.source, code: member.code, field: member.field, reason: member.reason, outcome: OutcomePrepared}
		if member.kind == preparedRefused {
			out[i].outcome = OutcomeRefused
		}
	}
	return out
}

type memberOrigin uint8

const (
	originInvalid memberOrigin = iota
	originHA
	originNative
)

type memberSpec struct {
	origin           memberOrigin
	alias            domain.HAAlias
	source           parser.Position
	ha               hascene.Member
	native           domain.StateValue
	capturedRevision uint64
	omissionCode     OmissionCode
	omissionReason   string
}
