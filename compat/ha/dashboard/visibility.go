package dashboard

import (
	"unicode/utf8"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

func (c *compiler) visibility(node parser.Node) ([]Visibility, bool) {
	if node.Kind() != parser.KindSequence {
		c.unsupported(CodeInvalidValue, node, "visibility must be a sequence")
		return nil, true
	}
	if len(node.Items()) == 0 {
		c.unsupported(CodeInvalidValue, node, "visibility condition list must be nonempty")
		return nil, true
	}
	invalid := false
	values := make([]Visibility, 0, len(node.Items()))
	for _, child := range node.Items() {
		c.conditions++
		if c.conditions > maxConditions {
			c.overflow = true
			break
		}
		value := c.condition(child)
		values = append(values, value)
		if value.status != StatusSupported {
			invalid = true
		}
	}
	return values, invalid
}

func (c *compiler) condition(node parser.Node) Visibility {
	v := Visibility{status: StatusSupported, location: locate(node)}
	if node.Kind() != parser.KindMapping {
		c.unsupported(CodeInvalidValue, node, "condition must be a mapping")
		v.status = StatusUnsupported
		return v
	}
	variant, hasVariant := node.Lookup("condition")
	name, ok := scalarText(variant)
	if !hasVariant || !ok {
		c.unsupported(CodeMissingField, node, "visibility condition kind is required")
		v.status = StatusUnsupported
		return v
	}
	switch name {
	case "state":
		v.kind = VisibilityState
	case "user":
		v.kind = VisibilityHAUser
	default:
		c.unsupported(CodeUnsupportedVariant, node, "visibility condition outside profile")
		v.status = StatusUnsupported
		return v
	}
	blocked := false
	var hasEntity, hasState, hasNot, hasUsers bool
	for _, entry := range node.Entries() {
		switch entry.Key {
		case "condition":
		case "entity", "entity_id":
			if v.kind != VisibilityState || hasEntity {
				c.unknown(entry, "entity outside state condition profile")
				blocked = true
				continue
			}
			hasEntity = true
			text, valid := scalarText(entry.Value)
			alias, err := domain.ParseHAAlias(text)
			if !valid || err != nil {
				c.unsupported(CodeInvalidValue, entry.Value, "invalid condition entity alias")
				blocked = true
			} else {
				v.entity = alias
			}
		case "state", "state_not":
			if v.kind != VisibilityState {
				c.unknown(entry, "state outside condition profile")
				blocked = true
				continue
			}
			text, valid := scalarText(entry.Value)
			if !valid || len(text) > 256 {
				c.unsupported(CodeInvalidValue, entry.Value, "state condition requires a bounded string")
				blocked = true
			} else {
				v.state = text
			}
			if entry.Key == "state" {
				hasState = true
			} else {
				hasNot = true
				v.stateNot = true
			}
		case "users":
			if v.kind != VisibilityHAUser {
				c.unknown(entry, "users outside user condition profile")
				blocked = true
				continue
			}
			hasUsers = true
			if entry.Value.Kind() != parser.KindSequence || len(entry.Value.Items()) == 0 || len(entry.Value.Items()) > 64 {
				c.unsupported(CodeInvalidValue, entry.Value, "HA user list must contain 1..64 IDs")
				blocked = true
				continue
			}
			for _, userNode := range entry.Value.Items() {
				text, valid := scalarText(userNode)
				if !valid || text == "" || len(text) > 128 || !utf8.ValidString(text) {
					c.unsupported(CodeInvalidValue, userNode, "HA user ID must be a bounded string")
					blocked = true
					continue
				}
				v.users = append(v.users, text)
			}
		default:
			c.unknown(entry, "visibility property outside profile")
			blocked = true
		}
	}
	if v.kind == VisibilityState && (!hasEntity || hasState == hasNot) {
		c.unsupported(CodeMissingField, node, "state condition needs one entity and exactly one state test")
		blocked = true
	}
	if v.kind == VisibilityHAUser {
		if !hasUsers {
			c.unsupported(CodeMissingField, node, "HA user condition needs users")
			blocked = true
		}
		// HA user IDs are not Homeward account IDs. A later explicit mapping is
		// required before this predicate can be used to display a branch.
		c.unsupported(CodeUnboundHAUser, node, "HA user visibility needs explicit account mapping")
		blocked = true
	}
	if blocked {
		v.status = StatusUnsupported
	}
	return v
}
