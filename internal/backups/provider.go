package backups

import (
	"context"
	"io"
)

type Capabilities struct{ limits Limits }

// Every provider admitted in this version implements the complete port,
// including atomic publication. Unsupported backends are rejected at startup.
func NewCapabilities(limits Limits) (Capabilities, error) {
	if !limits.Valid() {
		return Capabilities{}, NewError(InvalidInput)
	}
	return Capabilities{limits: limits}, nil
}
func (c Capabilities) Valid() bool             { return c.limits.Valid() }
func (c Capabilities) Limits() Limits          { return c.limits }
func (c Capabilities) Write() bool             { return c.Valid() }
func (c Capabilities) List() bool              { return c.Valid() }
func (c Capabilities) Open() bool              { return c.Valid() }
func (c Capabilities) Delete() bool            { return c.Valid() }
func (c Capabilities) Verify() bool            { return c.Valid() }
func (c Capabilities) AtomicPublication() bool { return c.Valid() }

type WriteIntent struct {
	id       ArtifactID
	maxBytes int64
	valid    bool
}

func NewWriteIntent(id ArtifactID, maxBytes int64) (WriteIntent, error) {
	if !id.Valid() || maxBytes < 0 || maxBytes > MaxArtifactBytes {
		return WriteIntent{}, NewError(InvalidInput)
	}
	return WriteIntent{id: id, maxBytes: maxBytes, valid: true}, nil
}
func (i WriteIntent) Valid() bool     { return i.valid }
func (i WriteIntent) ID() ArtifactID  { return i.id }
func (i WriteIntent) MaxBytes() int64 { return i.maxBytes }

type PublicationState uint8

const (
	NotPublished PublicationState = iota + 1
	Published
	AlreadyPresent
	Indeterminate
)

type WriteResult struct {
	state    PublicationState
	id       ArtifactID
	metadata Metadata
}

func NewWriteResult(state PublicationState, id ArtifactID, metadata Metadata) (WriteResult, error) {
	if !id.Valid() {
		return WriteResult{}, NewError(InvalidInput)
	}
	switch state {
	case NotPublished, Indeterminate:
		if metadata.Valid() {
			return WriteResult{}, NewError(InvalidInput)
		}
	case Published, AlreadyPresent:
		if !metadata.Valid() || metadata.ID() != id {
			return WriteResult{}, NewError(InvalidInput)
		}
	default:
		return WriteResult{}, NewError(InvalidInput)
	}
	return WriteResult{state, id, metadata}, nil
}
func (r WriteResult) State() PublicationState    { return r.state }
func (r WriteResult) ID() ArtifactID             { return r.id }
func (r WriteResult) Metadata() (Metadata, bool) { return r.metadata, r.metadata.Valid() }

type DeleteState uint8

const (
	DeleteNotApplied DeleteState = iota + 1
	Deleted
	AlreadyAbsent
	DeleteIndeterminate
)

type DeleteResult struct {
	state DeleteState
	id    ArtifactID
}

func NewDeleteResult(state DeleteState, id ArtifactID) (DeleteResult, error) {
	if !id.Valid() || state < DeleteNotApplied || state > DeleteIndeterminate {
		return DeleteResult{}, NewError(InvalidInput)
	}
	return DeleteResult{state, id}, nil
}
func (r DeleteResult) State() DeleteState { return r.state }
func (r DeleteResult) ID() ArtifactID     { return r.id }

// Cursor is an exclusive ArtifactID in ascending order, bound to its instance.
// Pages are not a snapshot across mutations; a new scan occurs on every call.
type Cursor struct {
	instance InstanceID
	after    ArtifactID
}

func NewCursor(instance InstanceID, after ArtifactID) (Cursor, error) {
	if !instance.Valid() || !after.Valid() {
		return Cursor{}, NewError(InvalidInput)
	}
	return Cursor{instance, after}, nil
}
func (c Cursor) Present() bool        { return c.instance.Valid() && c.after.Valid() }
func (c Cursor) Instance() InstanceID { return c.instance }
func (c Cursor) After() ArtifactID    { return c.after }

type ListRequest struct {
	limit  int
	cursor Cursor
}

func NewListRequest(limit int, cursor Cursor) (ListRequest, error) {
	if limit < 1 || limit > MaxPageSize || (!cursor.Present() && cursor != (Cursor{})) {
		return ListRequest{}, NewError(InvalidInput)
	}
	return ListRequest{limit, cursor}, nil
}
func (r ListRequest) Valid() bool    { return r.limit > 0 }
func (r ListRequest) Limit() int     { return r.limit }
func (r ListRequest) Cursor() Cursor { return r.cursor }

type Page struct {
	items []Metadata
	next  Cursor
	valid bool
}

func NewPage(items []Metadata, next Cursor) (Page, error) {
	if len(items) > MaxPageSize || (!next.Present() && next != (Cursor{})) {
		return Page{}, NewError(InvalidInput)
	}
	for i, m := range items {
		if !m.Valid() || i > 0 && items[i-1].ID().String() >= m.ID().String() {
			return Page{}, NewError(InvalidInput)
		}
	}
	if next.Present() && (len(items) == 0 || next.After() != items[len(items)-1].ID()) {
		return Page{}, NewError(InvalidInput)
	}
	return Page{items: append([]Metadata(nil), items...), next: next, valid: true}, nil
}
func (p Page) Valid() bool       { return p.valid }
func (p Page) Items() []Metadata { return append([]Metadata(nil), p.items...) }
func (p Page) Next() Cursor      { return p.next }

// ArtifactReader owns a bounded payload descriptor. Read through EOF checks
// its SHA256; Close without EOF releases it without asserting integrity.
type ArtifactReader interface {
	io.ReadCloser
	Metadata() Metadata
}
type Provider interface {
	Capabilities() Capabilities
	Write(context.Context, WriteIntent, io.Reader) (WriteResult, error)
	List(context.Context, ListRequest) (Page, error)
	Open(context.Context, ArtifactID) (ArtifactReader, error)
	Delete(context.Context, ArtifactID) (DeleteResult, error)
	Verify(context.Context, ArtifactID) (VerifiedArtifact, error)
}
type ManagedProvider interface {
	Provider
	Close() error
}
type Factory interface {
	Kind() ProviderKind
	Open(context.Context, InstanceSettings) (ManagedProvider, error)
}
