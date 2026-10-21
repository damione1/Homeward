package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type AuditPosition struct {
	upper, before int64
	first, valid  bool
}

func FirstAuditPosition() AuditPosition { return AuditPosition{first: true, valid: true} }
func AuditPositionAfter(upper, before int64) (AuditPosition, error) {
	if upper <= 0 || before <= 0 || before > upper {
		return AuditPosition{}, &Error{kind: InvalidSettings}
	}
	return AuditPosition{upper: upper, before: before, valid: true}, nil
}
func (p AuditPosition) Values() (int64, int64, bool, error) {
	if !p.valid {
		return 0, 0, false, &Error{kind: InvalidSettings}
	}
	return p.upper, p.before, p.first, nil
}

type AuthenticationAuditRead struct {
	size     uint32
	position AuditPosition
	valid    bool
}
type SecretAccessAuditRead struct {
	size     uint32
	position AuditPosition
	valid    bool
}

func NewAuthenticationAuditRead(size observability.PageSize, position AuditPosition) (AuthenticationAuditRead, error) {
	value, err := size.Value()
	if err != nil || !position.valid {
		return AuthenticationAuditRead{}, &Error{kind: InvalidSettings}
	}
	return AuthenticationAuditRead{value, position, true}, nil
}
func NewSecretAccessAuditRead(size observability.PageSize, position AuditPosition) (SecretAccessAuditRead, error) {
	value, err := size.Value()
	if err != nil || !position.valid {
		return SecretAccessAuditRead{}, &Error{kind: InvalidSettings}
	}
	return SecretAccessAuditRead{value, position, true}, nil
}

type AuthenticationAuditRow struct {
	id       int64
	at       time.Time
	action   obs.AuthAuditAction
	outcome  obs.AuditOutcome
	actor    contracts.AccountID
	hasActor bool
	route    string
	hasRoute bool
	invalid  []obs.StoredMetadataField
}

func (r AuthenticationAuditRow) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: authentication audit row"))
}
func (r AuthenticationAuditRow) ID() int64             { return r.id }
func (r AuthenticationAuditRow) At() (time.Time, bool) { return r.at, r.id > 0 && len(r.invalid) == 0 }
func (r AuthenticationAuditRow) Action() obs.AuthAuditAction {
	if r.id <= 0 || len(r.invalid) != 0 {
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_UNSPECIFIED
	}
	return r.action
}
func (r AuthenticationAuditRow) Outcome() obs.AuditOutcome {
	if r.id <= 0 || len(r.invalid) != 0 {
		return obs.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED
	}
	return r.outcome
}
func (r AuthenticationAuditRow) Actor() (contracts.AccountID, bool) {
	return r.actor, r.id > 0 && len(r.invalid) == 0 && r.hasActor
}

// This is private operational metadata for the server's frozen PublicPlan
// whitelist. It must never be directly serialized as a registered route.
func (r AuthenticationAuditRow) RouteMetadata() (string, bool) {
	return r.route, r.id > 0 && len(r.invalid) == 0 && r.hasRoute
}
func (r AuthenticationAuditRow) MetadataInvalid() *obs.StoredMetadataInvalid {
	if len(r.invalid) == 0 {
		return nil
	}
	return &obs.StoredMetadataInvalid{Fields: append([]obs.StoredMetadataField(nil), r.invalid...)}
}

type SecretAccessAuditRow struct {
	id        int64
	at        time.Time
	reason    obs.SecretDenialReason
	requester domain.IntegrationInstanceID
	invalid   []obs.StoredMetadataField
}

func (r SecretAccessAuditRow) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: secret audit row"))
}
func (r SecretAccessAuditRow) ID() int64             { return r.id }
func (r SecretAccessAuditRow) At() (time.Time, bool) { return r.at, r.id > 0 && len(r.invalid) == 0 }
func (r SecretAccessAuditRow) Reason() obs.SecretDenialReason {
	if r.id <= 0 || len(r.invalid) != 0 {
		return obs.SecretDenialReason_SECRET_DENIAL_REASON_UNSPECIFIED
	}
	return r.reason
}

// The next trusted server step pseudonymizes this bounded admitted ID. No raw
// requester name is a public audit attribute.
func (r SecretAccessAuditRow) Requester() (domain.IntegrationInstanceID, bool) {
	return r.requester, r.id > 0 && len(r.invalid) == 0
}
func (r SecretAccessAuditRow) MetadataInvalid() *obs.StoredMetadataInvalid {
	if len(r.invalid) == 0 {
		return nil
	}
	return &obs.StoredMetadataInvalid{Fields: append([]obs.StoredMetadataField(nil), r.invalid...)}
}

type AuthenticationAuditPage struct {
	rows        []AuthenticationAuditRow
	upper       int64
	next        AuditPosition
	more, valid bool
}

func (p AuthenticationAuditPage) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: authentication audit page"))
}
func (p AuthenticationAuditPage) Rows() []AuthenticationAuditRow {
	rows := append([]AuthenticationAuditRow(nil), p.rows...)
	for i := range rows {
		rows[i].invalid = append([]obs.StoredMetadataField(nil), rows[i].invalid...)
	}
	return rows
}
func (p AuthenticationAuditPage) Upper() (int64, error) {
	if !p.valid {
		return 0, &Error{kind: InvalidSettings}
	}
	return p.upper, nil
}
func (p AuthenticationAuditPage) Next() (AuditPosition, bool) { return p.next, p.valid && p.more }

type SecretAccessAuditPage struct {
	rows        []SecretAccessAuditRow
	upper       int64
	next        AuditPosition
	more, valid bool
}

func (p SecretAccessAuditPage) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: secret audit page"))
}
func (p SecretAccessAuditPage) Rows() []SecretAccessAuditRow {
	rows := append([]SecretAccessAuditRow(nil), p.rows...)
	for i := range rows {
		rows[i].invalid = append([]obs.StoredMetadataField(nil), rows[i].invalid...)
	}
	return rows
}
func (p SecretAccessAuditPage) Upper() (int64, error) {
	if !p.valid {
		return 0, &Error{kind: InvalidSettings}
	}
	return p.upper, nil
}
func (p SecretAccessAuditPage) Next() (AuditPosition, bool) { return p.next, p.valid && p.more }

const authenticationAuditColumns = `id,occurred_at,action,outcome,
CASE WHEN actor_account_id IS NULL THEN NULL WHEN octet_length(actor_account_id)<=128 THEN actor_account_id ELSE NULL END,
(actor_account_id IS NOT NULL AND octet_length(actor_account_id)>128),
CASE WHEN route_path IS NULL THEN NULL WHEN octet_length(route_path)<=256 THEN route_path ELSE NULL END,
(route_path IS NOT NULL AND octet_length(route_path)>256)`
const authenticationAuditFirst = `SELECT ` + authenticationAuditColumns + ` FROM public.auth_audit WHERE id<=$1 ORDER BY id DESC LIMIT $2`
const authenticationAuditAfter = `SELECT ` + authenticationAuditColumns + ` FROM public.auth_audit WHERE id<=$1 AND id<$2 ORDER BY id DESC LIMIT $3`
const secretAuditColumns = `id,recorded_at,
CASE WHEN octet_length(reason::text)<=64 THEN reason::text ELSE NULL END,
(octet_length(reason::text)>64),
CASE WHEN octet_length(requesting_instance)<=256 THEN requesting_instance ELSE NULL END,
(octet_length(requesting_instance)>256)`
const secretAuditFirst = `SELECT ` + secretAuditColumns + ` FROM public.secret_access_audit WHERE id<=$1 ORDER BY id DESC LIMIT $2`
const secretAuditAfter = `SELECT ` + secretAuditColumns + ` FROM public.secret_access_audit WHERE id<=$1 AND id<$2 ORDER BY id DESC LIMIT $3`

func auditTime(value pgtype.Timestamptz) (time.Time, bool) {
	if !value.Valid || value.InfinityModifier != pgtype.Finite || value.Time.IsZero() || timestamppb.New(value.Time).CheckValid() != nil {
		return time.Time{}, false
	}
	return value.Time.UTC(), true
}
func auditActionWire(action authAction) (obs.AuthAuditAction, bool) {
	switch action {
	case authActionBootstrap:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_BOOTSTRAP, true
	case authActionRecover:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_RECOVER, true
	case authActionCreateAccount:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_CREATE_ACCOUNT, true
	case authActionGetAccount:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_GET_ACCOUNT, true
	case authActionListAccounts:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_LIST_ACCOUNTS, true
	case authActionUpdateAccount:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_UPDATE_ACCOUNT, true
	case authActionDeleteAccount:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_DELETE_ACCOUNT, true
	case authActionCreateInvitation:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_CREATE_INVITATION, true
	case authActionAcceptInvitation:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_ACCEPT_INVITATION, true
	case authActionListInvitations:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_LIST_INVITATIONS, true
	case authActionRevokeInvitation:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_REVOKE_INVITATION, true
	case authActionTransport:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_TRANSPORT, true
	case authActionLoginDenied:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_LOGIN_DENIED, true
	case authActionLoginLimited:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_LOGIN_LIMITED, true
	case authActionChallengeIssued:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_CHALLENGE_ISSUED, true
	case authActionChallengeDenied:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_CHALLENGE_DENIED, true
	case authActionSessionCreate:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_SESSION_CREATE, true
	case authActionSessionRefresh:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_SESSION_REFRESH, true
	case authActionSessionReplay:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_SESSION_REPLAY, true
	case authActionSessionRevoke:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_SESSION_REVOKE, true
	case authActionPolicyRead:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_POLICY_READ, true
	case authActionPolicyReplace:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_POLICY_REPLACE, true
	case authActionPolicySeed:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_POLICY_SEED, true
	case authActionMFAEnroll:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_MFA_ENROLL, true
	case authActionMFARemove:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_MFA_REMOVE, true
	case authActionMFARecoveryReplace:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_MFA_RECOVERY_REPLACE, true
	case authActionMFAReset:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_MFA_RESET, true
	case authActionMFAPolicyReplace:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_MFA_POLICY_REPLACE, true
	case authActionMFAPolicyRead:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_MFA_POLICY_READ, true
	default:
		return obs.AuthAuditAction_AUTH_AUDIT_ACTION_UNSPECIFIED, false
	}
}
func auditOutcomeWire(value int16) (obs.AuditOutcome, bool) {
	switch value {
	case authOutcomeAdmitted:
		return obs.AuditOutcome_AUDIT_OUTCOME_ADMITTED, true
	case authOutcomeDenied:
		return obs.AuditOutcome_AUDIT_OUTCOME_DENIED, true
	default:
		return obs.AuditOutcome_AUDIT_OUTCOME_UNSPECIFIED, false
	}
}
func secretAuditReasonWire(value string) (obs.SecretDenialReason, bool) {
	foreign, _ := SecretDenialForeignHandle.sqlValue()
	inactive, _ := SecretDenialInactiveSession.sqlValue()
	switch value {
	case foreign:
		return obs.SecretDenialReason_SECRET_DENIAL_REASON_FOREIGN_HANDLE, true
	case inactive:
		return obs.SecretDenialReason_SECRET_DENIAL_REASON_INACTIVE_SESSION, true
	default:
		return obs.SecretDenialReason_SECRET_DENIAL_REASON_UNSPECIFIED, false
	}
}

func (s *Store) ReadAuthenticationAudits(ctx context.Context, request AuthenticationAuditRead) (AuthenticationAuditPage, error) {
	if accountContextError(ctx) != nil || s == nil || s.pool == nil || !request.valid || request.size == 0 || request.size > 100 || !request.position.valid {
		return AuthenticationAuditPage{}, &Error{kind: InvalidSettings}
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AuthenticationAuditPage{}, &Error{kind: Unavailable}
	}
	defer func() { _ = tx.Rollback(bounded) }()
	upper := request.position.upper
	if request.position.first {
		err = tx.QueryRow(bounded, `SELECT id FROM public.auth_audit ORDER BY id DESC LIMIT 1`).Scan(&upper)
		if errors.Is(err, pgx.ErrNoRows) {
			upper = 0
		} else if err != nil {
			return AuthenticationAuditPage{}, &Error{kind: Unavailable}
		}
	}
	page := AuthenticationAuditPage{upper: upper, valid: true, rows: make([]AuthenticationAuditRow, 0, request.size)}
	if upper > 0 {
		var rows pgx.Rows
		if request.position.first {
			rows, err = tx.Query(bounded, authenticationAuditFirst, upper, int(request.size)+1)
		} else {
			rows, err = tx.Query(bounded, authenticationAuditAfter, upper, request.position.before, int(request.size)+1)
		}
		if err != nil {
			return AuthenticationAuditPage{}, &Error{kind: Unavailable}
		}
		charge := uint64(unsafe.Sizeof(AuthenticationAuditPage{}))
		var last int64
		for rows.Next() {
			var row AuthenticationAuditRow
			var stamp pgtype.Timestamptz
			var action, outcome int16
			var actor, route *string
			var actorInvalid, routeInvalid bool
			if err = rows.Scan(&row.id, &stamp, &action, &outcome, &actor, &actorInvalid, &route, &routeInvalid); err != nil {
				rows.Close()
				return AuthenticationAuditPage{}, &Error{kind: Unavailable}
			}
			if row.id <= 0 || row.id > upper || last > 0 && row.id >= last {
				rows.Close()
				return AuthenticationAuditPage{}, &Error{kind: Unavailable}
			}
			last = row.id
			if len(page.rows) == int(request.size) {
				page.more = true
				break
			}
			var valid bool
			row.at, valid = auditTime(stamp)
			if !valid {
				row.invalid = append(row.invalid, obs.StoredMetadataField_STORED_METADATA_FIELD_OCCURRED_AT)
			}
			row.action, valid = auditActionWire(authAction(action))
			if !valid {
				row.invalid = append(row.invalid, obs.StoredMetadataField_STORED_METADATA_FIELD_ACTION)
			}
			row.outcome, valid = auditOutcomeWire(outcome)
			if !valid {
				row.invalid = append(row.invalid, obs.StoredMetadataField_STORED_METADATA_FIELD_OUTCOME)
			}
			if actor != nil {
				if len(*actor) > 128 || !utf8.ValidString(*actor) || strings.ContainsAny(*actor, "\x00\r\n") {
					actorInvalid = true
				} else if id, e := contracts.ParseAccountID(*actor); e != nil {
					actorInvalid = true
				} else {
					row.actor = id
					row.hasActor = true
				}
			}
			if actorInvalid {
				row.invalid = append(row.invalid, obs.StoredMetadataField_STORED_METADATA_FIELD_ACTOR)
			}
			if route != nil {
				if len(*route) > 256 || !utf8.ValidString(*route) {
					routeInvalid = true
				} else {
					row.route = *route
					row.hasRoute = true
				}
			}
			if routeInvalid {
				row.invalid = append(row.invalid, obs.StoredMetadataField_STORED_METADATA_FIELD_ROUTE)
			}
			rowCharge := uint64(unsafe.Sizeof(AuthenticationAuditRow{})) + uint64(len(row.route)) + uint64(len(row.invalid))*4 + 128
			if rowCharge > 256<<10-charge {
				page.more = true
				break
			}
			charge += rowCharge
			page.rows = append(page.rows, row)
		}
		rows.Close()
		if rows.Err() != nil {
			return AuthenticationAuditPage{}, &Error{kind: Unavailable}
		}
	}
	if page.more {
		if len(page.rows) == 0 {
			return AuthenticationAuditPage{}, &Error{kind: Unavailable}
		}
		page.next, _ = AuditPositionAfter(upper, page.rows[len(page.rows)-1].id)
	}
	if err = tx.Commit(bounded); err != nil {
		return AuthenticationAuditPage{}, &Error{kind: Unavailable}
	}
	return page, nil
}

func (s *Store) ReadSecretAccessAudits(ctx context.Context, request SecretAccessAuditRead) (SecretAccessAuditPage, error) {
	if accountContextError(ctx) != nil || s == nil || s.pool == nil || !request.valid || request.size == 0 || request.size > 100 || !request.position.valid {
		return SecretAccessAuditPage{}, &Error{kind: InvalidSettings}
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SecretAccessAuditPage{}, &Error{kind: Unavailable}
	}
	defer func() { _ = tx.Rollback(bounded) }()
	upper := request.position.upper
	if request.position.first {
		err = tx.QueryRow(bounded, `SELECT id FROM public.secret_access_audit ORDER BY id DESC LIMIT 1`).Scan(&upper)
		if errors.Is(err, pgx.ErrNoRows) {
			upper = 0
		} else if err != nil {
			return SecretAccessAuditPage{}, &Error{kind: Unavailable}
		}
	}
	page := SecretAccessAuditPage{upper: upper, valid: true, rows: make([]SecretAccessAuditRow, 0, request.size)}
	if upper > 0 {
		var rows pgx.Rows
		if request.position.first {
			rows, err = tx.Query(bounded, secretAuditFirst, upper, int(request.size)+1)
		} else {
			rows, err = tx.Query(bounded, secretAuditAfter, upper, request.position.before, int(request.size)+1)
		}
		if err != nil {
			return SecretAccessAuditPage{}, &Error{kind: Unavailable}
		}
		charge := uint64(unsafe.Sizeof(SecretAccessAuditPage{}))
		var last int64
		for rows.Next() {
			var row SecretAccessAuditRow
			var stamp pgtype.Timestamptz
			var reason, requester *string
			var reasonInvalid, requesterInvalid bool
			if err = rows.Scan(&row.id, &stamp, &reason, &reasonInvalid, &requester, &requesterInvalid); err != nil {
				rows.Close()
				return SecretAccessAuditPage{}, &Error{kind: Unavailable}
			}
			if row.id <= 0 || row.id > upper || last > 0 && row.id >= last {
				rows.Close()
				return SecretAccessAuditPage{}, &Error{kind: Unavailable}
			}
			last = row.id
			if len(page.rows) == int(request.size) {
				page.more = true
				break
			}
			var valid bool
			row.at, valid = auditTime(stamp)
			if !valid {
				row.invalid = append(row.invalid, obs.StoredMetadataField_STORED_METADATA_FIELD_OCCURRED_AT)
			}
			if reason == nil {
				reasonInvalid = true
			} else {
				row.reason, valid = secretAuditReasonWire(*reason)
				reasonInvalid = reasonInvalid || !valid
			}
			if reasonInvalid {
				row.invalid = append(row.invalid, obs.StoredMetadataField_STORED_METADATA_FIELD_ACTION)
			}
			if requester == nil {
				requesterInvalid = true
			} else if len(*requester) > 256 || !utf8.ValidString(*requester) {
				requesterInvalid = true
			} else if id, e := domain.ParseIntegrationInstanceID(*requester); e != nil {
				requesterInvalid = true
			} else {
				row.requester = id
			}
			if requesterInvalid {
				row.invalid = append(row.invalid, obs.StoredMetadataField_STORED_METADATA_FIELD_REQUESTING_INSTANCE)
			}
			rowCharge := uint64(unsafe.Sizeof(SecretAccessAuditRow{})) + 256 + uint64(len(row.invalid))*4
			if rowCharge > 256<<10-charge {
				page.more = true
				break
			}
			charge += rowCharge
			page.rows = append(page.rows, row)
		}
		rows.Close()
		if rows.Err() != nil {
			return SecretAccessAuditPage{}, &Error{kind: Unavailable}
		}
	}
	if page.more {
		if len(page.rows) == 0 {
			return SecretAccessAuditPage{}, &Error{kind: Unavailable}
		}
		page.next, _ = AuditPositionAfter(upper, page.rows[len(page.rows)-1].id)
	}
	if err = tx.Commit(bounded); err != nil {
		return SecretAccessAuditPage{}, &Error{kind: Unavailable}
	}
	return page, nil
}
