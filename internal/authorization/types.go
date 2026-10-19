// Package authorization owns admitted account policies and memory-only decisions.
// Authentication and PostgreSQL composition supply identities and current state.
package authorization

import (
	"crypto/sha256"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
)

var (
	ErrInvalid     = errors.New("authorization: invalid input")
	ErrDenied      = errors.New("authorization: permission denied")
	ErrUnavailable = errors.New("authorization: authority unavailable")
	ErrConflict    = errors.New("authorization: policy conflict")
	ErrCapacity    = errors.New("authorization: capacity exhausted")
)

// Permission uses the protocol's sole closed vocabulary. Decoding an enum does
// not make its operation meaningful; operational is checked separately.
type Permission struct {
	family    authv1.ScopeFamily
	operation authv1.ScopeOperation
}

func NewPermission(family authv1.ScopeFamily, operation authv1.ScopeOperation) (Permission, error) {
	if contracts.AdmitScope(&authv1.Scope{Family: family, Operation: operation}) != nil {
		return Permission{}, ErrInvalid
	}
	return Permission{family, operation}, nil
}
func (p Permission) Values() (authv1.ScopeFamily, authv1.ScopeOperation, error) {
	if _, err := NewPermission(p.family, p.operation); err != nil {
		return 0, 0, err
	}
	return p.family, p.operation, nil
}

type ScopeSet struct{ values map[Permission]struct{} }

func NewScopeSet(values []Permission) (ScopeSet, error) {
	if len(values) > 39 {
		return ScopeSet{}, ErrInvalid
	}
	set := make(map[Permission]struct{}, len(values))
	for _, p := range values {
		if _, _, err := p.Values(); err != nil {
			return ScopeSet{}, err
		}
		if _, duplicate := set[p]; duplicate {
			return ScopeSet{}, ErrInvalid
		}
		set[p] = struct{}{}
	}
	return ScopeSet{set}, nil
}

type principalKind uint8

const (
	principalInvalid principalKind = iota
	principalNative
	principalScoped
)

// Principal has no role snapshot or bearer material. A native session's
// credential ceiling is explicitly unbounded; an empty scoped set denies all.
type Principal struct {
	account    contracts.AccountID
	credential string
	kind       principalKind
	scopes     ScopeSet
}

func NativePrincipal(account contracts.AccountID, session contracts.SessionID) (Principal, error) {
	id, err := session.Value()
	if _, accountErr := account.Value(); err != nil || accountErr != nil {
		return Principal{}, ErrInvalid
	}
	return Principal{account: account, credential: id, kind: principalNative}, nil
}

func ScopedPrincipal(account contracts.AccountID, credential contracts.McpKeyID, scopes ScopeSet) (Principal, error) {
	id, err := credential.Value()
	if _, accountErr := account.Value(); err != nil || accountErr != nil || scopes.values == nil {
		return Principal{}, ErrInvalid
	}
	owned, err := NewScopeSet(scopeValues(scopes))
	if err != nil {
		return Principal{}, err
	}
	return Principal{account: account, credential: id, kind: principalScoped, scopes: owned}, nil
}

func (p Principal) Account() (contracts.AccountID, error) {
	if !p.valid() {
		return contracts.AccountID{}, ErrInvalid
	}
	return p.account, nil
}
func (p Principal) Actor() (domain.ActorID, error) {
	id, err := p.Account()
	if err != nil {
		return domain.ActorID{}, err
	}
	text, err := id.Value()
	if err != nil {
		return domain.ActorID{}, ErrInvalid
	}
	return domain.ParseActorID(text)
}
func (p Principal) Native() bool { return p.valid() && p.kind == principalNative }
func (p Principal) valid() bool {
	if _, err := p.account.Value(); err != nil || p.credential == "" {
		return false
	}
	switch p.kind {
	case principalNative:
		return p.scopes.values == nil
	case principalScoped:
		return p.scopes.values != nil
	case principalInvalid:
		return false
	default:
		return false
	}
}
func (p Principal) permits(permission Permission) bool {
	if !p.valid() {
		return false
	}
	switch p.kind {
	case principalNative:
		return true
	case principalScoped:
		_, ok := p.scopes.values[permission]
		return ok
	case principalInvalid:
		return false
	default:
		return false
	}
}

func scopeValues(set ScopeSet) []Permission {
	values := make([]Permission, 0, len(set.values))
	for permission := range set.values {
		values = append(values, permission)
	}
	return values
}

type DashboardID struct{ dashboard string }

func ParseDashboardID(text string) (DashboardID, error) {
	if !validLabel(text, 256) {
		return DashboardID{}, ErrInvalid
	}
	return DashboardID{text}, nil
}
func (id DashboardID) Value() (string, error) {
	if !validLabel(id.dashboard, 256) {
		return "", ErrInvalid
	}
	return id.dashboard, nil
}

// CapabilitySlot is an extensible schema/endpoint identity. The caller must
// obtain it from the trusted catalogue, never a display name or client role.
type CapabilitySlot struct{ schema, endpoint string }

func NewCapabilitySlot(schema, endpoint string) (CapabilitySlot, error) {
	if !validLabel(schema, 256) || !validLabel(endpoint, 256) {
		return CapabilitySlot{}, ErrInvalid
	}
	return CapabilitySlot{schema, endpoint}, nil
}
func (s CapabilitySlot) Values() (string, string, error) {
	if _, err := NewCapabilitySlot(s.schema, s.endpoint); err != nil {
		return "", "", err
	}
	return s.schema, s.endpoint, nil
}

type objectKind uint8

const (
	objectInvalid objectKind = iota
	objectEntity
	objectDashboard
	objectAccounts
	objectIntegrations
	objectGlobalConfiguration
	objectList
)

type Object struct {
	kind       objectKind
	entity     domain.EntityID
	slot       CapabilitySlot
	dashboard  DashboardID
	owner      contracts.AccountID
	list       domain.ListID
	collection bool
}

func ListObject(id domain.ListID) (Object, error) {
	if _, err := id.Value(); err != nil {
		return Object{}, ErrInvalid
	}
	return Object{kind: objectList, list: id}, nil
}

// ListCollectionObject denotes a bounded catalogue query or creation/import.
// Authorization still evaluates an all/explicit selector for the operation.
func ListCollectionObject() Object { return Object{kind: objectList, collection: true} }

func EntityObject(entity domain.EntityID, slot CapabilitySlot) (Object, error) {
	if _, err := entity.Value(); err != nil {
		return Object{}, ErrInvalid
	}
	if _, _, err := slot.Values(); err != nil {
		return Object{}, ErrInvalid
	}
	return Object{kind: objectEntity, entity: entity, slot: slot}, nil
}

// CommandTarget is the resolved immutable authorization identity of one
// intention. It is stored beside the exact actor/wire fingerprint so alias
// reassignment cannot authorize an old result against a different entity.
type CommandTarget struct {
	entity domain.EntityID
	slot   CapabilitySlot
}

func NewCommandTarget(entity domain.EntityID, slot CapabilitySlot) (CommandTarget, error) {
	if _, err := EntityObject(entity, slot); err != nil {
		return CommandTarget{}, err
	}
	return CommandTarget{entity, slot}, nil
}
func (t CommandTarget) Values() (domain.EntityID, CapabilitySlot, error) {
	if _, err := EntityObject(t.entity, t.slot); err != nil {
		return domain.EntityID{}, CapabilitySlot{}, err
	}
	return t.entity, t.slot, nil
}
func (t CommandTarget) Object() (Object, error) { return EntityObject(t.entity, t.slot) }
func DashboardObject(id DashboardID, trustedOwner contracts.AccountID) (Object, error) {
	if _, err := id.Value(); err != nil {
		return Object{}, ErrInvalid
	}
	if _, err := trustedOwner.Value(); err != nil {
		return Object{}, ErrInvalid
	}
	return Object{kind: objectDashboard, dashboard: id, owner: trustedOwner}, nil
}

// AdministrativeObject describes a known operation's category. The server
// route table must still bind a concrete registered procedure to its permission.
func AdministrativeObject(family authv1.ScopeFamily) (Object, error) {
	switch family {
	case authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS:
		return Object{kind: objectAccounts}, nil
	case authv1.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS:
		return Object{kind: objectIntegrations}, nil
	case authv1.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION:
		return Object{kind: objectGlobalConfiguration}, nil
	default:
		return Object{}, ErrInvalid
	}
}

func validLabel(text string, limit int) bool {
	return text != "" && len(text) <= limit && utf8.ValidString(text) && strings.TrimSpace(text) == text && !strings.ContainsAny(text, "\x00\r\n")
}

type Version struct {
	revision uint64
	hash     [sha256.Size]byte
}

func NewVersion(revision uint64, hash [sha256.Size]byte) (Version, error) {
	if revision == 0 || revision > 1<<63-1 || hash == ([sha256.Size]byte{}) {
		return Version{}, ErrInvalid
	}
	return Version{revision, hash}, nil
}
func (v Version) Values() (uint64, [sha256.Size]byte, error) {
	if _, err := NewVersion(v.revision, v.hash); err != nil {
		return 0, [sha256.Size]byte{}, err
	}
	return v.revision, v.hash, nil
}

type expectationKind uint8

const (
	expectationInvalid expectationKind = iota
	expectationAbsent
	expectationVersion
)

type ExpectedVersion struct {
	kind    expectationKind
	version Version
}

func ExpectAbsent() ExpectedVersion { return ExpectedVersion{kind: expectationAbsent} }
func ExpectVersion(version Version) (ExpectedVersion, error) {
	if _, _, err := version.Values(); err != nil {
		return ExpectedVersion{}, err
	}
	return ExpectedVersion{kind: expectationVersion, version: version}, nil
}
func (e ExpectedVersion) Values() (bool, Version, error) {
	switch e.kind {
	case expectationAbsent:
		return true, Version{}, nil
	case expectationVersion:
		if _, _, err := e.version.Values(); err != nil {
			return false, Version{}, err
		}
		return false, e.version, nil
	case expectationInvalid:
		return false, Version{}, ErrInvalid
	default:
		return false, Version{}, ErrInvalid
	}
}
