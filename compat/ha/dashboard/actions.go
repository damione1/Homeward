package dashboard

import (
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

func parseActionKind(text string) ActionKind {
	switch text {
	case "none":
		return ActionNone
	case "more-info":
		return ActionMoreInfo
	case "navigate":
		return ActionNavigate
	case "toggle":
		return ActionToggle
	case "perform-action":
		return ActionPerform
	default:
		return ActionInvalid
	}
}

func (c *compiler) action(gesture Gesture, node parser.Node, present bool, owner parser.Node, bound domain.HAAlias, hasBound bool) Action {
	if !present {
		return Action{gesture: gesture, kind: ActionAbsent, status: StatusSupported, location: locate(owner)}
	}
	a := Action{gesture: gesture, status: StatusSupported, location: locate(node)}
	if node.Kind() != parser.KindMapping {
		c.unsupported(CodeInvalidValue, node, "action must be a mapping")
		a.status = StatusUnsupported
		return a
	}
	typeNode, hasType := node.Lookup("action")
	text, ok := scalarText(typeNode)
	if !hasType || !ok {
		c.unsupported(CodeMissingField, node, "action variant is required")
		a.status = StatusUnsupported
		return a
	}
	a.kind = parseActionKind(text)
	if a.kind == ActionInvalid {
		c.unsupported(CodeUnsupportedVariant, node, "action variant outside inert profile")
		a.status = StatusUnsupported
		return a
	}
	blocked, hasNavigation, hasService, hasTarget := false, false, false, false
	for _, entry := range node.Entries() {
		if entry.Key == "action" {
			continue
		}
		switch entry.Key {
		case "entity":
			if a.kind != ActionMoreInfo {
				c.unknown(entry, "entity outside action profile")
				blocked = true
				continue
			}
			value, valid := scalarText(entry.Value)
			alias, err := domain.ParseHAAlias(value)
			if !valid || err != nil {
				c.unsupported(CodeInvalidValue, entry.Value, "invalid more-info entity alias")
				blocked = true
			} else {
				a.entity, a.hasEntity = alias, true
			}
		case "navigation_path":
			if a.kind != ActionNavigate {
				c.unknown(entry, "navigation path outside action profile")
				blocked = true
				continue
			}
			hasNavigation = true
			value, valid := scalarText(entry.Value)
			if !valid || !validNavigation(value) {
				c.unsupported(CodeInvalidValue, entry.Value, "navigation path must remain local")
				blocked = true
			} else {
				a.navigation = value
			}
		case "perform_action":
			if a.kind != ActionPerform {
				c.unknown(entry, "service outside action profile")
				blocked = true
				continue
			}
			hasService = true
			value, valid := scalarText(entry.Value)
			service, err := domain.ParseActionID(value)
			if !valid || !validService(value) || err != nil {
				c.unsupported(CodeInvalidValue, entry.Value, "service must be domain.action")
				blocked = true
			} else {
				a.service = service
			}
		case "target":
			if a.kind != ActionPerform {
				c.unknown(entry, "target outside action profile")
				blocked = true
				continue
			}
			hasTarget = true
			alias, valid := c.actionTarget(entry.Value)
			if !valid {
				blocked = true
			} else {
				a.entity, a.hasEntity = alias, true
			}
		case "confirmation":
			if a.kind == ActionNone {
				c.unknown(entry, "confirmation outside none action profile")
				blocked = true
				continue
			}
			value, valid := c.confirmation(entry.Value)
			if !valid {
				blocked = true
			} else {
				a.confirmation, a.hasConfirmation = value, true
			}
		default:
			c.unknown(entry, "action property outside profile")
			blocked = true
		}
	}
	switch a.kind {
	case ActionMoreInfo:
		if !a.hasEntity && hasBound {
			a.entity, a.hasEntity = bound, true
		}
		if !a.hasEntity {
			c.unsupported(CodeMissingField, node, "more-info requires an entity")
			blocked = true
		}
	case ActionNavigate:
		if !hasNavigation {
			c.unsupported(CodeMissingField, node, "navigate requires navigation_path")
			blocked = true
		}
	case ActionToggle:
		if !hasBound {
			c.unsupported(CodeMissingField, node, "toggle requires a card entity")
			blocked = true
		} else {
			a.entity, a.hasEntity = bound, true
		}
	case ActionPerform:
		if !hasService || !hasTarget {
			c.unsupported(CodeMissingField, node, "perform-action requires service and entity target")
			blocked = true
		}
	}
	if blocked {
		a.status = StatusUnsupported
	}
	return a
}

func validService(value string) bool {
	domainName, action, ok := strings.Cut(value, ".")
	return ok && !strings.Contains(action, ".") && validServicePart(domainName) && validServicePart(action)
}

func validServicePart(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' {
			continue
		}
		return false
	}
	return true
}

func (c *compiler) actionTarget(node parser.Node) (domain.HAAlias, bool) {
	if node.Kind() != parser.KindMapping {
		c.unsupported(CodeInvalidValue, node, "target must be a mapping")
		return domain.HAAlias{}, false
	}
	entries := node.Entries()
	if len(entries) != 1 || entries[0].Key != "entity_id" {
		c.unsupported(CodeUnsupportedProperty, node, "only one entity_id target is admitted")
		return domain.HAAlias{}, false
	}
	value, valid := scalarText(entries[0].Value)
	alias, err := domain.ParseHAAlias(value)
	if !valid || err != nil {
		c.unsupported(CodeInvalidValue, entries[0].Value, "invalid target entity alias")
		return domain.HAAlias{}, false
	}
	return alias, true
}

func (c *compiler) confirmation(node parser.Node) (string, bool) {
	if node.Kind() != parser.KindMapping {
		c.unsupported(CodeInvalidValue, node, "confirmation must be a mapping")
		return "", false
	}
	var text string
	valid := true
	for _, entry := range node.Entries() {
		if entry.Key != "text" {
			c.unknown(entry, "confirmation property outside profile")
			valid = false
			continue
		}
		value, ok := scalarText(entry.Value)
		if !ok || len(value) > 512 {
			c.unsupported(CodeInvalidValue, entry.Value, "confirmation text must be a bounded string")
			valid = false
		} else {
			text = value
		}
	}
	return text, valid
}
