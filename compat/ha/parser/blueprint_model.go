package parser

import (
	"crypto/sha256"
	"strings"
	"unicode/utf8"
)

const BlueprintExpandedProfile = "ha-2026.10/blueprint-expanded-1"

// InputName is admitted only by blueprint parsing. Its Go zero is invalid.
type InputName struct{ value string }

func (n InputName) String() string { return n.value }
func (n InputName) Valid() bool    { return validInputName(n.value) }
func validInputName(s string) bool {
	return s != "" && len(s) <= maxKeyBytes && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

type SelectorKind uint8

const (
	SelectorInvalid SelectorKind = iota
	SelectorDevice
	SelectorAction
	SelectorNumber
	SelectorBoolean
)

func (s SelectorKind) String() string {
	switch s {
	case SelectorDevice:
		return "device"
	case SelectorAction:
		return "action"
	case SelectorNumber:
		return "number"
	case SelectorBoolean:
		return "boolean"
	case SelectorInvalid:
		return ""
	default:
		return ""
	}
}
func selectorFromKey(key string) SelectorKind {
	switch key {
	case SelectorDevice.String():
		return SelectorDevice
	case SelectorAction.String():
		return SelectorAction
	case SelectorNumber.String():
		return SelectorNumber
	case SelectorBoolean.String():
		return SelectorBoolean
	default:
		return SelectorInvalid
	}
}

type InputOrigin uint8

const (
	InputOriginInvalid InputOrigin = iota
	InputOriginInvocation
	InputOriginDefault
)

func (o InputOrigin) String() string {
	switch o {
	case InputOriginInvocation:
		return "invocation"
	case InputOriginDefault:
		return "default"
	case InputOriginInvalid:
		return ""
	default:
		return ""
	}
}

type EffectiveInput struct {
	name        InputName
	selector    SelectorKind
	origin      InputOrigin
	value       Node
	declaration Position
}

func (i EffectiveInput) Name() InputName        { return i.name }
func (i EffectiveInput) Selector() SelectorKind { return i.selector }
func (i EffectiveInput) Origin() InputOrigin    { return i.origin }
func (i EffectiveInput) Value() Node            { return i.value }
func (i EffectiveInput) Declaration() Position  { return i.declaration }

// Substitution preserves both sides without replacing a value's real position.
// ReferenceChain is the include/alias chain of the !input use site; the value's
// own chain remains on Node.References.
type Substitution struct {
	name        InputName
	at          Position
	declaration Position
	origin      InputOrigin
	valueSource Position
	references  []Reference
}

func (s Substitution) Input() InputName            { return s.name }
func (s Substitution) At() Position                { return s.at }
func (s Substitution) Declaration() Position       { return s.declaration }
func (s Substitution) Origin() InputOrigin         { return s.origin }
func (s Substitution) ValueSource() Position       { return s.valueSource }
func (s Substitution) ReferenceChain() []Reference { return append([]Reference(nil), s.references...) }

// ExpandedBlueprint is syntax awaiting NHA-039 semantic admission. It cannot
// execute, subscribe, resolve entities, or send a command.
type ExpandedBlueprint struct {
	root                  Node
	sources               []SourceFile
	blueprintPath         string
	blueprintHash         [sha256.Size]byte
	definitionSourcesHash [sha256.Size]byte
	inputsHash            [sha256.Size]byte
	inputs                []EffectiveInput
}

func (b *ExpandedBlueprint) Valid() bool {
	return b != nil && b.root.kind == KindMapping && b.blueprintPath != ""
}
func (b *ExpandedBlueprint) Profile() string {
	if !b.Valid() {
		return ""
	}
	return BlueprintExpandedProfile
}
func (b *ExpandedBlueprint) Root() Node {
	if b == nil {
		return Node{}
	}
	return b.root
}
func (b *ExpandedBlueprint) Sources() []SourceFile {
	if b == nil {
		return nil
	}
	return append([]SourceFile(nil), b.sources...)
}
func (b *ExpandedBlueprint) BlueprintPath() string {
	if b == nil {
		return ""
	}
	return b.blueprintPath
}
func (b *ExpandedBlueprint) BlueprintSHA256() [sha256.Size]byte {
	if b == nil {
		return [sha256.Size]byte{}
	}
	return b.blueprintHash
}
func (b *ExpandedBlueprint) DefinitionSourcesSHA256() [sha256.Size]byte {
	if b == nil {
		return [sha256.Size]byte{}
	}
	return b.definitionSourcesHash
}
func (b *ExpandedBlueprint) InputsSHA256() [sha256.Size]byte {
	if b == nil {
		return [sha256.Size]byte{}
	}
	return b.inputsHash
}
func (b *ExpandedBlueprint) Inputs() []EffectiveInput {
	if b == nil {
		return nil
	}
	return append([]EffectiveInput(nil), b.inputs...)
}
