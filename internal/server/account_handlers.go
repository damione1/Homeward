package server

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
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
	guard, err := h.coordinator.enter(ctx, target)
	if err != nil {
		return nil, err
	}
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

var _ authv1connect.AccountServiceHandler = (*accountHandler)(nil)
var _ authv1connect.OperatorAccountServiceHandler = (*accountHandler)(nil)

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

func invitationWire(record storage.InvitationRecord) (*authv1.Invitation, error) {
	invitationID, err := record.ID()
	if err != nil {
		return nil, err
	}
	id, err := invitationID.Value()
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
	role, err := record.Role()
	if err != nil {
		return nil, err
	}
	expires, err := record.ExpiresAt()
	if err != nil {
		return nil, err
	}
	created, err := record.CreatedAt()
	if err != nil {
		return nil, err
	}
	accepted, err := record.AcceptedAt()
	if err != nil {
		return nil, err
	}
	revoked, err := record.RevokedAt()
	if err != nil {
		return nil, err
	}
	result := &authv1.Invitation{Id: &authv1.InvitationId{Value: id}, LoginName: login, Role: role,
		ExpiresAt: timestamppb.New(expires), CreatedAt: timestamppb.New(created)}
	if accepted != nil {
		result.AcceptedAt = timestamppb.New(*accepted)
	}
	if revoked != nil {
		result.RevokedAt = timestamppb.New(*revoked)
	}
	return result, nil
}

func accountRPCError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request deadline exceeded"))
	case errors.Is(err, accounts.ErrKDFBusy):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("password derivation busy"))
	case errors.Is(err, authorization.ErrCapacity):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("authorization capacity exhausted"))
	case errors.Is(err, storage.ErrAccountListTooLarge):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("account listing exceeds response limit"))
	case errors.Is(err, accounts.ErrInvalidLogin), errors.Is(err, accounts.ErrInvalidDisplayName),
		errors.Is(err, accounts.ErrInvalidPassword), errors.Is(err, accounts.ErrBlockedPassword),
		errors.Is(err, accounts.ErrInvalidInvitationToken), errors.Is(err, storage.ErrAccountInvalid):
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid account input"))
	case errors.Is(err, storage.ErrAccountPermission):
		return connect.NewError(connect.CodePermissionDenied, errors.New("administrator required"))
	case errors.Is(err, storage.ErrAccountNotFound), errors.Is(err, storage.ErrInvitationNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("account resource not found"))
	case errors.Is(err, storage.ErrAccountConflict):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("account identity conflict"))
	case errors.Is(err, storage.ErrBootstrapCompleted), errors.Is(err, storage.ErrLastActiveAdmin),
		errors.Is(err, storage.ErrInvitationExpired), errors.Is(err, storage.ErrInvitationUnavailable):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("account operation unavailable"))
	default:
		return connect.NewError(connect.CodeUnavailable, errors.New("account service unavailable"))
	}
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

func admittedInvitationID(wire *authv1.InvitationId) (contracts.InvitationID, error) {
	if wire == nil {
		return contracts.InvitationID{}, storage.ErrAccountInvalid
	}
	id, err := contracts.ParseInvitationID(wire.Value)
	if err != nil {
		return contracts.InvitationID{}, storage.ErrAccountInvalid
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
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	input, err := newAccountInput(ctx, request.Msg.LoginName, request.Msg.DisplayName, request.Msg.Password, authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		return nil, accountRPCError(err)
	}
	guard, err := h.accountCreationGuard(ctx, input.ID)
	if err != nil {
		return nil, accountRPCError(err)
	}
	if err := reserveAccountSeed(guard); err != nil {
		return nil, accountRPCError(err)
	}
	record, err := h.store.BootstrapAdmin(ctx, input)
	err = finishAccountMutation(guard, ctx, err, false)
	if err != nil {
		return nil, accountRPCError(err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.BootstrapAdminResponse{Account: account}), nil
}

func (h *accountHandler) RecoverAccount(ctx context.Context, request *connect.Request[authv1.RecoverAccountRequest]) (*connect.Response[authv1.RecoverAccountResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	id, err := admittedAccountID(request.Msg.AccountId)
	if err != nil {
		return nil, accountRPCError(err)
	}
	verifier, err := accounts.HashNewPassword(ctx, request.Msg.ReplacementPassword)
	if err != nil {
		return nil, accountRPCError(err)
	}
	guard, err := h.accountMutationGuard(ctx, id)
	if err != nil {
		return nil, accountRPCError(err)
	}
	record, err := h.store.RecoverAccount(ctx, id, verifier)
	err = finishAccountMutation(guard, ctx, err, true)
	if err != nil {
		return nil, accountRPCError(err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.RecoverAccountResponse{Account: account}), nil
}

func (h *accountHandler) CreateAccount(ctx context.Context, request *connect.Request[authv1.CreateAccountRequest]) (*connect.Response[authv1.CreateAccountResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	input, err := newAccountInput(ctx, request.Msg.LoginName, request.Msg.DisplayName, request.Msg.InitialPassword, request.Msg.Role)
	if err != nil {
		return nil, accountRPCError(err)
	}
	guard, err := h.accountCreationGuard(ctx, input.ID)
	if err != nil {
		return nil, accountRPCError(err)
	}
	if err := reserveAccountSeed(guard); err != nil {
		return nil, accountRPCError(err)
	}
	record, err := h.store.CreateAccount(ctx, actor, input)
	err = finishAccountMutation(guard, ctx, err, false)
	if err != nil {
		return nil, accountRPCError(err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.CreateAccountResponse{Account: account}), nil
}

func (h *accountHandler) GetAccount(ctx context.Context, request *connect.Request[authv1.GetAccountRequest]) (*connect.Response[authv1.GetAccountResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	target, err := admittedAccountID(request.Msg.AccountId)
	if err != nil {
		return nil, accountRPCError(err)
	}
	record, err := h.store.GetAccount(ctx, actor, target)
	if err != nil {
		return nil, accountRPCError(err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.GetAccountResponse{Account: account}), nil
}

func (h *accountHandler) ListAccounts(ctx context.Context, request *connect.Request[authv1.ListAccountsRequest]) (*connect.Response[authv1.ListAccountsResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	records, err := h.store.ListAccounts(ctx, actor)
	if err != nil {
		return nil, accountRPCError(err)
	}
	result := &authv1.ListAccountsResponse{Accounts: make([]*authv1.Account, 0, len(records))}
	for _, record := range records {
		account, err := accountWire(record)
		if err != nil {
			return nil, accountRPCError(err)
		}
		result.Accounts = append(result.Accounts, account)
	}
	return connect.NewResponse(result), nil
}

func (h *accountHandler) UpdateAccount(ctx context.Context, request *connect.Request[authv1.UpdateAccountRequest]) (*connect.Response[authv1.UpdateAccountResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	target, err := admittedAccountID(request.Msg.AccountId)
	if err != nil {
		return nil, accountRPCError(err)
	}
	var changes storage.AccountChanges
	if request.Msg.DisplayName != nil {
		value, err := accounts.ParseDisplayName(*request.Msg.DisplayName)
		if err != nil {
			return nil, accountRPCError(err)
		}
		changes.Display = &value
	}
	if request.Msg.Role != nil {
		role := *request.Msg.Role
		if err := contracts.AdmitAccountRole(role); err != nil {
			return nil, accountRPCError(storage.ErrAccountInvalid)
		}
		changes.Role = &role
	}
	if request.Msg.Status != nil {
		status := *request.Msg.Status
		if status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE && status != authv1.AccountStatus_ACCOUNT_STATUS_DISABLED {
			return nil, accountRPCError(storage.ErrAccountInvalid)
		}
		changes.Status = &status
	}
	guard, err := h.accountMutationGuard(ctx, target)
	if err != nil {
		return nil, accountRPCError(err)
	}
	outcome, err := h.store.UpdateAccountWithOutcome(ctx, actor, target, changes)
	var record storage.AccountRecord
	var rightsChanged bool
	if err == nil {
		record, rightsChanged, err = outcome.Result()
		if err != nil {
			// A committed update without an admitted outcome cannot establish
			// whether existing leases remain authorized.
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
			guard.release()
			return nil, accountRPCError(err)
		}
	}
	err = finishAccountMutation(guard, ctx, err, rightsChanged)
	if err != nil {
		return nil, accountRPCError(err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.UpdateAccountResponse{Account: account}), nil
}

func (h *accountHandler) DeleteAccount(ctx context.Context, request *connect.Request[authv1.DeleteAccountRequest]) (*connect.Response[authv1.DeleteAccountResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	target, err := admittedAccountID(request.Msg.AccountId)
	if err != nil {
		return nil, accountRPCError(err)
	}
	guard, err := h.accountMutationGuard(ctx, target)
	if err != nil {
		return nil, accountRPCError(err)
	}
	err = h.store.DeleteAccount(ctx, actor, target)
	err = finishAccountMutation(guard, ctx, err, true)
	if err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.DeleteAccountResponse{}), nil
}

func (h *accountHandler) CreateInvitation(ctx context.Context, request *connect.Request[authv1.CreateInvitationRequest]) (*connect.Response[authv1.CreateInvitationResponse], error) {
	if request == nil || request.Msg == nil || request.Msg.ExpiresAt == nil || request.Msg.ExpiresAt.CheckValid() != nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	login, err := accounts.ParseLoginName(request.Msg.LoginName)
	if err != nil {
		return nil, accountRPCError(err)
	}
	if err := contracts.AdmitAccountRole(request.Msg.Role); err != nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	expires := request.Msg.ExpiresAt.AsTime()
	if !expires.After(time.Now()) {
		return nil, accountRPCError(storage.ErrInvitationExpired)
	}
	id, err := accounts.NewInvitationID()
	if err != nil {
		return nil, accountRPCError(err)
	}
	token, err := accounts.NewInvitationToken()
	if err != nil {
		return nil, accountRPCError(err)
	}
	digest, err := token.Digest()
	if err != nil {
		return nil, accountRPCError(err)
	}
	record, err := h.store.CreateInvitation(ctx, actor, storage.NewInvitation{ID: id, Login: login, Role: request.Msg.Role, Digest: digest, ExpiresAt: expires})
	if err != nil {
		return nil, accountRPCError(err)
	}
	invitation, err := invitationWire(record)
	if err != nil {
		return nil, accountRPCError(err)
	}
	clearToken, err := token.Text()
	if err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.CreateInvitationResponse{Invitation: invitation, InvitationToken: clearToken}), nil
}

func (h *accountHandler) AcceptInvitation(ctx context.Context, request *connect.Request[authv1.AcceptInvitationRequest]) (*connect.Response[authv1.AcceptInvitationResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	token, err := accounts.ParseInvitationToken(request.Msg.InvitationToken)
	if err != nil {
		return nil, accountRPCError(err)
	}
	digest, err := token.Digest()
	if err != nil {
		return nil, accountRPCError(err)
	}
	display, err := accounts.ParseDisplayName(request.Msg.DisplayName)
	if err != nil {
		return nil, accountRPCError(err)
	}
	verifier, err := accounts.HashNewPassword(ctx, request.Msg.Password)
	if err != nil {
		return nil, accountRPCError(err)
	}
	id, err := accounts.NewAccountID()
	if err != nil {
		return nil, accountRPCError(err)
	}
	guard, err := h.accountCreationGuard(ctx, id)
	if err != nil {
		return nil, accountRPCError(err)
	}
	if err := reserveAccountSeed(guard); err != nil {
		return nil, accountRPCError(err)
	}
	record, err := h.store.AcceptInvitation(ctx, digest, id, display, verifier)
	err = finishAccountMutation(guard, ctx, err, false)
	if err != nil {
		return nil, accountRPCError(err)
	}
	account, err := accountWire(record)
	if err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.AcceptInvitationResponse{Account: account}), nil
}

func (h *accountHandler) ListInvitations(ctx context.Context, request *connect.Request[authv1.ListInvitationsRequest]) (*connect.Response[authv1.ListInvitationsResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	records, err := h.store.ListInvitations(ctx, actor)
	if err != nil {
		return nil, accountRPCError(err)
	}
	result := &authv1.ListInvitationsResponse{Invitations: make([]*authv1.Invitation, 0, len(records))}
	for _, record := range records {
		invitation, err := invitationWire(record)
		if err != nil {
			return nil, accountRPCError(err)
		}
		result.Invitations = append(result.Invitations, invitation)
	}
	return connect.NewResponse(result), nil
}

func (h *accountHandler) RevokeInvitation(ctx context.Context, request *connect.Request[authv1.RevokeInvitationRequest]) (*connect.Response[authv1.RevokeInvitationResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, accountRPCError(storage.ErrAccountInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, accountRPCError(err)
	}
	id, err := admittedInvitationID(request.Msg.InvitationId)
	if err != nil {
		return nil, accountRPCError(err)
	}
	if err := h.store.RevokeInvitation(ctx, actor, id); err != nil {
		return nil, accountRPCError(err)
	}
	return connect.NewResponse(&authv1.RevokeInvitationResponse{}), nil
}

// NHA-142 owns this policy. These procedures are deliberately not registered
// on the public plan by NHA-049.
func (h *accountHandler) GetSecondFactorPolicy(context.Context, *connect.Request[authv1.GetSecondFactorPolicyRequest]) (*connect.Response[authv1.GetSecondFactorPolicyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("second factor policy deferred"))
}

func (h *accountHandler) SetSecondFactorPolicy(context.Context, *connect.Request[authv1.SetSecondFactorPolicyRequest]) (*connect.Response[authv1.SetSecondFactorPolicyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("second factor policy deferred"))
}
