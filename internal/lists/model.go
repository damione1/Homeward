// Package lists owns private, durable local task and shopping lists. It has no
// Home Assistant client or public RPC surface.
package lists

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

const MaxPage = 256
const MaxUIDBytes = 255
const MaxNameBytes = 512
const MaxDescriptionBytes = 4096
const MaxZoneBytes = 128

type Code uint8

const (
	Invalid Code = iota + 1
	Denied
	NotFound
	Conflict
	ImportConflict
	StaleCursor
	Capacity
	Unavailable
	CommitUnknown
	CommittedResponseWithheld
	Closed
	ResyncRequired
)

type Error struct{ code Code }

func (e *Error) Error() string { return "lists: operation failed" }
func (e *Error) Code() Code {
	if e == nil {
		return Invalid
	}
	return e.code
}
func failure(code Code) error { return &Error{code: code} }
func IsCode(err error, code Code) bool {
	var target *Error
	return errors.As(err, &target) && target.code == code
}

type Origin uint8

const (
	OriginInvalid Origin = iota
	LocalTodo
	ShoppingList
	Native
)

func (o Origin) Valid() bool { return o == LocalTodo || o == ShoppingList || o == Native }
func (o Origin) sql() (string, error) {
	switch o {
	case LocalTodo:
		return "legacy_local_todo", nil
	case ShoppingList:
		return "legacy_shopping_list", nil
	case Native:
		return "native", nil
	default:
		return "", failure(Invalid)
	}
}

type lifecycle uint8

const (
	lifecycleInvalid lifecycle = iota
	lifecycleActive
	lifecycleDeleted
)

func lifecycleFromSQL(value int16) (lifecycle, error) {
	switch value {
	case int16(lifecycleActive):
		return lifecycleActive, nil
	case int16(lifecycleDeleted):
		return lifecycleDeleted, nil
	default:
		return lifecycleInvalid, failure(Unavailable)
	}
}
func originFromSQL(text string) (Origin, error) {
	switch text {
	case "legacy_local_todo":
		return LocalTodo, nil
	case "legacy_shopping_list":
		return ShoppingList, nil
	case "native":
		return Native, nil
	default:
		return OriginInvalid, failure(Unavailable)
	}
}

type ItemUID struct{ value string }

func ParseItemUID(value string) (ItemUID, error) {
	if !validText(value, MaxUIDBytes, true) {
		return ItemUID{}, failure(Invalid)
	}
	return ItemUID{value: value}, nil
}
func (id ItemUID) Value() (string, error) {
	parsed, err := ParseItemUID(id.value)
	return parsed.value, err
}

type ListRevision struct {
	id     domain.ListID
	number int64
}

func NewListRevision(id domain.ListID, number int64) (ListRevision, error) {
	if _, err := id.Value(); err != nil || number <= 0 {
		return ListRevision{}, failure(Invalid)
	}
	return ListRevision{id, number}, nil
}
func (r ListRevision) Values() (domain.ListID, int64, error) {
	admitted, err := NewListRevision(r.id, r.number)
	return admitted.id, admitted.number, err
}

type ItemRevision struct {
	list   domain.ListID
	uid    ItemUID
	number int64
}

func NewItemRevision(list domain.ListID, uid ItemUID, number int64) (ItemRevision, error) {
	if _, err := list.Value(); err != nil || number <= 0 {
		return ItemRevision{}, failure(Invalid)
	}
	if _, err := uid.Value(); err != nil {
		return ItemRevision{}, failure(Invalid)
	}
	return ItemRevision{list, uid, number}, nil
}
func (r ItemRevision) Values() (domain.ListID, ItemUID, int64, error) {
	admitted, err := NewItemRevision(r.list, r.uid, r.number)
	return admitted.list, admitted.uid, admitted.number, err
}

type dueKind uint8

const (
	dueInvalid dueKind = iota
	dueNone
	dueDate
	dueInstant
)

type Due struct {
	kind    dueKind
	date    string
	instant time.Time
	zone    string
}

func NoDue() Due { return Due{kind: dueNone} }
func NewDueDate(text string) (Due, error) {
	parsed, err := time.Parse("2006-01-02", text)
	if err != nil || parsed.Year() < 1 || parsed.Year() > 9999 || parsed.Format("2006-01-02") != text {
		return Due{}, failure(Invalid)
	}
	return Due{kind: dueDate, date: text}, nil
}
func NewDueInstant(at time.Time, zone string) (Due, error) {
	if !validPGInstant(at) || !validText(zone, MaxZoneBytes, true) || zone == "Local" {
		return Due{}, failure(Invalid)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return Due{}, failure(Invalid)
	}
	return Due{kind: dueInstant, instant: at.UTC(), zone: zone}, nil
}
func (d Due) Values() (string, time.Time, string, error) {
	switch d.kind {
	case dueNone:
		if d.date == "" && d.instant.IsZero() && d.zone == "" {
			return "", time.Time{}, "", nil
		}
	case dueDate:
		if _, err := NewDueDate(d.date); err == nil && d.instant.IsZero() && d.zone == "" {
			return d.date, time.Time{}, "", nil
		}
	case dueInstant:
		if _, err := NewDueInstant(d.instant, d.zone); err == nil && d.date == "" {
			return "", d.instant, d.zone, nil
		}
	}
	return "", time.Time{}, "", failure(Invalid)
}
func (d Due) Present() bool { _, _, _, err := d.Values(); return err == nil && d.kind != dueNone }

type ItemInput struct {
	Summary     string
	Completed   bool
	Description *string
	Due         Due
	CompletedAt *time.Time
}
type ItemPayload struct {
	summary     string
	completed   bool
	description *string
	due         Due
	completedAt *time.Time
	valid       bool
}

func NewItemPayload(input ItemInput) (ItemPayload, error) {
	if !validText(input.Summary, MaxNameBytes, true) {
		return ItemPayload{}, failure(Invalid)
	}
	if input.Description != nil && !validDescription(*input.Description) {
		return ItemPayload{}, failure(Invalid)
	}
	if _, _, _, err := input.Due.Values(); err != nil {
		return ItemPayload{}, err
	}
	if input.CompletedAt != nil && (!validPGInstant(*input.CompletedAt) || !input.Completed) {
		return ItemPayload{}, failure(Invalid)
	}
	p := ItemPayload{summary: strings.Clone(input.Summary), completed: input.Completed, due: input.Due, valid: true}
	if input.Description != nil {
		copied := strings.Clone(*input.Description)
		p.description = &copied
	}
	if input.CompletedAt != nil {
		copied := input.CompletedAt.UTC()
		p.completedAt = &copied
	}
	return p, nil
}
func (p ItemPayload) validFor(origin Origin) bool {
	if !p.valid || !origin.Valid() || !validText(p.summary, MaxNameBytes, true) {
		return false
	}
	if p.description != nil && !validDescription(*p.description) {
		return false
	}
	if p.completedAt != nil && (!p.completed || !validPGInstant(*p.completedAt)) {
		return false
	}
	if _, _, _, err := p.due.Values(); err != nil {
		return false
	}
	if origin == ShoppingList && (p.description != nil || p.due.kind != dueNone || p.completedAt != nil) {
		return false
	}
	return true
}

type ListPayload struct {
	name  string
	valid bool
}

func NewListPayload(name string) (ListPayload, error) {
	if !validText(name, MaxNameBytes, true) {
		return ListPayload{}, failure(Invalid)
	}
	return ListPayload{strings.Clone(name), true}, nil
}
func (p ListPayload) validPayload() bool { return p.valid && validText(p.name, MaxNameBytes, true) }

type LegacyListPayload struct {
	name  string
	valid bool
}

func NewLegacyListPayload(name string) (LegacyListPayload, error) {
	p, err := NewListPayload(name)
	return LegacyListPayload{p.name, p.valid}, err
}
func (p LegacyListPayload) validPayload() bool {
	return p.valid && validText(p.name, MaxNameBytes, true)
}

type LegacyItemPayload struct {
	uid      ItemUID
	position int64
	item     ItemPayload
	valid    bool
}

func NewLegacyItemPayload(uid ItemUID, position int64, item ItemPayload) (LegacyItemPayload, error) {
	if _, err := uid.Value(); err != nil || position < 0 || !item.valid {
		return LegacyItemPayload{}, failure(Invalid)
	}
	return LegacyItemPayload{uid, position, item, true}, nil
}
func (p LegacyItemPayload) validFor(origin Origin) bool {
	_, err := p.uid.Value()
	return p.valid && err == nil && p.position >= 0 && p.item.validFor(origin)
}

type ReceiptOutcome uint8

const (
	ReceiptInvalid ReceiptOutcome = iota
	Committed
	AlreadyImported
)

type MutationReceipt struct {
	list         domain.ListID
	item         ItemUID
	listRevision ListRevision
	itemRevision ItemRevision
	outcome      ReceiptOutcome
}

func (r MutationReceipt) ListID() domain.ListID { return r.list }
func (r MutationReceipt) ItemUID() (ItemUID, bool) {
	_, err := r.item.Value()
	return r.item, err == nil
}
func (r MutationReceipt) ListRevision() (ListRevision, bool) {
	_, _, err := r.listRevision.Values()
	return r.listRevision, err == nil
}
func (r MutationReceipt) ItemRevision() (ItemRevision, bool) {
	_, _, _, err := r.itemRevision.Values()
	return r.itemRevision, err == nil
}
func (r MutationReceipt) Outcome() ReceiptOutcome { return r.outcome }

type ListSnapshot struct {
	id       domain.ListID
	origin   Origin
	alias    domain.HAAlias
	name     string
	revision ListRevision
	updated  time.Time
}

func (s ListSnapshot) ID() domain.ListID { return s.id }
func (s ListSnapshot) Origin() Origin    { return s.origin }
func (s ListSnapshot) Alias() (domain.HAAlias, bool) {
	_, err := s.alias.Value()
	return s.alias, err == nil
}
func (s ListSnapshot) Name() string           { return s.name }
func (s ListSnapshot) Revision() ListRevision { return s.revision }
func (s ListSnapshot) UpdatedAt() time.Time   { return s.updated }

type ItemSnapshot struct {
	list     domain.ListID
	uid      ItemUID
	origin   Origin
	payload  ItemPayload
	position int64
	revision ItemRevision
	updated  time.Time
}

func (s ItemSnapshot) UID() ItemUID          { return s.uid }
func (s ItemSnapshot) ListID() domain.ListID { return s.list }
func (s ItemSnapshot) Summary() string       { return s.payload.summary }
func (s ItemSnapshot) Completed() bool       { return s.payload.completed }
func (s ItemSnapshot) Description() (string, bool) {
	if s.payload.description == nil {
		return "", false
	}
	return *s.payload.description, true
}
func (s ItemSnapshot) Due() Due { return s.payload.due }
func (s ItemSnapshot) CompletedAt() (time.Time, bool) {
	if s.payload.completedAt == nil {
		return time.Time{}, false
	}
	return *s.payload.completedAt, true
}
func (s ItemSnapshot) Position() int64        { return s.position }
func (s ItemSnapshot) Revision() ItemRevision { return s.revision }

type ListCursor struct {
	valid    bool
	kind     cursorViewKind
	revision int64
	view     [sha256.Size]byte
	last     domain.ListID
	rights   uint64
	semantic [sha256.Size]byte
}

type cursorViewKind uint8

const (
	cursorViewInvalid cursorViewKind = iota
	cursorViewAll
	cursorViewExplicit
)

type ItemCursor struct {
	valid        bool
	list         domain.ListID
	revision     int64
	lastPosition int64
	last         ItemUID
}
type ListPage struct {
	entries []ListSnapshot
	next    ListCursor
}

func (p ListPage) Entries() []ListSnapshot  { return append([]ListSnapshot(nil), p.entries...) }
func (p ListPage) Next() (ListCursor, bool) { return p.next, p.next.valid }

type ItemPage struct {
	entries []ItemSnapshot
	next    ItemCursor
}

func (p ItemPage) Entries() []ItemSnapshot  { return append([]ItemSnapshot(nil), p.entries...) }
func (p ItemPage) Next() (ItemCursor, bool) { return p.next, p.next.valid }

func validText(text string, max int, nonempty bool) bool {
	return (!nonempty || text != "") && len(text) <= max && utf8.ValidString(text) && !strings.ContainsAny(text, "\x00\r\n")
}
func validDescription(text string) bool {
	return len(text) <= MaxDescriptionBytes && utf8.ValidString(text) && !strings.ContainsRune(text, '\x00')
}
func validPGInstant(at time.Time) bool {
	utc := at.UTC()
	return !at.IsZero() && utc.Year() >= 1 && utc.Year() <= 9999 && utc.Nanosecond()%1000 == 0
}

// Import hashes are over a closed, ordered representation, not Go map order or
// the current mutable SQL row. They are never included in a mutation receipt.
func listSourceHash(alias domain.HAAlias, p LegacyListPayload) ([sha256.Size]byte, error) {
	text, err := alias.Value()
	if err != nil || !p.validPayload() {
		return [sha256.Size]byte{}, failure(Invalid)
	}
	data, err := json.Marshal(struct{ Alias, Name string }{text, p.name})
	if err != nil {
		return [sha256.Size]byte{}, failure(Invalid)
	}
	return sha256.Sum256(data), nil
}
func itemSourceHash(alias domain.HAAlias, p LegacyItemPayload) ([sha256.Size]byte, error) {
	text, err := alias.Value()
	if err != nil {
		return [sha256.Size]byte{}, failure(Invalid)
	}
	uid, err := p.uid.Value()
	if err != nil || p.position < 0 || !p.item.valid {
		return [sha256.Size]byte{}, failure(Invalid)
	}
	date, at, zone, err := p.item.due.Values()
	if err != nil {
		return [sha256.Size]byte{}, failure(Invalid)
	}
	var desc string
	var descPresent bool
	if p.item.description != nil {
		desc = *p.item.description
		descPresent = true
	}
	var done time.Time
	var donePresent bool
	if p.item.completedAt != nil {
		done = *p.item.completedAt
		donePresent = true
	}
	data, err := json.Marshal(struct {
		Alias, UID, Summary, Description, DueDate, DueZone string
		Position                                           int64
		Completed, DescriptionPresent, CompletedAtPresent  bool
		DueAt, CompletedAt                                 time.Time
	}{text, uid, p.item.summary, desc, date, zone, p.position, p.item.completed, descPresent, donePresent, at, done})
	if err != nil {
		return [sha256.Size]byte{}, failure(Invalid)
	}
	return sha256.Sum256(data), nil
}
