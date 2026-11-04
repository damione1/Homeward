package server

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"time"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"google.golang.org/protobuf/proto"
)

type historyNameOwner interface {
	HomeName(context.Context) (apitypes.HomeName, error)
	ResolveEntityName(context.Context, apitypes.EntityName) (domain.EntityID, error)
}

// historyV2Handler holds only the native reader and a stateless token cipher.
// The recorder remains the sole query/snapshot/reservation owner.
type historyV2Handler struct {
	native         *historyHandler
	names          historyNameOwner
	epochs         cipher.AEAD
	observationKey [32]byte
}

func newHistoryV2Handler(runtime *historyRuntime, names historyNameOwner) (*historyV2Handler, error) {
	if runtime == nil || runtime.authority == nil || nilReactiveInterface(names) {
		return nil, historyquery.ErrUnavailable
	}
	epochs, err := newAPIPageCipher()
	if err != nil {
		return nil, err
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	return &historyV2Handler{native: &historyHandler{runtime: runtime}, names: names, epochs: epochs, observationKey: key}, nil
}
func (h *historyV2Handler) route(ctx context.Context, kind historyquery.Kind) error {
	if h == nil || h.native == nil || h.native.runtime == nil || h.native.runtime.authority == nil {
		return historyquery.ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return authorization.ErrDenied
	}
	actor, ok := ActorFromContext(ctx)
	expected, err := principal.Actor()
	if err != nil || !ok || actor != expected {
		return authorization.ErrDenied
	}
	family := auth.ScopeFamily_SCOPE_FAMILY_HISTORY
	if kind == historyquery.ExecutionTraces {
		family = auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION
	}
	permission, err := authorization.NewPermission(family, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return err
	}
	return h.native.runtime.authority.WithCurrent(ctx, principal, func(current authorization.Current) error { return current.AdmitRoute(permission) })
}

// selectQuery authorizes the parent against the trusted native catalogue before
// diagnosing period, size, continuation or any other detailed request field.
func (h *historyV2Handler) selectQuery(ctx context.Context, kind historyquery.Kind, message proto.Message) (contracts.HistorySelection, domain.EntityID, error) {
	var zero contracts.HistorySelection
	var entity domain.EntityID
	if err := h.route(ctx, kind); err != nil {
		return zero, entity, err
	}
	parent := ""
	switch request := message.(type) {
	case *api.QueryObservationsRequest:
		if request == nil || kind != historyquery.Observations {
			return zero, entity, historyquery.ErrInvalid
		}
		parent = request.Parent
	case *api.QueryAggregatesRequest:
		if request == nil || kind != historyquery.Aggregates {
			return zero, entity, historyquery.ErrInvalid
		}
		parent = request.Parent
	case *api.QueryExecutionTracesRequest:
		if request == nil || kind != historyquery.ExecutionTraces {
			return zero, entity, historyquery.ErrInvalid
		}
		parent = request.Parent
	default:
		return zero, entity, historyquery.ErrInvalid
	}
	var home apitypes.HomeName
	if kind == historyquery.ExecutionTraces {
		name, err := apitypes.ParseHomeName(parent)
		if err != nil {
			return zero, entity, historyquery.ErrInvalid
		}
		home = name
	} else {
		name, err := apitypes.ParseEntityName(parent)
		if err != nil {
			return zero, entity, historyquery.ErrInvalid
		}
		home = name.Home()
		entity, err = h.names.ResolveEntityName(ctx, name)
		// A native registry miss cannot reveal existence before the entity fence.
		if err != nil {
			return zero, domain.EntityID{}, authorization.ErrDenied
		}
	}
	expected, err := h.names.HomeName(ctx)
	if err != nil {
		return zero, entity, historyquery.ErrUnavailable
	}
	if home != expected {
		return zero, entity, authorization.ErrDenied
	}
	nativeID := ""
	if kind != historyquery.ExecutionTraces {
		nativeID, err = entity.Value()
		if err != nil {
			return zero, entity, historyquery.ErrIntegrity
		}
	}
	// An authorization-only query never reaches SQL and ignores all caller
	// selection fields. Fixed UTC bounds satisfy each native kind's invariants.
	start := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	authorizationQuery, err := historyquery.NewQuery(kind, nativeID, start, start.Add(time.Hour), 1, "")
	if err != nil {
		return zero, entity, err
	}
	if _, err = h.native.capture(ctx, authorizationQuery, nil); err != nil {
		return zero, entity, err
	}
	selection, err := contracts.AdmitHistoryV2Selection(message)
	return selection, entity, err
}
