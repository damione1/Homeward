package dashboard

import (
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

func parseRecipeKind(variant string) RecipeKind {
	switch variant {
	case "custom:mushroom-template-card", "custom:mushroom-template-badge":
		return RecipeTemplate
	case "custom:mushroom-select-card":
		return RecipeSelect
	case "custom:mushroom-alarm-control-panel-card":
		return RecipeAlarm
	case "custom:mushroom-chips-card":
		return RecipeChips
	case "template":
		return RecipeTemplateChip
	case "button":
		return RecipeButton
	case "entity":
		return RecipeEntity
	case "markdown":
		return RecipeText
	}
	return RecipeInvalid
}

func staticText(node parser.Node, limit int) (string, bool) {
	text, ok := scalarText(node)
	return text, ok && len(text) <= limit && !strings.ContainsRune(text, '\x00') &&
		!strings.Contains(text, "{{") && !strings.Contains(text, "{%") && !strings.Contains(text, "[[[")
}

func (c *compiler) recipeCard(node parser.Node, variant string, depth int) Card {
	card := Card{kind: ElementRecipeCard, recipe: parseRecipeKind(variant), variant: variant, status: StatusSupported, location: locate(node)}
	if variant == "vertical-stack" {
		card.kind = ElementVerticalStackCard
	}
	if variant == "custom:bubble-card" {
		kind, _ := node.Lookup("card_type")
		text, _ := scalarText(kind)
		switch text {
		case "button":
			card.recipe = RecipeBubbleButton
		case "pop-up":
			card.recipe, card.kind = RecipeBubblePopup, ElementPopupCard
		default:
			c.unsupported(CodeUnsupportedVariant, node, "Bubble card_type outside the native recipe profile")
			card.status = StatusUnsupported
			return card
		}
	}
	if card.recipe == RecipeChips {
		card.kind = ElementChipGroup
	}
	if card.recipe == RecipeInvalid && card.kind != ElementVerticalStackCard {
		c.unsupported(CodeUnsupportedVariant, node, "card type outside native recipe profile")
		card.kind, card.status = ElementInvalid, StatusUnsupported
		return card
	}
	blocked, hasChildren := false, false
	actions := map[Gesture]parser.Node{}
	setAction := func(gesture Gesture, value parser.Node) {
		if _, duplicate := actions[gesture]; duplicate {
			c.unsupported(CodeInvalidValue, value, "ambiguous direct and nested gesture actions")
			blocked = true
		}
		actions[gesture] = value
	}
	for _, entry := range node.Entries() {
		switch entry.Key {
		case "type":
		case "card_type":
			if variant != "custom:bubble-card" {
				c.unknown(entry, "card_type outside Bubble recipe")
				blocked = true
			}
		case "entity":
			text, ok := scalarText(entry.Value)
			alias, err := domain.ParseHAAlias(text)
			if !ok || err != nil {
				c.unsupported(CodeInvalidValue, entry.Value, "invalid recipe entity alias")
				blocked = true
			} else {
				card.entity, card.hasEntity = alias, true
			}
		case "name", "primary", "content":
			// Markdown content is a plain-text native fallback. HTML is never
			// parsed and template delimiters require a separate server profile.
			limit := 256
			if card.recipe == RecipeText {
				limit = 4096
			}
			text, valid := staticText(entry.Value, limit)
			if !valid || card.hasName {
				c.unsupported(CodeUnsupportedProperty, entry.Value, "recipe label must be a single bounded static value; templates need server projection")
				blocked = true
			} else {
				card.name, card.hasName = text, true
			}
		case "secondary":
			text, valid := staticText(entry.Value, 512)
			if !valid {
				c.unsupported(CodeUnsupportedProperty, entry.Value, "secondary template outside server-projected profile")
				blocked = true
			} else {
				card.secondary = text
			}
		case "icon":
			text, valid := staticText(entry.Value, 256)
			if !valid {
				c.unsupported(CodeUnsupportedProperty, entry.Value, "icon template outside server-projected profile")
				blocked = true
			} else {
				card.icon = text
			}
		case "hash":
			text, valid := scalarText(entry.Value)
			anchor, err := ParsePopupAnchor(text)
			if card.kind != ElementPopupCard || !valid || err != nil {
				c.unsupported(CodeInvalidValue, entry.Value, "popup hash must be one local anchor")
				blocked = true
			} else {
				card.anchor = anchor
			}
		case "cards", "chips":
			allowed := entry.Key == "cards" && (card.kind == ElementPopupCard || card.kind == ElementVerticalStackCard) || entry.Key == "chips" && card.kind == ElementChipGroup
			if !allowed || entry.Value.Kind() != parser.KindSequence || hasChildren {
				c.unsupported(CodeUnsupportedProperty, entry.Value, "children outside recipe container profile")
				blocked = true
				continue
			}
			hasChildren = true
			for _, child := range entry.Value.Items() {
				if c.overflow {
					break
				}
				card.children = append(card.children, c.card(child, depth+1))
			}
		case "tap_action":
			setAction(GestureTap, entry.Value)
		case "hold_action":
			setAction(GestureHold, entry.Value)
		case "double_tap_action":
			setAction(GestureDoubleTap, entry.Value)
		case "button_action":
			if card.recipe != RecipeBubbleButton || entry.Value.Kind() != parser.KindMapping {
				c.unsupported(CodeUnsupportedProperty, entry.Value, "nested button actions outside Bubble button profile")
				blocked = true
				continue
			}
			for _, action := range entry.Value.Entries() {
				switch action.Key {
				case "tap_action":
					setAction(GestureTap, action.Value)
				case "hold_action":
					setAction(GestureHold, action.Value)
				case "double_tap_action":
					setAction(GestureDoubleTap, action.Value)
				default:
					c.unknown(action, "nested button action outside native profile")
					blocked = true
				}
			}
		case "sub_button":
			if variant != "custom:bubble-card" || !emptySubButtons(entry.Value) {
				c.unsupported(CodeUnsupportedProperty, entry.Value, "nonempty Bubble sub-buttons require an explicit native recipe")
				blocked = true
			}
		case "visibility":
			values, invalid := c.visibility(entry.Value)
			card.visibility = append(card.visibility, values...)
			blocked = blocked || invalid
		case "grid_options":
			options, invalid := c.gridOptions(entry.Value)
			card.grid, blocked = options, blocked || invalid
		default:
			if handled, invalid := c.cardExtension(&card, entry); handled {
				blocked = blocked || invalid
				continue
			}
			c.unknown(entry, "recipe property outside native profile")
			blocked = true
		}
	}
	if (card.recipe == RecipeSelect || card.recipe == RecipeAlarm || card.recipe == RecipeEntity) && !card.hasEntity {
		c.unsupported(CodeMissingField, node, "recipe requires an entity binding")
		blocked = true
	}
	if _, valid := card.Anchor(); card.kind == ElementPopupCard && !valid {
		c.unsupported(CodeMissingField, node, "popup requires an explicit local anchor")
		blocked = true
	}
	if (card.kind == ElementPopupCard || card.kind == ElementVerticalStackCard || card.kind == ElementChipGroup) && !hasChildren {
		c.unsupported(CodeMissingField, node, "recipe container requires children")
		blocked = true
	}
	for _, gesture := range []Gesture{GestureTap, GestureHold, GestureDoubleTap} {
		value, present := actions[gesture]
		card.actions = append(card.actions, c.action(gesture, value, present, node, card.entity, card.hasEntity))
	}
	for _, action := range card.actions {
		blocked = blocked || action.Status() != StatusSupported
	}
	if blocked {
		disableCard(&card)
	}
	return card
}

func emptySubButtons(node parser.Node) bool {
	if node.Kind() == parser.KindSequence {
		return len(node.Items()) == 0
	}
	if node.Kind() != parser.KindMapping {
		return false
	}
	for _, entry := range node.Entries() {
		if (entry.Key != "main" && entry.Key != "bottom") || entry.Value.Kind() != parser.KindSequence || len(entry.Value.Items()) != 0 {
			return false
		}
	}
	return true
}

func parseFeatureKind(value string) FeatureKind {
	switch value {
	case "toggle":
		return FeatureToggle
	case "light-brightness":
		return FeatureBrightness
	case "light-color-temp":
		return FeatureColorTemperature
	case "target-temperature":
		return FeatureTargetTemperature
	case "climate-hvac-modes":
		return FeatureClimateModes
	case "fan-speed":
		return FeatureFanSpeed
	case "numeric-input":
		return FeatureNumericInput
	case "cover-open-close":
		return FeatureCoverOpenClose
	case "cover-position":
		return FeatureCoverPosition
	case "alarm-modes":
		return FeatureAlarmModes
	}
	return FeatureInvalid
}

func (c *compiler) cardExtension(card *Card, entry parser.Entry) (bool, bool) {
	switch entry.Key {
	case "features":
		if card.kind != ElementTileCard || entry.Value.Kind() != parser.KindSequence || len(entry.Value.Items()) > 16 {
			c.unsupported(CodeInvalidValue, entry.Value, "features require a bounded tile feature list")
			return true, true
		}
		invalid := false
		for _, node := range entry.Value.Items() {
			kindNode, _ := node.Lookup("type")
			text, valid := scalarText(kindNode)
			kind := parseFeatureKind(text)
			if !valid || kind == FeatureInvalid {
				c.unsupported(CodeUnsupportedVariant, node, "feature requires its specialized native ticket or a known instance capability")
				invalid = true
				continue
			}
			for _, property := range node.Entries() {
				if property.Key != "type" {
					c.unknown(property, "feature property outside the admitted recipe profile")
					invalid = true
				}
			}
			card.features = append(card.features, Feature{kind: kind, location: locate(node)})
		}
		return true, invalid
	case "color", "icon_color", "layout", "fill_container", "multiline_secondary", "secondary_info", "features_position", "show_entity_picture", "state_content", "button_type", "show_state", "auto_close", "card_mod", "styles":
		c.unsupported(CodeUnsupportedProperty, entry.Value, "HA presentation/CSS/hooks are retained but not executed; native presentation uses its own recipe")
		return true, false
	}
	return false, false
}
