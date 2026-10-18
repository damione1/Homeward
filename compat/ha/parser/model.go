// Package parser loads the supported Home Assistant YAML syntax profile.
// Its source tree is syntax, not an executable configuration or runtime IR.
package parser

import (
	"crypto/sha256"
	"fmt"
)

const Profile = "ha-2026.10/core-1"
const DeclarativeProfile = "ha-2026.10/declarative-1"
const BlueprintInstanceProfile = "ha-2026.10/blueprint-instance-1"
const BlueprintDefinitionProfile = "ha-2026.10/blueprint-definition-1"
const LogicProfile = "ha-2026.10/logic-1"

// SyntaxProfile selects a closed grammar for the common YAML loader.
type SyntaxProfile uint8

const (
	SyntaxProfileInvalid SyntaxProfile = iota
	SyntaxProfileCore
	SyntaxProfileDeclarative
	SyntaxProfileBlueprintInstance
	SyntaxProfileBlueprintDefinition
	SyntaxProfileLogic
)

func (p SyntaxProfile) String() string {
	switch p {
	case SyntaxProfileCore:
		return Profile
	case SyntaxProfileDeclarative:
		return DeclarativeProfile
	case SyntaxProfileBlueprintInstance:
		return BlueprintInstanceProfile
	case SyntaxProfileBlueprintDefinition:
		return BlueprintDefinitionProfile
	case SyntaxProfileLogic:
		return LogicProfile
	default:
		return ""
	}
}

// Kind is the closed set of syntax values exposed by this parser.
type Kind uint8

const (
	KindInvalid Kind = iota
	KindMapping
	KindSequence
	KindScalar
	KindSecretRef
	KindInputRef
)

type ScalarKind uint8

const (
	ScalarInvalid ScalarKind = iota
	ScalarString
	ScalarBool
	ScalarInteger
	ScalarFloat
	ScalarNull
)

type Scalar struct {
	kind ScalarKind
	text string
}

func (s Scalar) Kind() ScalarKind { return s.kind }
func (s Scalar) Text() string     { return s.text }

// Position names a bundle-relative file. Line and Column are one-based when
// known, or zero when the YAML library supplies no coordinate.
type Position struct {
	File   string
	Line   int
	Column int
}

type ReferenceKind uint8

const (
	ReferenceInvalid ReferenceKind = iota
	ReferenceInclude
	ReferenceAlias
)

type Reference struct {
	Kind ReferenceKind
	At   Position
}

// Entry retains source order and the source position of its key.
type Entry struct {
	Key       string
	KeySource Position
	Value     Node
}

// Node has no exported mutable fields. Included and aliased values retain
// their definition position and a chain of reference sites.
type Node struct {
	kind          Kind
	scalar        Scalar
	secretKey     string
	entries       []Entry
	items         []Node
	source        Position
	path          string
	references    []Reference
	substitutions []Substitution
}

func (n Node) Kind() Kind {
	return n.kind
}
func (n Node) Source() Position {
	return n.source
}
func (n Node) Path() string {
	return n.path
}
func (n Node) References() []Reference {
	return append([]Reference(nil), n.references...)
}

// InputName identifies a structural placeholder, never template text.
func (n Node) InputName() (InputName, bool) {
	if n.kind != KindInputRef {
		return InputName{}, false
	}
	return InputName{value: n.scalar.text}, true
}

// Substitutions links every injected descendant to its value and definition.
func (n Node) Substitutions() []Substitution {
	return append([]Substitution(nil), n.substitutions...)
}
func (n Node) Entries() []Entry {
	if n.kind != KindMapping {
		return nil
	}
	return append([]Entry(nil), n.entries...)
}
func (n Node) Items() []Node {
	if n.kind != KindSequence {
		return nil
	}
	return append([]Node(nil), n.items...)
}
func (n Node) Lookup(key string) (Node, bool) {
	if n.kind != KindMapping {
		return Node{}, false
	}
	for _, entry := range n.entries {
		if entry.Key == key {
			return entry.Value, true
		}
	}
	return Node{}, false
}
func (n Node) Scalar() (Scalar, bool) {
	if n.kind != KindScalar {
		return Scalar{}, false
	}
	return n.scalar, true
}

// SecretKey returns an opaque reference name. The loader never reads a
// secrets file, resolves environment variables, or embeds a secret value.
func (n Node) SecretKey() (string, bool) {
	if n.kind != KindSecretRef {
		return "", false
	}
	return n.secretKey, true
}

// SourceFile contains the exact bytes used by the parser. Bytes always returns
// a copy; a snapshot can be persisted and later passed to LoadSources.
type SourceFile struct {
	path string
	data []byte
	hash [sha256.Size]byte
}

func (s SourceFile) Path() string              { return s.path }
func (s SourceFile) Bytes() []byte             { return append([]byte(nil), s.data...) }
func (s SourceFile) SHA256() [sha256.Size]byte { return s.hash }

type Document struct {
	root    Node
	profile SyntaxProfile
	sources []SourceFile
}

func (d *Document) Root() Node {
	if d == nil {
		return Node{}
	}
	return d.root
}
func (d *Document) Profile() string {
	if d == nil {
		return ""
	}
	return d.profile.String()
}

func (d *Document) Sources() []SourceFile {
	if d == nil {
		return nil
	}
	return append([]SourceFile(nil), d.sources...)
}

// Code is the closed diagnostic vocabulary. A caller must branch on Code,
// never parse an error message. String is its stable external spelling.
type Code uint8

const (
	CodeInvalid Code = iota
	CodeInvalidEntry
	CodeIncludeOutside
	CodeIncludeCycle
	CodeIncludeDepth
	CodeSizeLimit
	CodeReadFile
	CodeYAML
	CodeMultipleDocs
	CodeUnsupportedTag
	CodeAliasCycle
	CodeAliasLimit
	CodeDuplicateKey
	CodeUnknownField
	CodeUnsupportedForm
	CodeSyntaxLimit
	CodeBlueprintDefinition
	CodeBlueprintInputMissing
	CodeBlueprintInputUnknown
	CodeBlueprintInputType
	CodeBlueprintInputReference
	CodeBlueprintInputCollision
)

func (c Code) String() string {
	switch c {
	case CodeInvalidEntry:
		return "HA_INVALID_ENTRY"
	case CodeIncludeOutside:
		return "HA_INCLUDE_OUTSIDE"
	case CodeIncludeCycle:
		return "HA_INCLUDE_CYCLE"
	case CodeIncludeDepth:
		return "HA_INCLUDE_DEPTH"
	case CodeSizeLimit:
		return "HA_SIZE_LIMIT"
	case CodeReadFile:
		return "HA_READ_FILE"
	case CodeYAML:
		return "HA_YAML"
	case CodeMultipleDocs:
		return "HA_MULTIPLE_DOCS"
	case CodeUnsupportedTag:
		return "HA_UNSUPPORTED_TAG"
	case CodeAliasCycle:
		return "HA_ALIAS_CYCLE"
	case CodeAliasLimit:
		return "HA_ALIAS_LIMIT"
	case CodeDuplicateKey:
		return "HA_DUPLICATE_KEY"
	case CodeUnknownField:
		return "HA_UNKNOWN_FIELD"
	case CodeUnsupportedForm:
		return "HA_UNSUPPORTED_FORM"
	case CodeSyntaxLimit:
		return "HA_SYNTAX_LIMIT"
	case CodeBlueprintDefinition:
		return "HA_BLUEPRINT_DEFINITION"
	case CodeBlueprintInputMissing:
		return "HA_BLUEPRINT_INPUT_MISSING"
	case CodeBlueprintInputUnknown:
		return "HA_BLUEPRINT_INPUT_UNKNOWN"
	case CodeBlueprintInputType:
		return "HA_BLUEPRINT_INPUT_TYPE"
	case CodeBlueprintInputReference:
		return "HA_BLUEPRINT_INPUT_REFERENCE"
	case CodeBlueprintInputCollision:
		return "HA_BLUEPRINT_INPUT_COLLISION"
	case CodeInvalid:
		return "HA_INVALID"
	default:
		return "HA_INVALID"
	}
}

type Diagnostic struct {
	Code    Code
	At      Position
	Path    string
	Message string
}

func (d *Diagnostic) Error() string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d %s at %s: %s", d.At.File, d.At.Line, d.At.Column, d.Code.String(), d.Path, d.Message)
}

func diagnostic(code Code, at Position, path, message string) *Diagnostic {
	return &Diagnostic{Code: code, At: at, Path: path, Message: message}
}
