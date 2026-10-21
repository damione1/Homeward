package integrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync/atomic"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

// InternalAutomationPort is lent only by the Run-owned durable service to its
// script composition. Its unexported identity cannot be selected by wire data,
// a client-supplied context value, an ActorID, or a zero-value constructor.
type InternalAutomationPort struct{ service *DurableCommandService }

type internalOccurrenceKey struct{}
type internalOccurrence struct {
	port  *InternalAutomationPort
	cause string
	live  atomic.Bool
}

func (s *DurableCommandService) InternalAutomationPort() (*InternalAutomationPort, error) {
	if s == nil || s.router == nil || s.store == nil || s.router.internal == nil || s.router.internal.service != s {
		return nil, ErrInvalidInput
	}
	return s.router.internal, nil
}

func (p *InternalAutomationPort) WithOccurrence(ctx context.Context, cause string, fn func(context.Context) error) error {
	if p == nil || p.service == nil || p.service.router == nil || p.service.router.internal != p ||
		nilInterface(ctx) || fn == nil || cause == "" || len(cause) > maxVolatileIntentBytes {
		return ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, present := ctx.Value(internalOccurrenceKey{}).(*internalOccurrence); present {
		return ErrInvalidInput
	}
	if _, err := authorization.PrincipalFromContext(ctx); err == nil {
		return ErrCommandAuthorizationDenied
	}
	if err := p.service.admitOccurrence(ctx); err != nil {
		return err
	}
	state := &internalOccurrence{port: p, cause: cause}
	state.live.Store(true)
	defer func() {
		state.live.Store(false)
		p.service.endOccurrence()
	}()
	return fn(context.WithValue(ctx, internalOccurrenceKey{}, state))
}

func (p *InternalAutomationPort) ValidContext(ctx context.Context) bool {
	return p != nil && p.service != nil && p.service.router != nil && p.service.router.internal == p &&
		validInternalOccurrence(ctx, p) != nil
}

// SameOccurrence compares two live, privately admitted contexts. It exposes
// no token that can be serialized or used to enter the internal command path.
func (p *InternalAutomationPort) SameOccurrence(first, second context.Context) bool {
	if p == nil || p.service == nil || p.service.router == nil || p.service.router.internal != p {
		return false
	}
	firstState := validInternalOccurrence(first, p)
	return firstState != nil && firstState == validInternalOccurrence(second, p) && firstState.live.Load()
}

func (p *InternalAutomationPort) BelongsTo(service *DurableCommandService) bool {
	return p != nil && service != nil && p.service == service && service.router != nil && service.router.internal == p
}

func validInternalOccurrence(ctx context.Context, p *InternalAutomationPort) *internalOccurrence {
	if nilInterface(ctx) || p == nil {
		return nil
	}
	state, ok := ctx.Value(internalOccurrenceKey{}).(*internalOccurrence)
	if !ok || state == nil || state.port != p || !state.live.Load() {
		return nil
	}
	if _, err := authorization.PrincipalFromContext(ctx); err == nil {
		return nil
	}
	return state
}

func (r *CommandRouter) internalFromContext(ctx context.Context) (*internalOccurrence, error) {
	if r == nil || nilInterface(ctx) {
		return nil, ErrInvalidInput
	}
	if ctx.Value(internalOccurrenceKey{}) == nil {
		return nil, nil
	}
	state := validInternalOccurrence(ctx, r.internal)
	if state == nil {
		return nil, ErrCommandAuthorizationDenied
	}
	return state, nil
}

func (o *internalOccurrence) originFor(id domain.CommandID) (storage.InternalAutomationOriginKey, error) {
	if o == nil || !o.live.Load() || o.port == nil || o.cause == "" {
		return storage.InternalAutomationOriginKey{}, ErrCommandAuthorizationDenied
	}
	text, err := id.Value()
	if err != nil {
		return storage.InternalAutomationOriginKey{}, ErrInvalidInput
	}
	h := sha256.New()
	_, _ = h.Write([]byte("homeward/internal-occurrence/v1\x00"))
	_, _ = h.Write([]byte(o.cause))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	return storage.NewInternalAutomationOriginKey("automation-" + hex.EncodeToString(h.Sum(nil)))
}

func (o *internalOccurrence) fingerprint(id domain.CommandID, wire *actions.CommandRequest) (storage.InternalAutomationOriginKey, storage.InternalAutomationFingerprint, error) {
	key, err := o.originFor(id)
	if err != nil || wire == nil {
		return storage.InternalAutomationOriginKey{}, storage.InternalAutomationFingerprint{}, ErrInvalidInput
	}
	canonical, err := (proto.MarshalOptions{Deterministic: true}).Marshal(wire)
	if err != nil || len(canonical) == 0 || len(canonical) > maxVolatileIntentBytes {
		return storage.InternalAutomationOriginKey{}, storage.InternalAutomationFingerprint{}, ErrCommandIntentTooLarge
	}
	fingerprint, err := storage.NewInternalAutomationFingerprint(key, canonical)
	return key, fingerprint, err
}

func captureActorValid(actor domain.ActorID, internal *internalOccurrence) bool {
	if internal != nil {
		return internal.live.Load() && actor == (domain.ActorID{})
	}
	_, err := actor.Value()
	return err == nil
}

// Dispatch is the unprepared climate path. Prepared light, fan, switch and
// scene members enter the same service through their bound preparation tokens.
func (p *InternalAutomationPort) Dispatch(ctx context.Context, request *actions.CommandRequest) (*actions.CommandResponse, error) {
	if !p.ValidContext(ctx) || request == nil {
		return nil, ErrCommandAuthorizationDenied
	}
	return p.service.dispatch(ctx, domain.ActorID{}, request, nil)
}
