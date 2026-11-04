package lists

import (
	"context"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

// ListPrecondition is closed to native revision CAS and resource etag CAS.
// All checks run inside the owner's existing fenced SQL transaction.
type ListPrecondition interface {
	checkList(context.Context, pgx.Tx, domain.ListID, ListSnapshot) error
}
type ItemPrecondition interface {
	checkItem(domain.ListID, ItemUID, ItemSnapshot) error
}

func (r ListRevision) checkList(_ context.Context, _ pgx.Tx, id domain.ListID, current ListSnapshot) error {
	gotID, number, err := r.Values()
	if err != nil || gotID != id {
		return failure(Invalid)
	}
	_, actual, _ := current.revision.Values()
	if number != actual {
		return failure(Conflict)
	}
	if actual == math.MaxInt64 {
		return failure(Capacity)
	}
	return nil
}
func (r ItemRevision) checkItem(list domain.ListID, uid ItemUID, current ItemSnapshot) error {
	gotList, gotUID, number, err := r.Values()
	if err != nil || gotList != list || gotUID != uid {
		return failure(Invalid)
	}
	_, _, actual, _ := current.revision.Values()
	if number != actual {
		return failure(Conflict)
	}
	if actual == math.MaxInt64 {
		return failure(Capacity)
	}
	return nil
}

type resourceETag struct {
	list   domain.ListID
	uid    ItemUID
	token  string
	encode func(ListRevision, ItemRevision) (string, error)
}

func validETag(token string) bool {
	if !utf8.ValidString(token) || len(token) < 1 || len(token) > 256 {
		return false
	}
	for _, character := range token {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

// ExpectedListETag admits token representation, never a revision codec. The
// pure encoder binds the emitted representation to the locked current list.
func ExpectedListETag(id domain.ListID, token string, encode func(ListRevision) (string, error)) (ListPrecondition, error) {
	if _, err := id.Value(); err != nil || !validETag(token) || encode == nil {
		return nil, failure(Invalid)
	}
	return resourceETag{list: id, token: strings.Clone(token), encode: func(list ListRevision, _ ItemRevision) (string, error) { return encode(list) }}, nil
}

// ExpectedItemETag binds one compound token to the exact list and item. Both
// current revisions are read under the list lock before any mutation occurs.
// The token is never decoded, normalized or accepted via a previous codec.
func ExpectedItemETag(id domain.ListID, uid ItemUID, token string, encode func(ListRevision, ItemRevision) (string, error)) (ListPrecondition, ItemPrecondition, error) {
	if _, err := id.Value(); err != nil || !validETag(token) || encode == nil {
		return nil, nil, failure(Invalid)
	}
	if _, err := uid.Value(); err != nil {
		return nil, nil, failure(Invalid)
	}
	condition := resourceETag{id, uid, strings.Clone(token), encode}
	return condition, condition, nil
}
func (e resourceETag) checkList(ctx context.Context, tx pgx.Tx, id domain.ListID, current ListSnapshot) error {
	if id != e.list || e.encode == nil {
		return failure(Invalid)
	}
	var item ItemRevision
	if e.uid.value != "" {
		value, err := readItem(ctx, tx, id, e.uid, false)
		if err != nil {
			return err
		}
		item = value.revision
	}
	token, err := e.encode(current.revision, item)
	if err != nil {
		return failure(Invalid)
	}
	if token != e.token {
		return failure(Conflict)
	}
	_, number, err := current.revision.Values()
	if err != nil {
		return failure(Invalid)
	}
	if number == math.MaxInt64 {
		return failure(Capacity)
	}
	return nil
}
func (e resourceETag) checkItem(id domain.ListID, uid ItemUID, current ItemSnapshot) error {
	if id != e.list || uid != e.uid || uid.value == "" || e.encode == nil {
		return failure(Invalid)
	}
	token, err := e.encode(current.listRevision, current.revision)
	if err != nil {
		return failure(Invalid)
	}
	if token != e.token {
		return failure(Conflict)
	}
	_, _, number, err := current.revision.Values()
	if err != nil {
		return failure(Invalid)
	}
	if number == math.MaxInt64 {
		return failure(Capacity)
	}
	return nil
}
