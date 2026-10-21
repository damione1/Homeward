package server

import (
	"context"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
)

type policyViewAuthorizer struct {
	authority *authorization.Authority
	source    *sequencer.Sequencer
}

var _ view.Authorizer = (*policyViewAuthorizer)(nil)
var _ view.GrantChangeNotifier = (*policyViewAuthorizer)(nil)

func (a *policyViewAuthorizer) principal(ctx context.Context, actor domain.ActorID) (authorization.Principal, error) {
	if a == nil || a.authority == nil || a.source == nil {
		return authorization.Principal{}, authorization.ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return authorization.Principal{}, err
	}
	admitted, err := principal.Actor()
	if err != nil || admitted != actor {
		return authorization.Principal{}, authorization.ErrDenied
	}
	return principal, nil
}
func (a *policyViewAuthorizer) CurrentGrant(ctx context.Context, actor view.ActorID) (view.Grant, error) {
	principal, err := a.principal(ctx, actor)
	if err != nil {
		return view.Grant{}, err
	}
	current, err := a.authority.Current(principal)
	if err != nil {
		return view.Grant{}, err
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return view.Grant{}, err
	}
	if err := current.AdmitRoute(permission); err != nil {
		return view.Grant{}, err
	}
	snapshot, err := a.source.CaptureBounded(view.MaxSnapshotBytes)
	if err != nil {
		return view.Grant{}, err
	}
	access := make([]view.EntityAccess, 0, len(snapshot.Capabilities))
	for entity, descriptions := range snapshot.Capabilities {
		record, present := snapshot.Registry[entity]
		if !present || record.Retired() {
			continue
		}
		all := make([]authorization.CapabilitySlot, 0, len(descriptions))
		readable := make([]authorization.CapabilitySlot, 0, len(descriptions))
		for _, description := range descriptions {
			if description == nil {
				return view.Grant{}, authorization.ErrUnavailable
			}
			slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
			if err != nil {
				return view.Grant{}, err
			}
			all = append(all, slot)
			object, err := authorization.EntityObject(entity, slot)
			if err != nil {
				return view.Grant{}, err
			}
			if current.Authorize(permission, object) == nil {
				readable = append(readable, slot)
			}
		}
		if len(readable) == 0 {
			continue
		}
		entry, err := view.NewEntityAccess(entity, all, readable)
		if err != nil {
			return view.Grant{}, err
		}
		access = append(access, entry)
	}
	revision, binding, err := current.Binding()
	if err != nil {
		return view.Grant{}, err
	}
	return view.NewScopedGrant(actor, revision, binding, access)
}
func (a *policyViewAuthorizer) SubscribeGrantChanges(ctx context.Context, actor view.ActorID) (<-chan struct{}, func(), error) {
	principal, err := a.principal(ctx, actor)
	if err != nil {
		return nil, nil, err
	}
	sub, err := a.authority.Subscribe(ctx, principal)
	if err != nil {
		return nil, nil, err
	}
	return sub.Changes(), sub.Close, nil
}
