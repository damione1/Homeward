// Package dashboard imports a bounded Lovelace YAML bundle into inert native
// dashboard data. It does not render cards, evaluate visibility, fetch scripts,
// authorize actions or dispatch commands.
package dashboard

import (
	"crypto/sha256"
	"errors"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

const Profile = "homeward-dashboard-1"
const RecipesProfile = "homeward-dashboard-2"

// CompilationProfile is selected explicitly by the serving adapter. Existing
// callers retain the original profile; a new parser is never negotiated by name
// fragments or inferred from installed frontend resources.
type CompilationProfile uint8

const (
	CompilationInvalid CompilationProfile = iota
	CompilationBasic
	CompilationRecipes
)

func (p CompilationProfile) Name() string {
	switch p {
	case CompilationBasic:
		return Profile
	case CompilationRecipes:
		return RecipesProfile
	case CompilationInvalid:
		return ""
	}
	return ""
}

var ErrInvalidInput = errors.New("invalid Lovelace import input")

// Source is supplied by the caller. The HA URL, Homeward dashboard identity
// and YAML entry are different values; no title or numeric view index makes one.
type Source struct {
	id      authorization.DashboardID
	urlPath string
	entry   string
}

func NewSource(id authorization.DashboardID, urlPath, entry string) (Source, error) {
	if _, err := id.Value(); err != nil || !validSegment(urlPath, 128) || !validEntry(entry) {
		return Source{}, ErrInvalidInput
	}
	return Source{id: id, urlPath: urlPath, entry: entry}, nil
}

func (s Source) valid() bool {
	_, err := s.id.Value()
	return err == nil && validSegment(s.urlPath, 128) && validEntry(s.entry)
}

func validEntry(entry string) bool {
	_, err := parser.NewSourceFile(entry, nil)
	return err == nil
}
func (s Source) ID() authorization.DashboardID { return s.id }
func (s Source) URLPath() string               { return s.urlPath }
func (s Source) Entry() string                 { return s.entry }

type Status uint8

const (
	StatusInvalid Status = iota
	StatusSupported
	StatusUnsupported
	StatusFatal
)

func (s Status) Valid() bool { return s >= StatusSupported && s <= StatusFatal }

type Code uint8

const (
	CodeInvalid Code = iota
	CodeSyntax
	CodeInvalidValue
	CodeMissingField
	CodeUnknownField
	CodeUnsupportedVariant
	CodeUnsupportedProperty
	CodeUnboundHAUser
	CodeBlockedByAncestor
)

type Location struct {
	at         parser.Position
	path       string
	references []parser.Reference
}

func (l Location) Position() parser.Position { return l.at }
func (l Location) Path() string              { return l.path }
func (l Location) References() []parser.Reference {
	return append([]parser.Reference(nil), l.references...)
}
func (l Location) clone() Location {
	l.references = l.References()
	return l
}

type Diagnostic struct {
	code       Code
	location   Location
	syntaxCode parser.Code
	reason     string
}

func (d Diagnostic) Code() Code              { return d.code }
func (d Diagnostic) Location() Location      { return d.location.clone() }
func (d Diagnostic) SyntaxCode() parser.Code { return d.syntaxCode }
func (d Diagnostic) Reason() string          { return d.reason }
func (d Diagnostic) clone() Diagnostic {
	d.location = d.location.clone()
	return d
}

// Unsupported retains the original immutable syntax subtree. The exact source
// bytes are separately available through Result.Sources; no YAML is re-encoded.
type Unsupported struct {
	code     Code
	location Location
	node     parser.Node
}

func (u Unsupported) Code() Code         { return u.code }
func (u Unsupported) Location() Location { return u.location.clone() }
func (u Unsupported) Node() parser.Node  { return u.node }
func (u Unsupported) clone() Unsupported {
	u.location = u.location.clone()
	return u
}

// ElementKind is the sole vocabulary for renderable structural variants.
type ElementKind uint8

const (
	ElementInvalid ElementKind = iota
	ElementSectionsView
	ElementGridSection
	ElementTileCard
	ElementHeadingCard
	ElementGridCard
	ElementHorizontalStackCard
	ElementVerticalStackCard
	ElementRecipeCard
	ElementChipGroup
	ElementPopupCard
)

func validElement(k ElementKind) bool {
	return k >= ElementSectionsView && k <= ElementPopupCard
}

type RecipeKind uint8

const (
	RecipeInvalid RecipeKind = iota
	RecipeTemplate
	RecipeSelect
	RecipeAlarm
	RecipeBubbleButton
	RecipeBubblePopup
	RecipeChips
	RecipeTemplateChip
	RecipeButton
	RecipeEntity
	RecipeText
)

type FeatureKind uint8

const (
	FeatureInvalid FeatureKind = iota
	FeatureToggle
	FeatureBrightness
	FeatureColorTemperature
	FeatureTargetTemperature
	FeatureClimateModes
	FeatureFanSpeed
	FeatureNumericInput
	FeatureCoverOpenClose
	FeatureCoverPosition
	FeatureAlarmModes
)

type Feature struct {
	kind     FeatureKind
	location Location
}

func (f Feature) Kind() FeatureKind  { return f.kind }
func (f Feature) Location() Location { return f.location.clone() }

type PopupAnchor struct{ value string }

func ParsePopupAnchor(value string) (PopupAnchor, error) {
	if len(value) < 2 || value[0] != '#' || !validSegment(value[1:], 128) {
		return PopupAnchor{}, ErrInvalidInput
	}
	return PopupAnchor{value: value}, nil
}
func (a PopupAnchor) Value() (string, error) {
	if _, err := ParsePopupAnchor(a.value); err != nil {
		return "", err
	}
	return a.value, nil
}

type DimensionKind uint8

const (
	DimensionInvalid DimensionKind = iota
	DimensionCount
	DimensionFull
	DimensionAuto
)

type Dimension struct {
	kind  DimensionKind
	count uint8
}

func (d Dimension) Kind() DimensionKind { return d.kind }
func (d Dimension) Count() (uint8, bool) {
	return d.count, d.kind == DimensionCount
}

type GridOptions struct {
	columns, rows       Dimension
	hasColumns, hasRows bool
}

func (o GridOptions) Columns() (Dimension, bool) { return o.columns, o.hasColumns }
func (o GridOptions) Rows() (Dimension, bool)    { return o.rows, o.hasRows }

type HeadingStyle uint8

const (
	HeadingStyleInvalid HeadingStyle = iota
	HeadingStyleDefault
	HeadingStyleTitle
	HeadingStyleSubtitle
)

type Gesture uint8

const (
	GestureInvalid Gesture = iota
	GestureTap
	GestureHold
	GestureDoubleTap
)

type ActionKind uint8

const (
	ActionInvalid ActionKind = iota
	ActionAbsent
	ActionNone
	ActionMoreInfo
	ActionNavigate
	ActionToggle
	ActionPerform
)

func validAction(k ActionKind) bool { return k >= ActionAbsent && k <= ActionPerform }

type Action struct {
	gesture         Gesture
	kind            ActionKind
	status          Status
	location        Location
	entity          domain.HAAlias
	hasEntity       bool
	navigation      string
	service         domain.ActionID
	confirmation    string
	hasConfirmation bool
}

func (a Action) Gesture() Gesture   { return a.gesture }
func (a Action) Kind() ActionKind   { return a.kind }
func (a Action) Status() Status     { return a.status }
func (a Action) Location() Location { return a.location.clone() }
func (a Action) Entity() (domain.HAAlias, bool) {
	return a.entity, a.hasEntity && a.status == StatusSupported
}
func (a Action) Navigation() (string, bool) {
	return a.navigation, a.kind == ActionNavigate && a.status == StatusSupported
}
func (a Action) Service() (domain.ActionID, bool) {
	return a.service, a.kind == ActionPerform && a.status == StatusSupported
}
func (a Action) Confirmation() (string, bool) { return a.confirmation, a.hasConfirmation }
func (a Action) clone() Action {
	a.location = a.location.clone()
	return a
}

type VisibilityKind uint8

const (
	VisibilityInvalid VisibilityKind = iota
	VisibilityFlag
	VisibilityState
	VisibilityHAUser
	VisibilityAll
	VisibilityAny
)

func validVisibility(k VisibilityKind) bool { return k >= VisibilityFlag && k <= VisibilityAny }

type Visibility struct {
	kind     VisibilityKind
	status   Status
	location Location
	flag     bool
	entity   domain.HAAlias
	state    string
	stateNot bool
	users    []string
	children []Visibility
}

func (v Visibility) Kind() VisibilityKind { return v.kind }
func (v Visibility) Status() Status       { return v.status }
func (v Visibility) Location() Location   { return v.location.clone() }
func (v Visibility) Flag() (bool, bool)   { return v.flag, v.kind == VisibilityFlag }
func (v Visibility) Entity() (domain.HAAlias, bool) {
	return v.entity, v.kind == VisibilityState && v.status == StatusSupported
}
func (v Visibility) State() (string, bool, bool) {
	return v.state, v.stateNot, v.kind == VisibilityState && v.status == StatusSupported
}
func (v Visibility) HAUsers() []string { return append([]string(nil), v.users...) }
func (v Visibility) Children() []Visibility {
	result := make([]Visibility, len(v.children))
	for i, child := range v.children {
		result[i] = child.clone()
	}
	return result
}
func (v Visibility) clone() Visibility {
	v.location = v.location.clone()
	v.users = v.HAUsers()
	v.children = v.Children()
	return v
}

type Card struct {
	recipe       RecipeKind
	variant      string
	secondary    string
	anchor       PopupAnchor
	features     []Feature
	kind         ElementKind
	status       Status
	location     Location
	entity       domain.HAAlias
	hasEntity    bool
	name         string
	hasName      bool
	heading      string
	title        string
	hasTitle     bool
	icon         string
	vertical     bool
	hasVertical  bool
	hideState    bool
	hasHideState bool
	style        HeadingStyle
	grid         GridOptions
	columns      uint8
	square       bool
	hasSquare    bool
	children     []Card
	actions      []Action
	visibility   []Visibility
}

func (c Card) Kind() ElementKind  { return c.kind }
func (c Card) Variant() string    { return c.variant }
func (c Card) Recipe() RecipeKind { return c.recipe }
func (c Card) Secondary() string  { return c.secondary }
func (c Card) Anchor() (PopupAnchor, bool) {
	_, err := c.anchor.Value()
	return c.anchor, err == nil && c.status == StatusSupported
}
func (c Card) Features() []Feature {
	result := append([]Feature(nil), c.features...)
	for i := range result {
		result[i].location = result[i].location.clone()
	}
	return result
}
func (c Card) Status() Status     { return c.status }
func (c Card) Location() Location { return c.location.clone() }
func (c Card) Entity() (domain.HAAlias, bool) {
	return c.entity, c.hasEntity && c.status == StatusSupported
}
func (c Card) Name() (string, bool)       { return c.name, c.hasName }
func (c Card) Heading() string            { return c.heading }
func (c Card) Title() (string, bool)      { return c.title, c.hasTitle }
func (c Card) Icon() string               { return c.icon }
func (c Card) Vertical() (bool, bool)     { return c.vertical, c.hasVertical }
func (c Card) HideState() (bool, bool)    { return c.hideState, c.hasHideState }
func (c Card) HeadingStyle() HeadingStyle { return c.style }
func (c Card) GridOptions() GridOptions   { return c.grid }
func (c Card) Columns() (uint8, bool)     { return c.columns, c.columns != 0 }
func (c Card) Square() (bool, bool)       { return c.square, c.hasSquare }
func (c Card) Children() []Card {
	out := make([]Card, len(c.children))
	for i, child := range c.children {
		out[i] = child.clone()
	}
	return out
}
func (c Card) Actions() []Action {
	out := make([]Action, len(c.actions))
	for i, action := range c.actions {
		out[i] = action.clone()
	}
	return out
}
func (c Card) Visibility() []Visibility {
	out := make([]Visibility, len(c.visibility))
	for i, value := range c.visibility {
		out[i] = value.clone()
	}
	return out
}
func (c Card) clone() Card {
	c.location = c.location.clone()
	c.children = c.Children()
	c.actions = c.Actions()
	c.visibility = c.Visibility()
	c.features = c.Features()
	return c
}

type Section struct {
	kind       ElementKind
	status     Status
	location   Location
	columnSpan uint8
	rowSpan    uint8
	cards      []Card
	visibility []Visibility
}

func (s Section) Kind() ElementKind         { return s.kind }
func (s Section) Status() Status            { return s.status }
func (s Section) Location() Location        { return s.location.clone() }
func (s Section) ColumnSpan() (uint8, bool) { return s.columnSpan, s.columnSpan != 0 }
func (s Section) RowSpan() (uint8, bool)    { return s.rowSpan, s.rowSpan != 0 }
func (s Section) Cards() []Card {
	out := make([]Card, len(s.cards))
	for i, card := range s.cards {
		out[i] = card.clone()
	}
	return out
}
func (s Section) Visibility() []Visibility {
	out := make([]Visibility, len(s.visibility))
	for i, value := range s.visibility {
		out[i] = value.clone()
	}
	return out
}
func (s Section) clone() Section {
	s.location = s.location.clone()
	s.cards = s.Cards()
	s.visibility = s.Visibility()
	return s
}

type View struct {
	kind       ElementKind
	status     Status
	location   Location
	path       string
	title      string
	icon       string
	maxColumns uint8
	sections   []Section
	visibility []Visibility
}

func (v View) Kind() ElementKind  { return v.kind }
func (v View) Status() Status     { return v.status }
func (v View) Location() Location { return v.location.clone() }
func (v View) Path() string       { return v.path }

// NativeRoute identifies a view within this configuration snapshot. A pathless
// HA view uses its source node identity, without inventing a new HA URL. The
// client must retain the dashboard/configuration generation with this route.
func (v View) NativeRoute() string {
	if v.path != "" {
		return v.path
	}
	return v.location.path
}
func (v View) Title() string             { return v.title }
func (v View) Icon() string              { return v.icon }
func (v View) MaxColumns() (uint8, bool) { return v.maxColumns, v.maxColumns != 0 }
func (v View) Sections() []Section {
	out := make([]Section, len(v.sections))
	for i, section := range v.sections {
		out[i] = section.clone()
	}
	return out
}
func (v View) Visibility() []Visibility {
	out := make([]Visibility, len(v.visibility))
	for i, value := range v.visibility {
		out[i] = value.clone()
	}
	return out
}
func (v View) clone() View {
	v.location = v.location.clone()
	v.sections = v.Sections()
	v.visibility = v.Visibility()
	return v
}

// Requirements uses the same enums as the IR. No parallel string list names
// future client features. Unknown/zero values are refused by NewClientSupport.
type Requirements struct {
	version    uint8
	elements   []ElementKind
	actions    []ActionKind
	visibility []VisibilityKind
}

func (r Requirements) Version() uint8          { return r.version }
func (r Requirements) Elements() []ElementKind { return append([]ElementKind(nil), r.elements...) }
func (r Requirements) Actions() []ActionKind   { return append([]ActionKind(nil), r.actions...) }
func (r Requirements) Visibility() []VisibilityKind {
	return append([]VisibilityKind(nil), r.visibility...)
}
func (r Requirements) clone() Requirements {
	r.elements, r.actions, r.visibility = r.Elements(), r.Actions(), r.Visibility()
	return r
}

type ClientSupport struct{ requirements Requirements }

func NewClientSupport(version uint8, elements []ElementKind, actions []ActionKind, visibility []VisibilityKind) (ClientSupport, error) {
	if version != 1 && version != 2 {
		return ClientSupport{}, ErrInvalidInput
	}
	for _, kind := range elements {
		if !validElement(kind) || (version == 1 && kind > ElementHorizontalStackCard) {
			return ClientSupport{}, ErrInvalidInput
		}
	}
	for _, kind := range actions {
		if !validAction(kind) {
			return ClientSupport{}, ErrInvalidInput
		}
	}
	for _, kind := range visibility {
		if !validVisibility(kind) || (version == 1 && kind > VisibilityHAUser) {
			return ClientSupport{}, ErrInvalidInput
		}
	}
	return ClientSupport{Requirements{version: version, elements: append([]ElementKind(nil), elements...), actions: append([]ActionKind(nil), actions...), visibility: append([]VisibilityKind(nil), visibility...)}}, nil
}

func (r Requirements) SupportedBy(client ClientSupport) bool {
	if (r.version != 1 && r.version != 2) || client.requirements.version != r.version {
		return false
	}
	for _, kind := range r.elements {
		if !contains(client.requirements.elements, kind) {
			return false
		}
	}
	for _, kind := range r.actions {
		if !contains(client.requirements.actions, kind) {
			return false
		}
	}
	for _, kind := range r.visibility {
		if !contains(client.requirements.visibility, kind) {
			return false
		}
	}
	return true
}

func contains[T comparable](items []T, item T) bool {
	for _, candidate := range items {
		if candidate == item {
			return true
		}
	}
	return false
}

type Definition struct {
	source       Source
	profile      string
	views        []View
	requirements Requirements
	digest       [sha256.Size]byte
}

func (d Definition) Source() Source             { return d.source }
func (d Definition) Profile() string            { return d.profile }
func (d Definition) SHA256() [sha256.Size]byte  { return d.digest }
func (d Definition) Requirements() Requirements { return d.requirements.clone() }
func (d Definition) Views() []View {
	out := make([]View, len(d.views))
	for i, view := range d.views {
		out[i] = view.clone()
	}
	return out
}
func (d Definition) clone() Definition {
	d.views, d.requirements = d.Views(), d.requirements.clone()
	return d
}

type Result struct {
	status        Status
	source        Source
	definition    Definition
	hasDefinition bool
	sources       []parser.SourceFile
	unsupported   []Unsupported
	diagnostics   []Diagnostic
}

func (r Result) Status() Status                 { return r.status }
func (r Result) Source() Source                 { return r.source }
func (r Result) Definition() (Definition, bool) { return r.definition.clone(), r.hasDefinition }
func (r Result) Sources() []parser.SourceFile   { return append([]parser.SourceFile(nil), r.sources...) }
func (r Result) EntryBytes() ([]byte, bool) {
	for _, file := range r.sources {
		if file.Path() == r.source.entry {
			return file.Bytes(), true
		}
	}
	return nil, false
}
func (r Result) Diagnostics() []Diagnostic {
	out := make([]Diagnostic, len(r.diagnostics))
	for i, d := range r.diagnostics {
		out[i] = d.clone()
	}
	return out
}
func (r Result) Unsupported() []Unsupported {
	out := make([]Unsupported, len(r.unsupported))
	for i, item := range r.unsupported {
		out[i] = item.clone()
	}
	return out
}
