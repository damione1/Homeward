package dashboard

import "github.com/damione1/Homeward/compat/ha/parser"

func (c *compiler) viewExtension(view *View, entry parser.Entry) (bool, bool) {
	var values []parser.Node
	switch entry.Key {
	case "subview", "dense_section_placement", "top_margin":
		if _, valid := scalarBool(entry.Value); !valid {
			c.unsupported(CodeInvalidValue, entry.Value, "HA view presentation option must be a boolean")
			return true, true
		}
		c.unsupported(CodeUnsupportedProperty, entry.Value, "HA view presentation is adapted to native layout and navigation")
		return true, false
	case "theme", "background":
		c.unsupported(CodeUnsupportedProperty, entry.Value, "HA theme/background is retained; native theme is selected by the client")
		return true, false
	case "cards", "badges":
		if entry.Value.Kind() != parser.KindSequence {
			c.unsupported(CodeInvalidValue, entry.Value, "view cards/badges must be a sequence")
			return true, true
		}
		values = entry.Value.Items()
	case "header", "footer":
		if entry.Value.Kind() != parser.KindMapping {
			c.unsupported(CodeInvalidValue, entry.Value, "view header/footer must be a mapping")
			return true, true
		}
		for _, property := range entry.Value.Entries() {
			switch property.Key {
			case "card":
				values = append(values, property.Value)
			case "layout", "badges_position", "badges_wrap", "max_width":
				c.unsupported(CodeUnsupportedProperty, property.Value, "header/footer presentation is adapted to the native layout")
			default:
				c.unknown(property, "header/footer property outside native profile")
				return true, true
			}
		}
	default:
		return false, false
	}
	if len(values) == 0 {
		return true, false
	}
	c.sections++
	if c.sections > maxSections {
		c.overflow = true
		return true, true
	}
	section := Section{kind: ElementGridSection, status: StatusSupported, location: locate(entry.Value)}
	for _, value := range values {
		if c.overflow {
			break
		}
		section.cards = append(section.cards, c.card(value, 0))
	}
	view.sections = append(view.sections, section)
	return true, false
}
