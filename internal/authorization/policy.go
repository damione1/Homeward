package authorization

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strconv"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
)

const Schema = contracts.AuthorizationPolicySchema
const SyntaxProfile = contracts.AuthorizationPolicySyntaxProfile
const MaxSourceBytes = contracts.MaxAuthorizationPolicySourceBytes
const MaxGrants = 39
const MaxEntities = 4096
const MaxSlots = 64
const MaxDashboards = 1024
const MaxLists = 4096
const MaxDepth = 16
const MaxAuthorityBytes = 32 << 20

type selectionKind uint8

const (
	selectionInvalid selectionKind = iota
	selectionAll
	selectionExplicit
	selectionOwn
)

type slots struct {
	kind     selectionKind
	explicit map[CapabilitySlot]struct{}
}
type grant struct {
	permission Permission
	object     objectKind
	kind       selectionKind
	allSlots   slots
	entities   map[domain.EntityID]slots
	dashboards map[DashboardID]struct{}
	lists      map[domain.ListID]struct{}
}

type Policy struct {
	account  contracts.AccountID
	grants   []grant
	semantic [sha256.Size]byte
	cost     int
}

type PolicySource struct {
	policy Policy
	bytes  []byte
	hash   [sha256.Size]byte
}

func (p PolicySource) Account() (contracts.AccountID, error) {
	if !p.valid() {
		return contracts.AccountID{}, ErrInvalid
	}
	return p.policy.account, nil
}
func (p PolicySource) Bytes() ([]byte, error) {
	if !p.valid() {
		return nil, ErrInvalid
	}
	return bytes.Clone(p.bytes), nil
}
func (p PolicySource) SHA256() ([sha256.Size]byte, error) {
	if !p.valid() {
		return [sha256.Size]byte{}, ErrInvalid
	}
	return p.hash, nil
}
func (p PolicySource) SemanticSHA256() ([sha256.Size]byte, error) {
	if !p.valid() {
		return [sha256.Size]byte{}, ErrInvalid
	}
	return p.policy.semantic, nil
}
func (p PolicySource) valid() bool {
	_, err := p.policy.account.Value()
	return err == nil && p.bytes != nil && len(p.bytes) <= MaxSourceBytes && p.hash != ([sha256.Size]byte{}) && p.policy.semantic != ([sha256.Size]byte{})
}

func EmptySource(account contracts.AccountID) (PolicySource, error) {
	text, err := account.Value()
	if err != nil {
		return PolicySource{}, ErrInvalid
	}
	// Quoted YAML strings are produced by the same admitted decoder's encoder.
	return encodeEmptySource(account, text)
}

func (s slots) permits(slot CapabilitySlot) bool {
	switch s.kind {
	case selectionAll:
		return true
	case selectionExplicit:
		_, allowed := s.explicit[slot]
		return allowed
	case selectionOwn, selectionInvalid:
		return false
	default:
		return false
	}
}

func (g grant) permits(account contracts.AccountID, object Object) bool {
	if object.kind != g.object {
		return false
	}
	switch object.kind {
	case objectEntity:
		if _, err := object.entity.Value(); err != nil {
			return false
		}
		if _, _, err := object.slot.Values(); err != nil {
			return false
		}
		switch g.kind {
		case selectionAll:
			return g.allSlots.permits(object.slot)
		case selectionExplicit:
			allowed, ok := g.entities[object.entity]
			return ok && allowed.permits(object.slot)
		case selectionOwn, selectionInvalid:
			return false
		default:
			return false
		}
	case objectDashboard:
		if _, err := object.dashboard.Value(); err != nil {
			return false
		}
		if _, err := object.owner.Value(); err != nil {
			return false
		}
		switch g.kind {
		case selectionAll:
			return true
		case selectionOwn:
			return object.owner == account
		case selectionExplicit:
			_, allowed := g.dashboards[object.dashboard]
			return allowed
		case selectionInvalid:
			return false
		default:
			return false
		}
	case objectList:
		if object.collection {
			if _, err := object.list.Value(); err == nil {
				return false
			}
			if g.permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_WRITE {
				return g.kind == selectionAll
			}
			return g.kind == selectionAll || g.kind == selectionExplicit && len(g.lists) != 0
		}
		if _, err := object.list.Value(); err != nil {
			return false
		}
		switch g.kind {
		case selectionAll:
			return true
		case selectionExplicit:
			_, allowed := g.lists[object.list]
			return allowed
		case selectionOwn, selectionInvalid:
			return false
		default:
			return false
		}
	case objectAccounts, objectIntegrations, objectGlobalConfiguration, objectInvalid:
		return false
	default:
		return false
	}
}

func configurable(permission Permission) objectKind {
	switch permission.family {
	case authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeFamily_SCOPE_FAMILY_HISTORY, authv1.ScopeFamily_SCOPE_FAMILY_MEDIA:
		if permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_READ {
			return objectEntity
		}
	case authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, authv1.ScopeFamily_SCOPE_FAMILY_CALENDARS:
		if permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_READ || permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_WRITE {
			return objectEntity
		}
	case authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS:
		if permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_READ || permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_WRITE || permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_ADMINISTER {
			return objectDashboard
		}
	case authv1.ScopeFamily_SCOPE_FAMILY_LISTS:
		if permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_READ || permission.operation == authv1.ScopeOperation_SCOPE_OPERATION_WRITE {
			return objectList
		}
	case authv1.ScopeFamily_SCOPE_FAMILY_UNSPECIFIED,
		authv1.ScopeFamily_SCOPE_FAMILY_LOCATION,
		authv1.ScopeFamily_SCOPE_FAMILY_LOGIC,
		authv1.ScopeFamily_SCOPE_FAMILY_INTEGRATIONS, authv1.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION,
		authv1.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, authv1.ScopeFamily_SCOPE_FAMILY_BACKUPS:
		return objectInvalid
	default:
		return objectInvalid
	}
	return objectInvalid
}

func semanticHash(policy Policy) [sha256.Size]byte {
	hash := sha256.New()
	write := func(text string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(text)))
		hash.Write(size[:])
		hash.Write([]byte(text))
	}
	write(Schema)
	account, _ := policy.account.Value()
	write(account)
	ordered := append([]grant(nil), policy.grants...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].permission.family != ordered[j].permission.family {
			return ordered[i].permission.family < ordered[j].permission.family
		}
		return ordered[i].permission.operation < ordered[j].permission.operation
	})
	writeSlots := func(value slots) {
		write(string([]byte{byte(value.kind)}))
		write(strconv.Itoa(len(value.explicit)))
		keys := make([]CapabilitySlot, 0, len(value.explicit))
		for key := range value.explicit {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].schema != keys[j].schema {
				return keys[i].schema < keys[j].schema
			}
			return keys[i].endpoint < keys[j].endpoint
		})
		for _, key := range keys {
			write(key.schema)
			write(key.endpoint)
		}
	}
	write(strconv.Itoa(len(ordered)))
	for _, value := range ordered {
		write(value.permission.family.String())
		write(value.permission.operation.String())
		write(string([]byte{byte(value.object), byte(value.kind)}))
		writeSlots(value.allSlots)
		write(strconv.Itoa(len(value.entities)))
		entities := make([]domain.EntityID, 0, len(value.entities))
		for id := range value.entities {
			entities = append(entities, id)
		}
		sort.Slice(entities, func(i, j int) bool {
			left, _ := entities[i].Value()
			right, _ := entities[j].Value()
			return left < right
		})
		for _, id := range entities {
			text, _ := id.Value()
			write(text)
			writeSlots(value.entities[id])
		}
		write(strconv.Itoa(len(value.dashboards)))
		dashboards := make([]DashboardID, 0, len(value.dashboards))
		for id := range value.dashboards {
			dashboards = append(dashboards, id)
		}
		sort.Slice(dashboards, func(i, j int) bool { return dashboards[i].dashboard < dashboards[j].dashboard })
		for _, id := range dashboards {
			write(id.dashboard)
		}
		// The old /1 semantic encoding is byte-for-byte unchanged for all
		// pre-LISTS grants. Only the new object variant appends a collection.
		if value.object == objectList {
			write(strconv.Itoa(len(value.lists)))
			lists := make([]domain.ListID, 0, len(value.lists))
			for id := range value.lists {
				lists = append(lists, id)
			}
			sort.Slice(lists, func(i, j int) bool {
				left, _ := lists[i].Value()
				right, _ := lists[j].Value()
				return left < right
			})
			for _, id := range lists {
				text, _ := id.Value()
				write(text)
			}
		}
		// Delimit each collection and grant independently of caller ordering.
		write("end-grant")
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}
