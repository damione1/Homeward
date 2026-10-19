package authorization

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
	"go.yaml.in/yaml/v3"
)

// Diagnostic never includes rejected source text or object identifiers.
type Diagnostic struct{ path string }

func (d *Diagnostic) Error() string { return "authorization: invalid policy source" }
func (d *Diagnostic) Unwrap() error { return ErrInvalid }
func (d *Diagnostic) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}
func invalidSource(path string) error { return &Diagnostic{path: path} }

type emptyInput struct{}
type sourceInput struct {
	Schema  string        `yaml:"schema"`
	Account string        `yaml:"account_id"`
	Grants  *[]grantInput `yaml:"grants"`
}
type permissionInput struct {
	Family    string `yaml:"family"`
	Operation string `yaml:"operation"`
}
type grantInput struct {
	Scope   *permissionInput `yaml:"scope"`
	Objects *objectInput     `yaml:"objects"`
}
type objectInput struct {
	Entities   *entitiesInput   `yaml:"entities"`
	Dashboards *dashboardsInput `yaml:"dashboards"`
	Lists      *listsInput      `yaml:"lists"`
}
type listsInput struct {
	All      *emptyInput `yaml:"all"`
	Explicit *[]string   `yaml:"explicit"`
}
type entitiesInput struct {
	All      *allEntitiesInput `yaml:"all"`
	Explicit *[]entityInput    `yaml:"explicit"`
}
type allEntitiesInput struct {
	Capabilities *slotsInput `yaml:"capabilities"`
}
type entityInput struct {
	ID           string      `yaml:"entity_id"`
	Capabilities *slotsInput `yaml:"capabilities"`
}
type slotsInput struct {
	All      *emptyInput  `yaml:"all"`
	Explicit *[]slotInput `yaml:"explicit"`
}
type slotInput struct {
	Schema   string `yaml:"schema_id"`
	Endpoint string `yaml:"endpoint"`
}
type dashboardsInput struct {
	All      *emptyInput `yaml:"all"`
	Own      *emptyInput `yaml:"own"`
	Explicit *[]string   `yaml:"explicit"`
}

func encodeEmptySource(account contracts.AccountID, text string) (PolicySource, error) {
	empty := []grantInput{}
	input := sourceInput{Schema: Schema, Account: text, Grants: &empty}
	data, err := yaml.Marshal(input)
	if err != nil {
		return PolicySource{}, ErrInvalid
	}
	return AdmitPolicySource(account, data)
}

func AdmitPolicySource(account contracts.AccountID, data []byte) (PolicySource, error) {
	accountText, err := account.Value()
	if err != nil || len(data) == 0 || len(data) > MaxSourceBytes || !utf8.Valid(data) {
		return PolicySource{}, invalidSource("source")
	}
	owned := bytes.Clone(data)
	decoder := yaml.NewDecoder(bytes.NewReader(owned))
	var tree yaml.Node
	if err := decoder.Decode(&tree); err != nil || len(tree.Content) != 1 || tree.Content[0].Kind != yaml.MappingNode {
		return PolicySource{}, invalidSource("source")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return PolicySource{}, invalidSource("source")
	}
	if !safeNode(&tree, 0) {
		return PolicySource{}, invalidSource("source")
	}
	decoder = yaml.NewDecoder(bytes.NewReader(owned))
	decoder.KnownFields(true)
	var input sourceInput
	if err := decoder.Decode(&input); err != nil {
		return PolicySource{}, invalidSource("source")
	}
	if input.Schema != Schema {
		return PolicySource{}, invalidSource("schema")
	}
	if input.Account != accountText {
		return PolicySource{}, invalidSource("account_id")
	}
	if input.Grants == nil || len(*input.Grants) > MaxGrants {
		return PolicySource{}, invalidSource("grants")
	}
	policy := Policy{account: account, grants: make([]grant, 0, len(*input.Grants)), cost: 256}
	seen := make(map[Permission]struct{}, len(*input.Grants))
	entities, dashboards, lists := 0, 0, 0
	for _, raw := range *input.Grants {
		if raw.Scope == nil || raw.Objects == nil {
			return PolicySource{}, invalidSource("grants")
		}
		permission, err := admitPermission(*raw.Scope)
		if err != nil {
			return PolicySource{}, invalidSource("grants.scope")
		}
		if _, duplicate := seen[permission]; duplicate {
			return PolicySource{}, invalidSource("grants.scope")
		}
		seen[permission] = struct{}{}
		kind := configurable(permission)
		if kind == objectInvalid {
			return PolicySource{}, invalidSource("grants.scope")
		}
		selectors := 0
		if raw.Objects.Entities != nil {
			selectors++
		}
		if raw.Objects.Dashboards != nil {
			selectors++
		}
		if raw.Objects.Lists != nil {
			selectors++
		}
		if selectors != 1 {
			return PolicySource{}, invalidSource("grants.objects")
		}
		value := grant{permission: permission, object: kind}
		policy.cost += 128
		switch kind {
		case objectEntity:
			if raw.Objects.Entities == nil {
				return PolicySource{}, invalidSource("grants.objects")
			}
			selection := raw.Objects.Entities
			if (selection.All == nil) == (selection.Explicit == nil) {
				return PolicySource{}, invalidSource("grants.objects.entities")
			}
			if selection.All != nil {
				value.kind = selectionAll
				value.allSlots, err = admitSlots(selection.All.Capabilities)
				if err != nil {
					return PolicySource{}, err
				}
				policy.cost += slotCost(value.allSlots)
			} else {
				value.kind = selectionExplicit
				entities += len(*selection.Explicit)
				if entities > MaxEntities {
					return PolicySource{}, invalidSource("grants.objects.entities.explicit")
				}
				value.entities = make(map[domain.EntityID]slots, len(*selection.Explicit))
				seenEntities := make(map[domain.EntityID]struct{}, len(*selection.Explicit))
				for _, entity := range *selection.Explicit {
					id, parseErr := domain.ParseEntityID(entity.ID)
					if parseErr != nil {
						return PolicySource{}, invalidSource("grants.objects.entities.entity_id")
					}
					if _, duplicate := seenEntities[id]; duplicate {
						return PolicySource{}, invalidSource("grants.objects.entities.entity_id")
					}
					seenEntities[id] = struct{}{}
					allowed, slotErr := admitSlots(entity.Capabilities)
					if slotErr != nil {
						return PolicySource{}, slotErr
					}
					if allowed.kind == selectionExplicit && len(allowed.explicit) == 0 {
						continue
					}
					value.entities[id] = allowed
					policy.cost += 128 + len(entity.ID) + slotCost(allowed)
				}
			}
		case objectDashboard:
			if raw.Objects.Dashboards == nil {
				return PolicySource{}, invalidSource("grants.objects")
			}
			selection := raw.Objects.Dashboards
			count := 0
			if selection.All != nil {
				count++
			}
			if selection.Own != nil {
				count++
			}
			if selection.Explicit != nil {
				count++
			}
			if count != 1 {
				return PolicySource{}, invalidSource("grants.objects.dashboards")
			}
			switch {
			case selection.All != nil:
				value.kind = selectionAll
			case selection.Own != nil:
				value.kind = selectionOwn
			case selection.Explicit != nil:
				value.kind = selectionExplicit
				dashboards += len(*selection.Explicit)
				if dashboards > MaxDashboards {
					return PolicySource{}, invalidSource("grants.objects.dashboards.explicit")
				}
				value.dashboards = make(map[DashboardID]struct{}, len(*selection.Explicit))
				for _, text := range *selection.Explicit {
					id, parseErr := ParseDashboardID(text)
					if parseErr != nil {
						return PolicySource{}, invalidSource("grants.objects.dashboards.explicit")
					}
					if _, duplicate := value.dashboards[id]; duplicate {
						return PolicySource{}, invalidSource("grants.objects.dashboards.explicit")
					}
					value.dashboards[id] = struct{}{}
					policy.cost += 80 + len(text)
				}
			}
		case objectList:
			if raw.Objects.Lists == nil {
				return PolicySource{}, invalidSource("grants.objects")
			}
			selection := raw.Objects.Lists
			if (selection.All == nil) == (selection.Explicit == nil) {
				return PolicySource{}, invalidSource("grants.objects.lists")
			}
			if selection.All != nil {
				value.kind = selectionAll
			} else {
				value.kind = selectionExplicit
				lists += len(*selection.Explicit)
				if lists > MaxLists {
					return PolicySource{}, invalidSource("grants.objects.lists.explicit")
				}
				value.lists = make(map[domain.ListID]struct{}, len(*selection.Explicit))
				for _, text := range *selection.Explicit {
					id, parseErr := domain.ParseListID(text)
					if parseErr != nil {
						return PolicySource{}, invalidSource("grants.objects.lists.explicit")
					}
					if _, duplicate := value.lists[id]; duplicate {
						return PolicySource{}, invalidSource("grants.objects.lists.explicit")
					}
					value.lists[id] = struct{}{}
					policy.cost += 80 + len(text)
				}
			}
		case objectAccounts, objectIntegrations, objectGlobalConfiguration, objectInvalid:
			return PolicySource{}, invalidSource("grants.objects")
		default:
			return PolicySource{}, invalidSource("grants.objects")
		}
		if !emptyGrant(value) {
			policy.grants = append(policy.grants, value)
		}
	}
	policy.semantic = semanticHash(policy)
	return PolicySource{policy: policy, bytes: owned, hash: sha256.Sum256(owned)}, nil
}

func emptyGrant(value grant) bool {
	switch value.object {
	case objectEntity:
		return value.kind == selectionExplicit && len(value.entities) == 0 || value.kind == selectionAll && value.allSlots.kind == selectionExplicit && len(value.allSlots.explicit) == 0
	case objectDashboard:
		return value.kind == selectionExplicit && len(value.dashboards) == 0
	case objectList:
		return value.kind == selectionExplicit && len(value.lists) == 0
	case objectInvalid, objectAccounts, objectIntegrations, objectGlobalConfiguration:
		return true
	default:
		return true
	}
}

func safeNode(node *yaml.Node, depth int) bool {
	if node == nil || depth > MaxDepth || node.Anchor != "" || node.Kind == yaml.AliasNode {
		return false
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		if node.Kind == yaml.SequenceNode && node.Tag != "!!seq" {
			return false
		}
	case yaml.MappingNode:
		if node.Tag != "!!map" || len(node.Content)%2 != 0 {
			return false
		}
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return false
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return false
			}
			seen[key.Value] = struct{}{}
		}
	case yaml.ScalarNode:
		return node.Tag == "!!str"
	default:
		return false
	}
	for _, child := range node.Content {
		if !safeNode(child, depth+1) {
			return false
		}
	}
	return true
}

func admitPermission(raw permissionInput) (Permission, error) {
	if !scopeToken(raw.Family) || !scopeToken(raw.Operation) {
		return Permission{}, ErrInvalid
	}
	family, ok := authv1.ScopeFamily_value["SCOPE_FAMILY_"+strings.ToUpper(raw.Family)]
	if !ok {
		return Permission{}, ErrInvalid
	}
	operation, ok := authv1.ScopeOperation_value["SCOPE_OPERATION_"+strings.ToUpper(raw.Operation)]
	if !ok {
		return Permission{}, ErrInvalid
	}
	return NewPermission(authv1.ScopeFamily(family), authv1.ScopeOperation(operation))
}
func scopeToken(text string) bool {
	if text == "" {
		return false
	}
	for _, char := range text {
		if (char < 'a' || char > 'z') && char != '_' {
			return false
		}
	}
	return true
}
func admitSlots(raw *slotsInput) (slots, error) {
	if raw == nil || (raw.All == nil) == (raw.Explicit == nil) {
		return slots{}, invalidSource("grants.objects.capabilities")
	}
	if raw.All != nil {
		return slots{kind: selectionAll}, nil
	}
	if len(*raw.Explicit) > MaxSlots {
		return slots{}, invalidSource("grants.objects.capabilities.explicit")
	}
	value := slots{kind: selectionExplicit, explicit: make(map[CapabilitySlot]struct{}, len(*raw.Explicit))}
	for _, pair := range *raw.Explicit {
		slot, err := NewCapabilitySlot(pair.Schema, pair.Endpoint)
		if err != nil {
			return slots{}, invalidSource("grants.objects.capabilities.explicit")
		}
		if _, duplicate := value.explicit[slot]; duplicate {
			return slots{}, invalidSource("grants.objects.capabilities.explicit")
		}
		value.explicit[slot] = struct{}{}
	}
	return value, nil
}
func slotCost(value slots) int {
	cost := 64
	for slot := range value.explicit {
		cost += 96 + len(slot.schema) + len(slot.endpoint)
	}
	return cost
}
