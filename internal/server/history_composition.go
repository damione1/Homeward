package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
)

type historyReader interface {
	WithHistoryReadReservation(context.Context, func(context.Context) error) error
	ReadHistoricalRecorderPageAt(context.Context, historyquery.Query, *storage.RecorderQueryCursor, *storage.RecorderLiveHigh) (storage.RecorderQueryPage, error)
	ReadExecutionHistoryPage(context.Context, storage.HistoryPageQuery) (storage.HistoryPage, error)
}
type historyRuntime struct {
	reader        historyReader
	authority     *authorization.Authority
	sequence      *sequencer.Sequencer
	currentConfig func(context.Context) (view.ConfigStamp, error)
	codec         *historyquery.CursorCodec
}

func newHistoryRuntime(store *storage.Store, authority *authorization.Authority, sequence *sequencer.Sequencer, currentConfig func(context.Context) (view.ConfigStamp, error)) (*historyRuntime, error) {
	if store == nil || authority == nil || sequence == nil || currentConfig == nil {
		return nil, historyquery.ErrInvalid
	}
	codec, err := historyquery.NewCursorCodec()
	if err != nil {
		return nil, err
	}
	return &historyRuntime{reader: store, authority: authority, sequence: sequence, currentConfig: currentConfig, codec: codec}, nil
}

type historyAdmission struct {
	principal  authorization.Principal
	permission authorization.Permission
	objects    []authorization.Object
	binding    historyquery.Binding
	liveHigh   *storage.RecorderLiveHigh
}

// capture is memory-only, called under the existing authorization fence. The
// catalogue comes from the server sequencer, never from a caller's schema.
func (h *historyHandler) capture(ctx context.Context, q historyquery.Query, expected *historyquery.Binding) (historyAdmission, error) {
	var admitted historyAdmission
	if h == nil || h.runtime == nil || h.runtime.authority == nil || h.runtime.sequence == nil || h.runtime.currentConfig == nil || ctx == nil {
		return admitted, authorization.ErrUnavailable
	}
	if err := historyContextError(ctx); err != nil {
		return admitted, err
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		if cause := historyContextError(ctx); cause != nil {
			return admitted, cause
		}
		return admitted, authorization.ErrDenied
	}
	actor, ok := ActorFromContext(ctx)
	trusted, actorErr := principal.Actor()
	if !ok || actorErr != nil || actor != trusted {
		return admitted, authorization.ErrDenied
	}
	family := auth.ScopeFamily_SCOPE_FAMILY_HISTORY
	if q.Kind() == historyquery.ExecutionTraces {
		family = auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION
	}
	permission, err := authorization.NewPermission(family, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return admitted, err
	}
	err = h.runtime.authority.WithCurrent(ctx, principal, func(current authorization.Current) error {
		if err := current.AdmitRoute(permission); err != nil {
			return err
		}
		stamp, err := h.runtime.currentConfig(ctx)
		if err != nil {
			return authorization.ErrUnavailable
		}
		revision, configHash, err := stamp.Components()
		if err != nil {
			return authorization.ErrUnavailable
		}
		hash := sha256.New()
		hash.Write([]byte("homeward-history-catalogue-v1"))
		hash.Write(configHash[:])
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], revision)
		hash.Write(n[:])
		objects := make([]authorization.Object, 0)
		var liveHigh *storage.RecorderLiveHigh
		if q.Kind() == historyquery.ExecutionTraces {
			object, err := authorization.AdministrativeObject(family)
			if err != nil {
				return err
			}
			objects = append(objects, object)
		} else {
			// Capture only this entity from the existing trusted projection. The
			// reservation is checked before any declaration is cloned.
			snapshot, err := h.runtime.sequence.CaptureEntityCatalogue(q.Entity(), 1<<20)
			if err != nil {
				if errors.Is(err, sequencer.ErrIdentityConflict) {
					return authorization.ErrDenied
				}
				return authorization.ErrUnavailable
			}
			epoch, sequence, err := snapshot.Cursor().Components()
			if err != nil {
				return authorization.ErrUnavailable
			}
			high, err := storage.NewRecorderLiveHigh(epoch, sequence)
			if err != nil {
				return authorization.ErrUnavailable
			}
			liveHigh = &high
			record := snapshot.Record()
			device, _, err := record.IDs()
			if err != nil {
				return authorization.ErrUnavailable
			}
			deviceName, _ := device.Value()
			instance, key, err := record.Source()
			if err != nil {
				return authorization.ErrUnavailable
			}
			instanceName, _ := instance.Value()
			sourceDevice, sourceEntity, err := key.Components()
			if err != nil {
				return authorization.ErrUnavailable
			}
			for _, value := range []string{deviceName, instanceName, sourceDevice, sourceEntity} {
				binary.BigEndian.PutUint64(n[:], uint64(len(value)))
				hash.Write(n[:])
				hash.Write([]byte(value))
			}
			descriptions := snapshot.Descriptions()
			if len(descriptions) == 0 || len(descriptions) > authorization.MaxSlots {
				return authorization.ErrDenied
			}
			encoded := make([][]byte, 0, len(descriptions))
			seen := make(map[authorization.CapabilitySlot]struct{}, len(descriptions))
			for _, description := range descriptions {
				if description == nil {
					return authorization.ErrUnavailable
				}
				slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
				if err != nil {
					return authorization.ErrUnavailable
				}
				if _, exists := seen[slot]; exists {
					return authorization.ErrUnavailable
				}
				seen[slot] = struct{}{}
				object, err := authorization.EntityObject(q.Entity(), slot)
				if err != nil {
					return err
				}
				objects = append(objects, object)
				wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(description)
				if err != nil {
					return authorization.ErrUnavailable
				}
				encoded = append(encoded, wire)
			}
			sort.Slice(encoded, func(i, j int) bool { return string(encoded[i]) < string(encoded[j]) })
			identity, _ := q.Entity().Value()
			hash.Write([]byte(identity))
			for _, wire := range encoded {
				binary.BigEndian.PutUint64(n[:], uint64(len(wire)))
				hash.Write(n[:])
				hash.Write(wire)
			}
		}
		for _, object := range objects {
			if err := current.Authorize(permission, object); err != nil {
				return err
			}
		}
		_, policy, err := current.Binding()
		if err != nil {
			return err
		}
		var catalogue [32]byte
		copy(catalogue[:], hash.Sum(nil))
		binding, err := historyquery.NewBinding(policy, catalogue)
		if err != nil {
			return err
		}
		if expected != nil && !binding.Equal(*expected) {
			return authorization.ErrDenied
		}
		admitted = historyAdmission{principal: principal, permission: permission, objects: objects, binding: binding, liveHigh: liveHigh}
		return nil
	})
	if cause := historyContextError(ctx); cause != nil {
		return admitted, cause
	}
	return admitted, err
}
func (h *historyHandler) begin(ctx context.Context, q historyquery.Query) (context.Context, historyAdmission, func(), error) {
	admitted, err := h.capture(ctx, q, nil)
	if err != nil {
		return nil, admitted, nil, err
	}
	permits := make([]*authorization.Permit, 0, len(admitted.objects))
	release := func() {
		for i := len(permits) - 1; i >= 0; i-- {
			permits[i].Release()
		}
	}
	current := ctx
	for _, object := range admitted.objects {
		permit, err := h.runtime.authority.Permit(current, admitted.principal, admitted.permission, object)
		if err != nil {
			if cause := historyContextError(current); cause != nil {
				err = cause
			}
			release()
			return nil, admitted, nil, err
		}
		permits = append(permits, permit)
		current = permit.Context()
	}
	if _, err = h.capture(current, q, &admitted.binding); err != nil {
		release()
		return nil, admitted, nil, err
	}
	return current, admitted, release, nil
}

// Preserve the cause that actually ended the lease. PrincipalFromContext must
// not turn a normal request deadline into a permission decision.
func historyContextError(ctx context.Context) error {
	if ctx == nil {
		return authorization.ErrUnavailable
	}
	cause := context.Cause(ctx)
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(cause, authorization.ErrDenied) {
		return authorization.ErrDenied
	}
	if errors.Is(cause, authorization.ErrUnavailable) {
		return authorization.ErrUnavailable
	}
	var terminal authorizationTerminationCause
	if errors.As(cause, &terminal) {
		switch terminal.reason {
		case auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED, auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED, auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_ACCOUNT_DISABLED, auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED:
			return authorization.ErrDenied
		case auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE:
			return authorization.ErrUnavailable
		default:
			return authorization.ErrUnavailable
		}
	}
	return ctx.Err()
}
