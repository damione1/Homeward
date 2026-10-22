package configplans

import "github.com/damione1/Homeward/compat/ha/parser"

// Entry and Files expose an admitted source to the existing compilers. They do
// not publish, authorize, or activate it. Invalid zero candidates fail closed.
func (c Candidate) Entry() string {
	if !c.Valid() {
		return ""
	}
	return c.entry
}

func (c Candidate) Files() []parser.SourceFile {
	if !c.Valid() {
		return nil
	}
	out := make([]parser.SourceFile, 0, len(c.files))
	for _, source := range c.files {
		copy, err := parser.NewSourceFile(source.Path(), source.Bytes())
		if err != nil {
			return nil
		}
		out = append(out, copy)
	}
	return out
}
