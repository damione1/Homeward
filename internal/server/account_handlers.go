package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/operator/v1/operatorv1connect"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type accountHandler struct {
	store       *storage.Store
	coordinator *sessionCoordinator
}

func (h *accountHandler) accountMutationGuard(ctx context.Context, target contracts.AccountID) (*sessionGuard, error) {
	if h == nil || h.coordinator == nil {
		return nil, storage.ErrSessionUnavailable
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, err
	}
	guard, err := h.coordinator.enter(ctx, target)
	if err != nil {
		ticket.release()
		return nil, err
	}
	guard.policyTicket = ticket
	guard.mutation, err = h.coordinator.authority.BeginMutation(target)
	if err != nil {
		guard.release()
		return nil, err
	}
	return guard, nil
}

func (h *accountHandler) accountCreationGuard(ctx context.Context, target contracts.AccountID) (*sessionGuard, error) {
	if h == nil || h.coordinator == nil {
		return nil, storage.ErrSessionUnavailable
	}
	// Reconcile unpublished creation IDs before taking this new account's gate.
	// A database outage cannot accumulate a fresh unknown ID on every retry.
	if err := h.coordinator.reconcilePendingCreations(ctx); err != nil {
		return nil, err
	}
	guard, err := h.accountMutationGuard(ctx, target)
	if err == nil {
		guard.creating = true
	}
	return guard, err
}

func finishAccountMutation(guard *sessionGuard, ctx context.Context, err error, revoking bool) error {
	if guard == nil {
		return err
	}
	if err == nil {
		if publishErr := guard.refreshAuthorization(ctx); publishErr != nil {
			// The transaction committed but the current memory projection could
			// not be established. Close admissions until PG reconciliation.
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
			guard.release()
			return storage.ErrAccountUnavailable
		}
	}
	if errors.Is(err, storage.ErrAccountCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	} else if err == nil && revoking {
		guard.cancelLeases(ctx, nil)
	}
	guard.release()
	return err
}

// The initial source is the same admitted empty policy that storage seeds in
// the account's transaction. Reserve it before any account INSERT can commit.
func reserveAccountSeed(guard *sessionGuard) error {
	if guard == nil || guard.mutation == nil {
		return authorization.ErrInvalid
	}
	source, err := authorization.EmptySource(guard.gate.account)
	if err == nil {
		err = guard.mutation.ReservePolicy(source)
	}
	if err != nil {
		guard.release()
	}
	return err
}

var _ operatorv1connect.OperatorAccountServiceHandler = (*accountHandler)(nil)

func accountWire(record storage.AccountRecord) (*authv1.Account, error) {
	accountID, err := record.ID()
	if err != nil {
		return nil, err
	}
	id, err := accountID.Value()
	if err != nil {
		return nil, err
	}
	loginName, err := record.Login()
	if err != nil {
		return nil, err
	}
	login, _, err := loginName.Values()
	if err != nil {
		return nil, err
	}
	displayName, err := record.Display()
	if err != nil {
		return nil, err
	}
	display, err := displayName.Value()
	if err != nil {
		return nil, err
	}
	role, err := record.Role()
	if err != nil {
		return nil, err
	}
	status, err := record.Status()
	if err != nil {
		return nil, err
	}
	created, err := record.CreatedAt()
	if err != nil {
		return nil, err
	}
	updated, err := record.UpdatedAt()
	if err != nil {
		return nil, err
	}
	secondFactor, err := record.SecondFactorEnabled()
	if err != nil {
		return nil, err
	}
	return &authv1.Account{
		Id: &authv1.AccountId{Value: id}, LoginName: login, DisplayName: display,
		Role: role, Status: status, CreatedAt: timestamppb.New(created),
		UpdatedAt: timestamppb.New(updated), SecondFactorEnabled: secondFactor,
	}, nil
}

func admittedActor(ctx context.Context) (contracts.AccountID, error) {
	actor, ok := ActorFromContext(ctx)
	if !ok {
		return contracts.AccountID{}, storage.ErrAccountPermission
	}
	value, err := actor.Value()
	if err != nil {
		return contracts.AccountID{}, storage.ErrAccountPermission
	}
	id, err := contracts.ParseAccountID(value)
	if err != nil {
		return contracts.AccountID{}, storage.ErrAccountPermission
	}
	return id, nil
}

func admittedAccountID(wire *authv1.AccountId) (contracts.AccountID, error) {
	if wire == nil {
		return contracts.AccountID{}, storage.ErrAccountInvalid
	}
	id, err := contracts.ParseAccountID(wire.Value)
	if err != nil {
		return contracts.AccountID{}, storage.ErrAccountInvalid
	}
	return id, nil
}

func newAccountInput(ctx context.Context, loginName, displayName, password string, role authv1.AccountRole) (storage.NewAccount, error) {
	login, err := accounts.ParseLoginName(loginName)
	if err != nil {
		return storage.NewAccount{}, err
	}
	display, err := accounts.ParseDisplayName(displayName)
	if err != nil {
		return storage.NewAccount{}, err
	}
	if err := contracts.AdmitAccountRole(role); err != nil {
		return storage.NewAccount{}, storage.ErrAccountInvalid
	}
	verifier, err := accounts.HashNewPassword(ctx, password)
	if err != nil {
		return storage.NewAccount{}, err
	}
	id, err := accounts.NewAccountID()
	if err != nil {
		return storage.NewAccount{}, storage.ErrAccountUnavailable
	}
	return storage.NewAccount{ID: id, Login: login, Display: display, Role: role, Verifier: verifier}, nil
}

func (h *accountHandler) BootstrapAdmin(ctx context.Context, request *connect.Request[authv1.BootstrapAdminRequest]) (*connect.Response[authv1.BootstrapAdminResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, storage.ErrAccountInvalid)
	}
	input, err := newAccountInput(ctx, request.Msg.LoginName, request.Msg.DisplayName, request.Msg.Password, authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.accountCreationGuard(ctx, input.ID)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := reserveAccountSeed(guard); err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.BootstrapAdmin(ctx, input)
	err = finishAccountMutation(guard, ctx, err, false)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&authv1.BootstrapAdminResponse{Account: account}), nil
}

func (h *accountHandler) RecoverAccount(ctx context.Context, request *connect.Request[authv1.RecoverAccountRequest]) (*connect.Response[authv1.RecoverAccountResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, storage.ErrAccountInvalid)
	}
	id, err := admittedAccountID(request.Msg.AccountId)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	verifier, err := accounts.HashNewPassword(ctx, request.Msg.ReplacementPassword)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	guard, err := h.accountMutationGuard(ctx, id)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	record, err := h.store.RecoverAccount(ctx, id, verifier)
	err = finishAccountMutation(guard, ctx, err, true)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return connect.NewResponse(&authv1.RecoverAccountResponse{Account: account}), nil
}
