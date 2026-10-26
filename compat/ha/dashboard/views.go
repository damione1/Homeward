package dashboard

import (
	"strconv"

	"github.com/damione1/Homeward/compat/ha/parser"
)

func scalarText(node parser.Node) (string, bool) {
	value, ok := node.Scalar()
	return value.Text(), ok && value.Kind() == parser.ScalarString
}

func scalarBool(node parser.Node) (bool, bool) {
	value, ok := node.Scalar()
	if !ok || value.Kind() != parser.ScalarBool {
		return false, false
	}
	return value.Text() == "true", true
}

func positiveCount(node parser.Node) (uint8, bool) {
	value, ok := node.Scalar()
	if !ok || value.Kind() != parser.ScalarInteger {
		return 0, false
	}
	n, err := strconv.Atoi(value.Text())
	if err != nil || n < 1 || n > 12 {
		return 0, false
	}
	return uint8(n), true
}

func (c *compiler) view(node parser.Node) View {
	v := View{status: StatusSupported, location: locate(node)}
	if node.Kind() != parser.KindMapping {
		c.unsupported(CodeInvalidValue, node, "view must be a mapping")
		v.status = StatusUnsupported
		return v
	}
	blocked := false
	hasType, hasPath, hasTitle, hasSections := false, false, false, false
	for _, entry := range node.Entries() {
		switch entry.Key {
		case "type":
			hasType = true
			if text, ok := scalarText(entry.Value); !ok || text != "sections" {
				c.unsupported(CodeUnsupportedVariant, entry.Value, "only sections views are admitted")
				blocked = true
			} else {
				v.kind = ElementSectionsView
			}
		case "path":
			hasPath = true
			text, ok := scalarText(entry.Value)
			if !ok || !validSegment(text, 128) {
				c.unsupported(CodeInvalidValue, entry.Value, "view path must be one stable local segment")
				blocked = true
			} else {
				v.path = text
			}
		case "title":
			hasTitle = true
			text, ok := scalarText(entry.Value)
			if !ok || text == "" || len(text) > 256 {
				c.unsupported(CodeInvalidValue, entry.Value, "view title must be a bounded nonempty string")
				blocked = true
			} else {
				v.title = text
			}
		case "icon":
			text, ok := scalarText(entry.Value)
			if !ok || text == "" || len(text) > 256 {
				c.unsupported(CodeInvalidValue, entry.Value, "icon must be a bounded string")
				blocked = true
			} else {
				v.icon = text
			}
		case "max_columns":
			count, ok := positiveCount(entry.Value)
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "max_columns must be 1..12")
				blocked = true
			} else {
				v.maxColumns = count
			}
		case "sections":
			hasSections = true
			if entry.Value.Kind() != parser.KindSequence {
				c.unsupported(CodeInvalidValue, entry.Value, "sections must be a sequence")
				blocked = true
				continue
			}
			for _, child := range entry.Value.Items() {
				if c.overflow {
					break
				}
				v.sections = append(v.sections, c.section(child))
			}
		case "visible":
			flag, ok := scalarBool(entry.Value)
			if !ok {
				c.unsupported(CodeUnsupportedVariant, entry.Value, "only boolean view visibility is admitted")
				blocked = true
			} else {
				v.visibility = append(v.visibility, Visibility{kind: VisibilityFlag, status: StatusSupported, location: locate(entry.Value), flag: flag})
			}
		default:
			if c.profile == CompilationRecipes {
				if handled, invalid := c.viewExtension(&v, entry); handled {
					blocked = blocked || invalid
					continue
				}
			}
			c.unknown(entry, "view property outside profile")
			blocked = true
		}
	}
	if !hasType && c.profile == CompilationRecipes && hasSections {
		v.kind = ElementSectionsView
		hasType = true
	}
	if !hasType {
		c.unsupported(CodeMissingField, node, "explicit sections view type is required")
		blocked = true
	}
	if !hasPath && c.profile != CompilationRecipes {
		c.unsupported(CodeMissingField, node, "stable view path is required")
		blocked = true
	}
	if !hasTitle {
		c.unsupported(CodeMissingField, node, "view title is required")
		blocked = true
	}
	if !hasSections {
		c.unsupported(CodeMissingField, node, "sections are required")
		blocked = true
	}
	if blocked {
		disableView(&v)
	}
	return v
}

func (c *compiler) section(node parser.Node) Section {
	c.sections++
	s := Section{status: StatusSupported, location: locate(node)}
	if c.sections > maxSections {
		c.overflow = true
		s.status = StatusUnsupported
		return s
	}
	if node.Kind() != parser.KindMapping {
		c.unsupported(CodeInvalidValue, node, "section must be a mapping")
		s.status = StatusUnsupported
		return s
	}
	blocked, hasType, hasCards := false, false, false
	for _, entry := range node.Entries() {
		switch entry.Key {
		case "type":
			hasType = true
			if text, ok := scalarText(entry.Value); !ok || text != "grid" {
				c.unsupported(CodeUnsupportedVariant, entry.Value, "only grid sections are admitted")
				blocked = true
			} else {
				s.kind = ElementGridSection
			}
		case "cards":
			hasCards = true
			if entry.Value.Kind() != parser.KindSequence {
				c.unsupported(CodeInvalidValue, entry.Value, "section cards must be a sequence")
				blocked = true
				continue
			}
			for _, child := range entry.Value.Items() {
				if c.overflow {
					break
				}
				s.cards = append(s.cards, c.card(child, 0))
			}
		case "column_span":
			count, ok := positiveCount(entry.Value)
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "column_span must be 1..12")
				blocked = true
			} else {
				s.columnSpan = count
			}
		case "row_span":
			count, ok := positiveCount(entry.Value)
			if !ok {
				c.unsupported(CodeInvalidValue, entry.Value, "row_span must be 1..12")
				blocked = true
			} else {
				s.rowSpan = count
			}
		case "visibility":
			values, invalid := c.visibility(entry.Value)
			s.visibility = append(s.visibility, values...)
			blocked = blocked || invalid
		default:
			c.unknown(entry, "section property outside profile")
			blocked = true
		}
	}
	if !hasType {
		c.unsupported(CodeMissingField, node, "explicit grid section type is required")
		blocked = true
	}
	if !hasCards {
		c.unsupported(CodeMissingField, node, "grid section cards are required")
		blocked = true
	}
	if blocked {
		disableSection(&s)
	}
	return s
}
