package server

import (
	"context"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

func authInvalid(value proto.Message, field string, rule api.ValidationReason) apierrors.Fault {
	violation, err := apierrors.NewViolation(value.ProtoReflect().Descriptor(), field, rule)
	if err != nil {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	return apierrors.Validation(violation)
}
func authShape(value proto.Message) error {
	if value == nil {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	if err := contracts.AdmitAuthenticationRequestShape(value); err != nil {
		return authInvalid(value, "", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
	}
	return nil
}

func authInteractiveActor(ctx context.Context, coordinator *sessionCoordinator) (contracts.AccountID, error) {
	if coordinator == nil || coordinator.authority == nil {
		return contracts.AccountID{}, storage.ErrAccountUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
		return contracts.AccountID{}, errSessionUnauthenticated
	}
	actor, err := admittedActor(ctx)
	account, accountErr := principal.Account()
	if err != nil || accountErr != nil || actor != account {
		return contracts.AccountID{}, errSessionUnauthenticated
	}
	if _, err := coordinator.authority.Current(principal); err != nil {
		return contracts.AccountID{}, err
	}
	return actor, nil
}

func authAccountTarget(ctx context.Context, coordinator *sessionCoordinator, names authResourceNames, parent string, allowAdmin bool, operation native.ScopeOperation) (contracts.AccountID, error) {
	actor, err := authInteractiveActor(ctx, coordinator)
	if err != nil {
		return contracts.AccountID{}, err
	}
	self, err := names.account(ctx, actor)
	if err != nil {
		return contracts.AccountID{}, err
	}
	if parent == self.String() {
		return actor, nil
	}
	if !allowAdmin {
		return contracts.AccountID{}, authorization.ErrDenied
	}
	if _, err := authAdministrativeActor(ctx, coordinator, operation); err != nil {
		return contracts.AccountID{}, err
	}
	name, err := apitypes.ParseAccountName(parent)
	if err != nil {
		return contracts.AccountID{}, storage.ErrAccountInvalid
	}
	return names.resolveAccount(ctx, name)
}

func nativeAccountRole(value api.AccountRole) (native.AccountRole, error) {
	switch value {
	case api.AccountRole_ACCOUNT_ROLE_ADMIN:
		return native.AccountRole_ACCOUNT_ROLE_ADMIN, nil
	case api.AccountRole_ACCOUNT_ROLE_USER:
		return native.AccountRole_ACCOUNT_ROLE_USER, nil
	default:
		return 0, storage.ErrAccountInvalid
	}
}
func publicAccountRole(value native.AccountRole) (api.AccountRole, error) {
	switch value {
	case native.AccountRole_ACCOUNT_ROLE_ADMIN:
		return api.AccountRole_ACCOUNT_ROLE_ADMIN, nil
	case native.AccountRole_ACCOUNT_ROLE_USER:
		return api.AccountRole_ACCOUNT_ROLE_USER, nil
	default:
		return 0, storage.ErrAPIResourceIntegrity
	}
}
func nativeAccountState(value api.AccountState) (native.AccountStatus, error) {
	switch value {
	case api.AccountState_ACCOUNT_STATE_ACTIVE:
		return native.AccountStatus_ACCOUNT_STATUS_ACTIVE, nil
	case api.AccountState_ACCOUNT_STATE_DISABLED:
		return native.AccountStatus_ACCOUNT_STATUS_DISABLED, nil
	default:
		return 0, storage.ErrAccountInvalid
	}
}

// Authentication and current administrative rights precede all request parsing.
// The native storage operation rechecks role/status inside its owner transaction.
func authAdministrativeActor(ctx context.Context, coordinator *sessionCoordinator, operation native.ScopeOperation) (contracts.AccountID, error) {
	if coordinator == nil || coordinator.authority == nil {
		return contracts.AccountID{}, storage.ErrAccountUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
		return contracts.AccountID{}, errSessionUnauthenticated
	}
	actor, err := admittedActor(ctx)
	account, accountErr := principal.Account()
	if err != nil || accountErr != nil || account != actor {
		return contracts.AccountID{}, errSessionUnauthenticated
	}
	current, err := coordinator.authority.Current(principal)
	if err != nil {
		return contracts.AccountID{}, err
	}
	permission, err := authorization.NewPermission(native.ScopeFamily_SCOPE_FAMILY_ACCOUNTS, operation)
	if err != nil {
		return contracts.AccountID{}, err
	}
	object, err := authorization.AdministrativeObject(native.ScopeFamily_SCOPE_FAMILY_ACCOUNTS)
	if err != nil {
		return contracts.AccountID{}, err
	}
	if err := current.Authorize(permission, object); err != nil {
		return contracts.AccountID{}, err
	}
	return actor, nil
}

func accountAPIChanges(request *api.UpdateAccountRequest, current storage.AccountRecord) (storage.AccountChanges, error) {
	value := request.GetAccount()
	if value == nil {
		return storage.AccountChanges{}, authInvalid(request, "account", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	}
	expected, err := storage.ParseAccountVersion(value.Etag)
	if err != nil {
		return storage.AccountChanges{}, authInvalid(request, "account.etag", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
	}
	changes := storage.AccountChanges{Expected: &expected}
	paths := []string{}
	if request.UpdateMask == nil {
		if value.DisplayName != "" {
			paths = append(paths, "display_name")
		}
		if value.Role != api.AccountRole_ACCOUNT_ROLE_UNSPECIFIED {
			paths = append(paths, "role")
		}
		if value.State != api.AccountState_ACCOUNT_STATE_UNSPECIFIED {
			paths = append(paths, "state")
		}
		if value.LoginName != "" {
			paths = append(paths, "login_name")
		}
		if value.InitialPassword != "" {
			paths = append(paths, "initial_password")
		}
	} else {
		paths = request.UpdateMask.Paths
	}
	if len(paths) == 1 && paths[0] == "*" {
		paths = []string{"display_name", "role", "state"}
		if value.LoginName != "" {
			paths = append(paths, "login_name")
		}
		if value.InitialPassword != "" {
			paths = append(paths, "initial_password")
		}
	}
	for _, path := range paths {
		switch path {
		case "display_name":
			display, err := accounts.ParseDisplayName(value.DisplayName)
			if err != nil {
				return storage.AccountChanges{}, authInvalid(request, "account.display_name", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
			}
			changes.Display = &display
		case "role":
			role, err := nativeAccountRole(value.Role)
			if err != nil {
				return storage.AccountChanges{}, authInvalid(request, "account.role", api.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE)
			}
			changes.Role = &role
		case "state":
			state, err := nativeAccountState(value.State)
			if err != nil {
				return storage.AccountChanges{}, authInvalid(request, "account.state", api.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE)
			}
			changes.Status = &state
		case "login_name":
			login, err := current.Login()
			if err != nil {
				return storage.AccountChanges{}, err
			}
			display, _, err := login.Values()
			if err != nil {
				return storage.AccountChanges{}, err
			}
			if value.LoginName != display {
				return storage.AccountChanges{}, authInvalid(request, "account.login_name", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
			}
		case "name", "etag", "create_time", "update_time", "second_factor_enabled":
			// Identifier/CAS handled independently; output-only fields ignored.
		default:
			return storage.AccountChanges{}, authInvalid(request, "update_mask", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
		}
	}
	return changes, nil
}
