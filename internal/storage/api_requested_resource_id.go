package storage

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// APIRequestedResourceID is a caller-chosen public identifier. It cannot be used
// as a native domain identity or an authorization credential.
type APIRequestedResourceID struct{ value string }

func ParseAPIRequestedResourceID(value string) (APIRequestedResourceID, error) {
	id := APIRequestedResourceID{value: value}
	if !id.valid() {
		return APIRequestedResourceID{}, ErrAPIResourceIdentity
	}
	return id, nil
}
func (id APIRequestedResourceID) valid() bool {
	return apiResourceID(id.value) && id.value[0] >= 'a' && id.value[0] <= 'z'
}
func (id APIRequestedResourceID) String() string { return id.value }

var ErrAPIResourceAlreadyExists = errors.New("API resource identity already exists")

// InsertAPIResourceIdentityTx reserves the requested name in the existing index.
// The native owner must call it in the same transaction as creation and any
// native durable request receipt. A collision never updates or aliases another object.
func InsertAPIResourceIdentityTx(ctx context.Context, tx pgx.Tx, kind APIResourceType, nativeKey string, id APIRequestedResourceID) (APIResourceIdentity, error) {
	if ctx == nil || tx == nil || kind.Collection() == "" || !id.valid() || len(nativeKey) < 1 || len(nativeKey) > 2048 || strings.ContainsRune(nativeKey, 0) {
		return APIResourceIdentity{}, ErrAPIResourceIdentity
	}
	_, err := tx.Exec(ctx, `INSERT INTO homeward_api_resource_names(resource_type,native_key,resource_id) VALUES ($1,$2,$3)`, kind.Collection(), nativeKey, id.value)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return APIResourceIdentity{}, ErrAPIResourceAlreadyExists
		}
		return APIResourceIdentity{}, ErrAPIResourceUnavailable
	}
	return APIResourceIdentity{resourceType: kind.Collection(), nativeKey: nativeKey, resourceID: id.value}, nil
}
