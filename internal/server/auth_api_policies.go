package server

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strings"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type authPolicyKind uint8

const (
	authAuthorizationPolicy authPolicyKind = iota + 1
	authSecondFactorPolicy
)

func (k authPolicyKind) label() string {
	switch k {
	case authAuthorizationPolicy:
		return "authorization-policy"
	case authSecondFactorPolicy:
		return "second-factor-policy"
	default:
		return ""
	}
}
func authPolicyEtag(kind authPolicyKind, revision uint64, hash [32]byte) (string, error) {
	if kind.label() == "" || revision < 1 || revision > math.MaxInt64 {
		return "", storage.ErrAPIResourceIntegrity
	}
	var value [40]byte
	binary.BigEndian.PutUint64(value[:8], revision)
	copy(value[8:], hash[:])
	return `W/"` + kind.label() + `-` + base64.RawURLEncoding.EncodeToString(value[:]) + `"`, nil
}
func parseAuthPolicyEtag(kind authPolicyKind, etag string) (uint64, [32]byte, error) {
	var hash [32]byte
	if kind.label() == "" {
		return 0, hash, storage.ErrAccountInvalid
	}
	value, ok := strings.CutPrefix(etag, `W/"`+kind.label()+`-`)
	if !ok {
		return 0, hash, storage.ErrAccountInvalid
	}
	value, ok = strings.CutSuffix(value, `"`)
	if !ok {
		return 0, hash, storage.ErrAccountInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) != 40 || base64.RawURLEncoding.EncodeToString(data) != value {
		return 0, hash, storage.ErrAccountInvalid
	}
	revision := binary.BigEndian.Uint64(data[:8])
	if revision < 1 || revision > math.MaxInt64 {
		return 0, hash, storage.ErrAccountInvalid
	}
	copy(hash[:], data[8:])
	return revision, hash, nil
}

func (n authResourceNames) secondFactorPolicyResource(ctx context.Context, snapshot storage.MFAPolicySnapshot) (*api.SecondFactorPolicy, error) {
	if nilReactiveInterface(n.home) {
		return nil, storage.ErrAPIResourceUnavailable
	}
	home, err := n.home.HomeName(ctx)
	if err != nil {
		return nil, err
	}
	source, err := snapshot.Source()
	if err != nil {
		return nil, err
	}
	required, err := source.Required()
	if err != nil {
		return nil, err
	}
	version, err := snapshot.Version()
	if err != nil {
		return nil, err
	}
	revision, hash, err := version.Values()
	if err != nil {
		return nil, err
	}
	etag, err := authPolicyEtag(authSecondFactorPolicy, revision, hash)
	if err != nil {
		return nil, err
	}
	updated, err := snapshot.UpdatedAt()
	if err != nil {
		return nil, err
	}
	return &api.SecondFactorPolicy{Name: home.String() + "/secondFactorPolicy", RequireAdminFactor: required, UpdateTime: timestamppb.New(updated), Etag: etag}, nil
}

func (h *accountResourceHandler) GetSecondFactorPolicy(ctx context.Context, request *connect.Request[api.GetSecondFactorPolicyRequest]) (*connect.Response[api.SecondFactorPolicy], error) {
	admission, err := admitAuthAdministrativeRead(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer admission.close()
	ctx = admission.permit.Context()
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.GetSecondFactorPolicyRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	name, err := apitypes.ParseSecondFactorPolicyName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	if err := h.names.parent(ctx, name.Home().String()); err != nil {
		return nil, authAPIError(ctx, err)
	}
	snapshot, err := h.store.MFAPolicy(ctx)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.secondFactorPolicyResource(ctx, snapshot)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return authReadResponse(ctx, admission, resource)
}

func (h *accountResourceHandler) ChangeSecondFactorPolicy(ctx context.Context, request *connect.Request[api.ChangeSecondFactorPolicyRequest]) (*connect.Response[api.ChangeSecondFactorPolicyResponse], error) {
	actor, err := authAdministrativeActor(ctx, h.coordinator, native.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.ChangeSecondFactorPolicyRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	name, err := apitypes.ParseSecondFactorPolicyName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	if err := h.names.parent(ctx, name.Home().String()); err != nil {
		return nil, authAPIError(ctx, err)
	}
	revision, hash, err := parseAuthPolicyEtag(authSecondFactorPolicy, r.Etag)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "etag", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	expected, err := authmfa.NewExpectedPolicyVersion(revision, hash[:])
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if h.coordinator.mfa == nil {
		return nil, authAPIError(ctx, storage.ErrAccountUnavailable)
	}
	if r.RequireAdminFactor {
		if err := h.coordinator.mfa.requireReady(); err != nil {
			return nil, authAPIError(ctx, err)
		}
	}
	if err := h.coordinator.reconcileMFAPolicy(ctx); err != nil {
		return nil, authAPIError(ctx, err)
	}
	owner, err := h.coordinator.policy.beginReplace(ctx, expected)
	if err != nil {
		return nil, authPolicyAPIError(ctx, err)
	}
	defer owner.abandon()
	result, err := h.coordinator.mfa.repository.SetMFAPolicy(owner.ctx, actor, expected, authmfa.NewPolicySource(r.RequireAdminFactor), h.coordinator.clock.Now())
	if err != nil {
		if authmfa.KindOf(err) == authmfa.ErrorCommitUnknown {
			known := result.AttemptedAccounts()
			_ = owner.setUnknown(known)
			h.coordinator.quarantineMFAPolicyAccounts(known)
		} else {
			_ = owner.rollback()
		}
		return nil, authPolicyAPIError(ctx, err)
	}
	known := result.AttemptedAccounts()
	policy, returned, _, err := result.Result()
	if err == nil {
		known = returned
	}
	if err == nil {
		err = h.coordinator.publishMFAPolicyAccounts(owner.ctx, known)
	}
	if err != nil {
		_ = owner.setUnknown(known)
		h.coordinator.quarantineMFAPolicyAccounts(known)
		return nil, authPolicyAPIError(ctx, err)
	}
	version, err := policy.Version()
	if err == nil {
		err = owner.publish(version)
	}
	if err != nil {
		_ = owner.setUnknown(known)
		h.coordinator.quarantineMFAPolicyAccounts(known)
		return nil, authPolicyAPIError(ctx, err)
	}
	resource, err := h.names.secondFactorPolicyResource(ctx, policy)
	if err != nil {
		return nil, authPolicyAPIError(ctx, err)
	}
	continuation := api.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_CONTINUES
	for _, id := range known {
		if id == actor {
			continuation = api.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_REAUTHENTICATION_REQUIRED
			break
		}
	}
	return connect.NewResponse(&api.ChangeSecondFactorPolicyResponse{Policy: resource, Continuation: continuation}), nil
}
func authPolicyAPIError(ctx context.Context, err error) error {
	if authmfa.KindOf(err) == authmfa.ErrorConflict {
		err = storage.ErrAccountVersionConflict
	}
	return authAPIError(ctx, err)
}

func (n authResourceNames) authorizationPolicyResource(ctx context.Context, snapshot storage.AuthorizationPolicySnapshot) (*api.AuthorizationPolicy, error) {
	source, err := snapshot.Source()
	if err != nil {
		return nil, err
	}
	account, err := source.Account()
	if err != nil {
		return nil, err
	}
	name, err := n.account(ctx, account)
	if err != nil {
		return nil, err
	}
	data, err := source.Bytes()
	if err != nil {
		return nil, err
	}
	version, err := snapshot.Version()
	if err != nil {
		return nil, err
	}
	revision, hash, err := version.Values()
	if err != nil {
		return nil, err
	}
	etag, err := authPolicyEtag(authAuthorizationPolicy, revision, hash)
	if err != nil {
		return nil, err
	}
	updated, err := snapshot.UpdatedAt()
	if err != nil {
		return nil, err
	}
	return &api.AuthorizationPolicy{Name: name.String() + "/authorizationPolicy", YamlSource: data, Schema: authorization.Schema, SyntaxProfile: authorization.SyntaxProfile, Origin: api.AuthorizationPolicyOrigin_AUTHORIZATION_POLICY_ORIGIN_STORED, UpdateTime: timestamppb.New(updated), Etag: etag}, nil
}

func (h *accountResourceHandler) GetAuthorizationPolicy(ctx context.Context, request *connect.Request[api.GetAuthorizationPolicyRequest]) (*connect.Response[api.AuthorizationPolicy], error) {
	admission, err := admitAuthAdministrativeRead(ctx, h.coordinator)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	defer admission.close()
	ctx = admission.permit.Context()
	actor := admission.actor
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.GetAuthorizationPolicyRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	name, err := apitypes.ParseAuthorizationPolicyName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	account, err := h.names.resolveAccount(ctx, name.Account())
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	snapshot, err := h.store.GetAuthorizationPolicy(ctx, actor, account)
	if errors.Is(err, storage.ErrPolicyNotConfigured) {
		// The native owner already evaluates an absent configurable document
		// as EmptySource. Expose that effective source without inventing a
		// stored revision, timestamp or CAS precondition.
		source, sourceErr := authorization.EmptySource(account)
		if sourceErr != nil {
			return nil, authAPIError(ctx, sourceErr)
		}
		data, sourceErr := source.Bytes()
		if sourceErr != nil {
			return nil, authAPIError(ctx, sourceErr)
		}
		return authReadResponse(ctx, admission, &api.AuthorizationPolicy{
			Name: name.String(), YamlSource: data, Schema: authorization.Schema,
			SyntaxProfile: authorization.SyntaxProfile,
			Origin:        api.AuthorizationPolicyOrigin_AUTHORIZATION_POLICY_ORIGIN_DEFAULT,
		})
	}
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.authorizationPolicyResource(ctx, snapshot)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	return authReadResponse(ctx, admission, resource)
}

func (h *accountResourceHandler) ReplaceAuthorizationPolicy(ctx context.Context, request *connect.Request[api.ReplaceAuthorizationPolicyRequest]) (*connect.Response[api.ReplaceAuthorizationPolicyResponse], error) {
	actor, err := authAdministrativeActor(ctx, h.coordinator, native.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, authAPIError(ctx, authInvalid(&api.ReplaceAuthorizationPolicyRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	if err := authShape(r); err != nil {
		return nil, authAPIError(ctx, err)
	}
	name, err := apitypes.ParseAuthorizationPolicyName(r.Name)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	target, err := h.names.resolveAccount(ctx, name.Account())
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	source, err := authorization.AdmitPolicySource(target, r.YamlSource)
	if err != nil {
		return nil, authAPIError(ctx, authInvalid(r, "yaml_source", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	var expected authorization.ExpectedVersion
	switch condition := r.Precondition.(type) {
	case *api.ReplaceAuthorizationPolicyRequest_ExpectAbsent:
		if condition == nil || condition.ExpectAbsent == nil {
			return nil, authAPIError(ctx, authInvalid(r, "expect_absent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
		}
		expected = authorization.ExpectAbsent()
	case *api.ReplaceAuthorizationPolicyRequest_ExpectedEtag:
		if condition == nil {
			return nil, authAPIError(ctx, authInvalid(r, "expected_etag", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
		}
		revision, hash, parseErr := parseAuthPolicyEtag(authAuthorizationPolicy, condition.ExpectedEtag)
		if parseErr != nil {
			return nil, authAPIError(ctx, authInvalid(r, "expected_etag", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
		}
		version, parseErr := authorization.NewVersion(revision, hash)
		if parseErr != nil {
			return nil, authAPIError(ctx, parseErr)
		}
		expected, err = authorization.ExpectVersion(version)
		if err != nil {
			return nil, authAPIError(ctx, err)
		}
	default:
		return nil, authAPIError(ctx, authInvalid(r, "", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	guard, err := h.accountMutationGuard(ctx, target)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	if err := guard.mutation.ReservePolicy(source); err != nil {
		guard.release()
		return nil, authAPIError(ctx, err)
	}
	outcome, err := h.store.ReplaceAuthorizationPolicy(ctx, actor, target, source, expected)
	var snapshot storage.AuthorizationPolicySnapshot
	var changed bool
	if err == nil {
		snapshot, changed, err = outcome.Result()
		if err != nil {
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
			guard.release()
			return nil, authAPIError(ctx, err)
		}
	}
	if err == nil && changed {
		guard.cancelAllMCPLeases()
	}
	if err := finishAccountMutation(guard, ctx, err, false); err != nil {
		return nil, authAPIError(ctx, err)
	}
	resource, err := h.names.authorizationPolicyResource(ctx, snapshot)
	if err != nil {
		return nil, authAPIError(ctx, err)
	}
	rights := api.PolicyRightsChange_POLICY_RIGHTS_CHANGE_UNCHANGED
	if changed {
		rights = api.PolicyRightsChange_POLICY_RIGHTS_CHANGE_CHANGED
	}
	return connect.NewResponse(&api.ReplaceAuthorizationPolicyResponse{Policy: resource, RightsChange: rights}), nil
}
