package dashboard

import (
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

func parseCardKind(text string) ElementKind {
	switch text {
	case "tile":
		return ElementTileCard
	case "heading":
		return ElementHeadingCard
	case "grid":
		return ElementGridCard
	case "horizontal-stack":
		return ElementHorizontalStackCard
	default:
		return ElementInvalid
	}
}

func (c *compiler) card(node parser.Node, depth int) Card {
	c.cards++
	card := Card{status: StatusSupported, location: locate(node)}
	if c.cards > maxCards || depth > maxCardDepth {
		c.overflow = true
		card.status = StatusUnsupported
		return card
	}
	if node.Kind() != parser.KindMapping {
		c.unsupported(CodeInvalidValue, node, "card must be a mapping")
		card.status = StatusUnsupported
		return card
	}
	typeNode, hasType := node.Lookup("type")
	typeText, typeOK := scalarText(typeNode)
	if !hasType || !typeOK {
		c.unsupported(CodeMissingField, node, "card type is required")
		card.status = StatusUnsupported
		return card
	}
	card.kind = parseCardKind(typeText)
	card.variant = typeText
	if c.profile == CompilationRecipes {
		if card.kind == ElementInvalid {
			return c.recipeCard(node, typeText, depth)
		}
	}
	if card.kind == ElementInvalid {
		c.unsupported(CodeUnsupportedVariant, node, "card type outside native profile")
		card.status = StatusUnsupported
		return card
	}
	if card.kind == ElementHeadingCard {
		card.style = HeadingStyleDefault
	}
	blocked := false
	var hasEntity, hasHeading, hasCards bool
	var tap, hold, double parser.Node
	var hasTap, hasHold, hasDouble bool
	for _, entry := range node.Entries() {
		if entry.Key == "type" {
			continue
		}
		switch entry.Key {
		case "grid_options":
			options, invalid := c.gridOptions(entry.Value)
			card.grid = options
			blocked = blocked || invalid
		case "visibility":
			values, invalid := c.visibility(entry.Value)
			card.visibility = append(card.visibility, values...)
			blocked = blocked || invalid
		case "entity":
			if card.kind != ElementTileCard {
				c.unknown(entry, "entity outside card profile")
				blocked = true
				continue
			}
			hasEntity = true
			text, ok := scalarText(entry.Value)
			alias, err := domain.ParseHAAlias(text)
			if !ok || err != nil {
				c.unsupported(CodeInvalidValue, entry.Value, "invalid tile entity alias")
				blocked = true
			} else {
				card.entity, card.hasEntity = alias, true
			}
		case "name":
			if card.kind != ElementTileCard {
				c.unknown(entry, "name outside card profile")
				blocked = true
				continue
			}
			text, ok := scalarText(entry.Value)
			if !ok || len(text) > 256 {
				c.unsupported(CodeInvalidValue, entry.Value, "tile name must be a bounded string")
				blocked = true
			} else {
				card.name, card.hasName = text, true
			}
		case "heading":
			if card.kind != ElementHeadingCard {
				c.unknown(entry, "heading outside card profile")
				blocked = true
				continue
			}
			hasHeading = true
			text, ok := scalarText(entry.Value)
			if !ok || text == "" || len(text) > 256 {
				c.unsupported(CodeInvalidValue, entry.Value, "heading must be a bounded nonempty string")
				blocked = true
			} else {
				card.heading = text
			}
		case "heading_style":
			if card.kind != ElementHeadingCard {
				c.unknown(entry, "heading style outside card profile")
				blocked = true
				continue
			}
			text, ok := scalarText(entry.Value)
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "heading style must be a string")
				blocked = true
				continue
			}
			switch text {
			case "title":
				card.style = HeadingStyleTitle
			case "subtitle":
				card.style = HeadingStyleSubtitle
			default:
				c.unsupported(CodeUnsupportedVariant, entry.Value, "heading style outside profile")
				blocked = true
			}
		case "icon":
			if card.kind != ElementTileCard && card.kind != ElementHeadingCard {
				c.unknown(entry, "icon outside card profile")
				blocked = true
				continue
			}
			text, ok := scalarText(entry.Value)
			if !ok || text == "" || len(text) > 256 {
				c.unsupported(CodeInvalidValue, entry.Value, "icon must be a bounded string")
				blocked = true
			} else {
				card.icon = text
			}
		case "vertical":
			if card.kind != ElementTileCard {
				c.unknown(entry, "vertical outside card profile")
				blocked = true
				continue
			}
			value, ok := scalarBool(entry.Value)
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "vertical must be boolean")
				blocked = true
			} else {
				card.vertical, card.hasVertical = value, true
			}
		case "hide_state":
			if card.kind != ElementTileCard {
				c.unknown(entry, "hide_state outside card profile")
				blocked = true
				continue
			}
			value, ok := scalarBool(entry.Value)
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "hide_state must be boolean")
				blocked = true
			} else {
				card.hideState, card.hasHideState = value, true
			}
		case "title":
			if card.kind != ElementGridCard && card.kind != ElementHorizontalStackCard {
				c.unknown(entry, "title outside stack card profile")
				blocked = true
				continue
			}
			text, ok := scalarText(entry.Value)
			if !ok || len(text) > 256 {
				c.unsupported(CodeInvalidValue, entry.Value, "stack title must be a bounded string")
				blocked = true
			} else {
				card.title, card.hasTitle = text, true
			}
		case "columns":
			if card.kind != ElementGridCard {
				c.unknown(entry, "columns outside grid card profile")
				blocked = true
				continue
			}
			count, ok := positiveCount(entry.Value)
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "grid columns must be 1..12")
				blocked = true
			} else {
				card.columns = count
			}
		case "square":
			if card.kind != ElementGridCard {
				c.unknown(entry, "square outside grid card profile")
				blocked = true
				continue
			}
			value, ok := scalarBool(entry.Value)
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "square must be boolean")
				blocked = true
			} else {
				card.square, card.hasSquare = value, true
			}
		case "cards":
			if card.kind != ElementGridCard && card.kind != ElementHorizontalStackCard {
				c.unknown(entry, "cards outside container profile")
				blocked = true
				continue
			}
			hasCards = true
			if entry.Value.Kind() != parser.KindSequence {
				c.unsupported(CodeInvalidValue, entry.Value, "container cards must be a sequence")
				blocked = true
				continue
			}
			for _, child := range entry.Value.Items() {
				if c.overflow {
					break
				}
				card.children = append(card.children, c.card(child, depth+1))
			}
		case "tap_action":
			if card.kind != ElementTileCard && card.kind != ElementHeadingCard {
				c.unknown(entry, "tap action outside card profile")
				blocked = true
				continue
			}
			tap, hasTap = entry.Value, true
		case "hold_action":
			if card.kind != ElementTileCard {
				c.unknown(entry, "hold action outside tile profile")
				blocked = true
				continue
			}
			hold, hasHold = entry.Value, true
		case "double_tap_action":
			if card.kind != ElementTileCard {
				c.unknown(entry, "double tap action outside tile profile")
				blocked = true
				continue
			}
			double, hasDouble = entry.Value, true
		default:
			if c.profile == CompilationRecipes {
				if handled, invalid := c.cardExtension(&card, entry); handled {
					blocked = blocked || invalid
					continue
				}
			}
			c.unknown(entry, "card property outside profile")
			blocked = true
		}
	}
	if card.kind == ElementTileCard && !hasEntity {
		c.unsupported(CodeMissingField, node, "tile entity is required")
		blocked = true
	}
	if card.kind == ElementHeadingCard && !hasHeading {
		c.unsupported(CodeMissingField, node, "heading text is required")
		blocked = true
	}
	if (card.kind == ElementGridCard || card.kind == ElementHorizontalStackCard) && !hasCards {
		c.unsupported(CodeMissingField, node, "container cards are required")
		blocked = true
	}
	if card.kind == ElementTileCard {
		card.actions = []Action{
			c.action(GestureTap, tap, hasTap, node, card.entity, card.hasEntity),
			c.action(GestureHold, hold, hasHold, node, card.entity, card.hasEntity),
			c.action(GestureDoubleTap, double, hasDouble, node, card.entity, card.hasEntity),
		}
	} else if card.kind == ElementHeadingCard {
		card.actions = []Action{c.action(GestureTap, tap, hasTap, node, domain.HAAlias{}, false)}
	}
	for _, action := range card.actions {
		if action.status != StatusSupported {
			blocked = true
		}
	}
	if blocked {
		disableCard(&card)
	}
	return card
}

func (c *compiler) gridOptions(node parser.Node) (GridOptions, bool) {
	var options GridOptions
	if node.Kind() != parser.KindMapping {
		c.unsupported(CodeInvalidValue, node, "grid_options must be a mapping")
		return options, true
	}
	invalid := false
	for _, entry := range node.Entries() {
		switch entry.Key {
		case "columns":
			value, ok := gridDimension(entry.Value, "full")
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "grid columns must be 1..12 or full")
				invalid = true
			} else {
				options.columns, options.hasColumns = value, true
			}
		case "rows":
			value, ok := gridDimension(entry.Value, "auto")
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "grid rows must be 1..12 or auto")
				invalid = true
			} else {
				options.rows, options.hasRows = value, true
			}
		default:
			c.unknown(entry, "grid option outside profile")
			invalid = true
		}
	}
	return options, invalid
}

func gridDimension(node parser.Node, keyword string) (Dimension, bool) {
	if count, ok := positiveCount(node); ok {
		return Dimension{kind: DimensionCount, count: count}, true
	}
	if text, ok := scalarText(node); ok && text == keyword {
		if keyword == "full" {
			return Dimension{kind: DimensionFull}, true
		}
		return Dimension{kind: DimensionAuto}, true
	}
	return Dimension{}, false
}
