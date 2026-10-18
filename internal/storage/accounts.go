package storage

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrAccountInvalid = errors.New("accounts: invalid input")
var ErrAccountUnavailable = errors.New("accounts: storage unavailable")
var ErrAccountCommitUnknown = errors.New("accounts: commit outcome unknown")
var ErrAccountConflict = errors.New("accounts: account or login conflict")
var ErrAccountNotFound = errors.New("accounts: account not found")
var ErrAccountPermission = errors.New("accounts: administrator required")
var ErrBootstrapCompleted = errors.New("accounts: bootstrap already completed")
var ErrLastActiveAdmin = errors.New("accounts: last active administrator protected")
var ErrInvitationNotFound = errors.New("accounts: invitation not found")
var ErrInvitationExpired = errors.New("accounts: invitation expired")
var ErrInvitationUnavailable = errors.New("accounts: invitation already used or revoked")
var ErrAccountListTooLarge = errors.New("accounts: account listing exceeds bounded response")

const sqlRoleAdmin int16 = 1
const sqlRoleUser int16 = 2
const sqlStatusActive int16 = 1
const sqlStatusDisabled int16 = 2

type AccountRecord struct {
	id                  contracts.AccountID
	login               accounts.LoginName
	display             accounts.DisplayName
	role                authv1.AccountRole
	status              authv1.AccountStatus
	secondFactorEnabled bool
	createdAt           time.Time
	updatedAt           time.Time
	verifier            accounts.Verifier
	revision            RevocationRevision
}

func (r AccountRecord) valid() bool {
	if _, err := r.id.Value(); err != nil {
		return false
	}
	if _, _, err := r.login.Values(); err != nil {
		return false
	}
	if _, err := r.display.Value(); err != nil {
		return false
	}
	if _, err := sqlRole(r.role); err != nil {
		return false
	}
	if _, err := sqlStatus(r.status); err != nil {
		return false
	}
	if _, _, _, err := r.verifier.StorageValues(); err != nil {
		return false
	}
	return r.revision.validFor(r.id) && !r.createdAt.IsZero() && !r.updatedAt.Before(r.createdAt)
}

func (r AccountRecord) ID() (contracts.AccountID, error) {
	if !r.valid() {
		return contracts.AccountID{}, ErrAccountUnavailable
	}
	return r.id, nil
}
func (r AccountRecord) Login() (accounts.LoginName, error) {
	if !r.valid() {
		return accounts.LoginName{}, ErrAccountUnavailable
	}
	return r.login, nil
}
func (r AccountRecord) Display() (accounts.DisplayName, error) {
	if !r.valid() {
		return accounts.DisplayName{}, ErrAccountUnavailable
	}
	return r.display, nil
}
func (r AccountRecord) Role() (authv1.AccountRole, error) {
	if !r.valid() {
		return 0, ErrAccountUnavailable
	}
	return r.role, nil
}
func (r AccountRecord) Status() (authv1.AccountStatus, error) {
	if !r.valid() {
		return 0, ErrAccountUnavailable
	}
	return r.status, nil
}
func (r AccountRecord) SecondFactorEnabled() (bool, error) {
	if !r.valid() {
		return false, ErrAccountUnavailable
	}
	return r.secondFactorEnabled, nil
}
func (r AccountRecord) CreatedAt() (time.Time, error) {
	if !r.valid() {
		return time.Time{}, ErrAccountUnavailable
	}
	return r.createdAt, nil
}
func (r AccountRecord) UpdatedAt() (time.Time, error) {
	if !r.valid() {
		return time.Time{}, ErrAccountUnavailable
	}
	return r.updatedAt, nil
}

func (r AccountRecord) Verifier() (accounts.Verifier, error) {
	if !r.valid() {
		return accounts.Verifier{}, ErrAccountUnavailable
	}
	return r.verifier, nil
}

func (r AccountRecord) Revision() (RevocationRevision, error) {
	if !r.valid() {
		return RevocationRevision{}, ErrAccountUnavailable
	}
	return r.revision, nil
}

type InvitationRecord struct {
	id         contracts.InvitationID
	login      accounts.LoginName
	role       authv1.AccountRole
	expiresAt  time.Time
	createdAt  time.Time
	acceptedAt *time.Time
	revokedAt  *time.Time
}

func (r InvitationRecord) valid() bool {
	if _, err := r.id.Value(); err != nil {
		return false
	}
	if _, _, err := r.login.Values(); err != nil {
		return false
	}
	if _, err := sqlRole(r.role); err != nil {
		return false
	}
	if r.createdAt.IsZero() || !r.expiresAt.After(r.createdAt) || (r.acceptedAt != nil && r.revokedAt != nil) {
		return false
	}
	if r.acceptedAt != nil && r.acceptedAt.Before(r.createdAt) {
		return false
	}
	if r.revokedAt != nil && r.revokedAt.Before(r.createdAt) {
		return false
	}
	return true
}

func (r InvitationRecord) ID() (contracts.InvitationID, error) {
	if !r.valid() {
		return contracts.InvitationID{}, ErrAccountUnavailable
	}
	return r.id, nil
}
func (r InvitationRecord) Login() (accounts.LoginName, error) {
	if !r.valid() {
		return accounts.LoginName{}, ErrAccountUnavailable
	}
	return r.login, nil
}
func (r InvitationRecord) Role() (authv1.AccountRole, error) {
	if !r.valid() {
		return 0, ErrAccountUnavailable
	}
	return r.role, nil
}
func (r InvitationRecord) ExpiresAt() (time.Time, error) {
	if !r.valid() {
		return time.Time{}, ErrAccountUnavailable
	}
	return r.expiresAt, nil
}
func (r InvitationRecord) CreatedAt() (time.Time, error) {
	if !r.valid() {
		return time.Time{}, ErrAccountUnavailable
	}
	return r.createdAt, nil
}
func (r InvitationRecord) AcceptedAt() (*time.Time, error) {
	if !r.valid() {
		return nil, ErrAccountUnavailable
	}
	if r.acceptedAt == nil {
		return nil, nil
	}
	copy := *r.acceptedAt
	return &copy, nil
}
func (r InvitationRecord) RevokedAt() (*time.Time, error) {
	if !r.valid() {
		return nil, ErrAccountUnavailable
	}
	if r.revokedAt == nil {
		return nil, nil
	}
	copy := *r.revokedAt
	return &copy, nil
}

// RevocationRevision is bound to one account. Session verification in NHA-141
// can capture it and fail closed if it ceases to be current.
type RevocationRevision struct {
	account contracts.AccountID
	number  int64
}

func (r RevocationRevision) validFor(id contracts.AccountID) bool {
	value, err := id.Value()
	if err != nil || r.number <= 0 {
		return false
	}
	other, err := r.account.Value()
	return err == nil && other == value
}

func (r RevocationRevision) NewerThan(previous RevocationRevision) bool {
	return r.validFor(previous.account) && previous.validFor(r.account) && r.number > previous.number
}

type NewAccount struct {
	ID       contracts.AccountID
	Login    accounts.LoginName
	Display  accounts.DisplayName
	Role     authv1.AccountRole
	Verifier accounts.Verifier
}

type NewInvitation struct {
	ID        contracts.InvitationID
	Login     accounts.LoginName
	Role      authv1.AccountRole
	Digest    [32]byte
	ExpiresAt time.Time
}

type AccountChanges struct {
	Display *accounts.DisplayName
	Role    *authv1.AccountRole
	Status  *authv1.AccountStatus
}

type authAction int16

const (
	authActionBootstrap authAction = iota + 1
	authActionRecover
	authActionCreateAccount
	authActionGetAccount
	authActionListAccounts
	authActionUpdateAccount
	authActionDeleteAccount
	authActionCreateInvitation
	authActionAcceptInvitation
	authActionListInvitations
	authActionRevokeInvitation
	authActionTransport
	authActionLoginDenied
	authActionLoginLimited
	authActionChallengeIssued
	authActionChallengeDenied
	authActionSessionCreate
	authActionSessionRefresh
	authActionSessionReplay
	authActionSessionRevoke
)

const authOutcomeAdmitted int16 = 1
const authOutcomeDenied int16 = 2

type accountAudit struct {
	actor      *contracts.AccountID
	target     *contracts.AccountID
	invitation *contracts.InvitationID
	action     authAction
}

func sqlAccountID(id contracts.AccountID) (string, error) {
	value, err := id.Value()
	if err != nil || len(value) > 128 {
		return "", ErrAccountInvalid
	}
	return value, nil
}

func sqlInvitationID(id contracts.InvitationID) (string, error) {
	value, err := id.Value()
	if err != nil || len(value) > 128 {
		return "", ErrAccountInvalid
	}
	return value, nil
}

func sqlRole(role authv1.AccountRole) (int16, error) {
	switch role {
	case authv1.AccountRole_ACCOUNT_ROLE_ADMIN:
		return sqlRoleAdmin, nil
	case authv1.AccountRole_ACCOUNT_ROLE_USER:
		return sqlRoleUser, nil
	default:
		return 0, ErrAccountInvalid
	}
}

func sqlStatus(status authv1.AccountStatus) (int16, error) {
	switch status {
	case authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE:
		return sqlStatusActive, nil
	case authv1.AccountStatus_ACCOUNT_STATUS_DISABLED:
		return sqlStatusDisabled, nil
	default:
		return 0, ErrAccountInvalid
	}
}

func admittedSQLRole(value int16) (authv1.AccountRole, error) {
	switch value {
	case sqlRoleAdmin:
		return authv1.AccountRole_ACCOUNT_ROLE_ADMIN, nil
	case sqlRoleUser:
		return authv1.AccountRole_ACCOUNT_ROLE_USER, nil
	default:
		return 0, ErrAccountUnavailable
	}
}

func admittedSQLStatus(value int16) (authv1.AccountStatus, error) {
	switch value {
	case sqlStatusActive:
		return authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, nil
	case sqlStatusDisabled:
		return authv1.AccountStatus_ACCOUNT_STATUS_DISABLED, nil
	default:
		return 0, ErrAccountUnavailable
	}
}

func nullableAccountID(id *contracts.AccountID) (any, error) {
	if id == nil {
		return nil, nil
	}
	return sqlAccountID(*id)
}

func nullableInvitationID(id *contracts.InvitationID) (any, error) {
	if id == nil {
		return nil, nil
	}
	return sqlInvitationID(*id)
}

func insertAccountAudit(ctx context.Context, tx pgx.Tx, event accountAudit, outcome int16) error {
	actor, err := nullableAccountID(event.actor)
	if err != nil {
		return err
	}
	target, err := nullableAccountID(event.target)
	if err != nil {
		return err
	}
	invitation, err := nullableInvitationID(event.invitation)
	if err != nil {
		return err
	}
	if event.action < authActionBootstrap || event.action > authActionSessionRevoke || (outcome != authOutcomeAdmitted && outcome != authOutcomeDenied) {
		return ErrAccountInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.auth_audit(actor_account_id, target_account_id, invitation_id, action, outcome)
        VALUES ($1,$2,$3,$4,$5)`, actor, target, invitation, int16(event.action), outcome)
	return err
}

// accountTransaction commits an audit even for a business denial. SQL failures
// roll back the whole operation. An indeterminate commit is never replayed.
func accountTransaction[T any](s *Store, ctx context.Context, event accountAudit, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	if err := accountContextError(ctx); err != nil {
		return zero, err
	}
	if s == nil || s.pool == nil || fn == nil {
		return zero, ErrAccountUnavailable
	}
	var value T
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SAVEPOINT auth_action`); err != nil {
			return err
		}
		value, decision = fn(tx)
		auditOutcome := authOutcomeAdmitted
		if decision != nil {
			auditOutcome = authOutcomeDenied
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT auth_action`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT auth_action`); err != nil {
			return err
		}
		return insertAccountAudit(ctx, tx, event, auditOutcome)
	})
	if outcome == TransactionIndeterminate {
		return zero, ErrAccountCommitUnknown
	}
	if err != nil || outcome != TransactionCommitted {
		return zero, ErrAccountUnavailable
	}
	if decision != nil {
		return zero, classifyAccountError(decision)
	}
	return value, nil
}

// accountContextError admits context interfaces before they reach pgx. A typed
// nil pointer still makes the interface itself non-nil and can panic in pgx.
func accountContextError(ctx context.Context) error {
	if ctx == nil {
		return ErrAccountInvalid
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return ErrAccountInvalid
		}
	}
	return ctx.Err()
}

func classifyAccountError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrAccountConflict
	}
	for _, known := range []error{ErrAccountInvalid, ErrAccountConflict, ErrAccountNotFound, ErrAccountPermission,
		ErrBootstrapCompleted, ErrLastActiveAdmin, ErrInvitationNotFound, ErrInvitationExpired, ErrInvitationUnavailable, ErrAccountListTooLarge} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrAccountUnavailable
}

func lockAuthSingleton(ctx context.Context, tx pgx.Tx) (bool, error) {
	var completed bool
	if err := tx.QueryRow(ctx, `SELECT completed FROM public.auth_bootstrap WHERE singleton = true FOR UPDATE`).Scan(&completed); err != nil {
		return false, err
	}
	return completed, nil
}

func requireAdmin(ctx context.Context, tx pgx.Tx, actor contracts.AccountID) error {
	id, err := sqlAccountID(actor)
	if err != nil {
		return ErrAccountPermission
	}
	var role, status int16
	err = tx.QueryRow(ctx, `SELECT role, status FROM public.auth_accounts WHERE id=$1`, id).Scan(&role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAccountPermission
	}
	if err != nil {
		return err
	}
	if role != sqlRoleAdmin || status != sqlStatusActive {
		return ErrAccountPermission
	}
	return nil
}

func activeAdminCount(ctx context.Context, tx pgx.Tx) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM public.auth_accounts WHERE role=$1 AND status=$2`,
		sqlRoleAdmin, sqlStatusActive).Scan(&count)
	return count, err
}

const accountColumns = `id,login_name,login_key,display_name,role,status,password_profile,password_salt,password_digest,revocation_revision,second_factor_enabled,created_at,updated_at`

func scanAccount(row pgx.Row) (AccountRecord, error) {
	var id, loginName, loginKey, displayName string
	var role, status, profile int16
	var salt, digest []byte
	var revision int64
	var secondFactor bool
	var created, updated time.Time
	if err := row.Scan(&id, &loginName, &loginKey, &displayName, &role, &status, &profile, &salt, &digest, &revision, &secondFactor, &created, &updated); err != nil {
		return AccountRecord{}, err
	}
	accountID, err := contracts.ParseAccountID(id)
	if err != nil {
		return AccountRecord{}, ErrAccountUnavailable
	}
	login, err := accounts.ParseLoginName(loginName)
	if err != nil {
		return AccountRecord{}, ErrAccountUnavailable
	}
	_, key, err := login.Values()
	if err != nil || key != loginKey {
		return AccountRecord{}, ErrAccountUnavailable
	}
	display, err := accounts.ParseDisplayName(displayName)
	if err != nil {
		return AccountRecord{}, ErrAccountUnavailable
	}
	admittedRole, err := admittedSQLRole(role)
	if err != nil {
		return AccountRecord{}, err
	}
	admittedStatus, err := admittedSQLStatus(status)
	if err != nil {
		return AccountRecord{}, err
	}
	verifier, err := accounts.ParseVerifier(profile, salt, digest)
	if err != nil || revision <= 0 || updated.Before(created) {
		return AccountRecord{}, ErrAccountUnavailable
	}
	return AccountRecord{id: accountID, login: login, display: display, role: admittedRole, status: admittedStatus,
		secondFactorEnabled: secondFactor, createdAt: created, updatedAt: updated,
		verifier: verifier, revision: RevocationRevision{account: accountID, number: revision}}, nil
}

func accountByID(ctx context.Context, tx pgx.Tx, id contracts.AccountID) (AccountRecord, error) {
	value, err := sqlAccountID(id)
	if err != nil {
		return AccountRecord{}, err
	}
	account, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountColumns+` FROM public.auth_accounts WHERE id=$1`, value))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountRecord{}, ErrAccountNotFound
	}
	return account, err
}

const invitationColumns = `id,login_name,login_key,role,expires_at,created_at,accepted_at,revoked_at`

func scanInvitation(row pgx.Row) (InvitationRecord, error) {
	var id, loginName, loginKey string
	var role int16
	var expires, created time.Time
	var accepted, revoked *time.Time
	if err := row.Scan(&id, &loginName, &loginKey, &role, &expires, &created, &accepted, &revoked); err != nil {
		return InvitationRecord{}, err
	}
	invitationID, err := contracts.ParseInvitationID(id)
	if err != nil {
		return InvitationRecord{}, ErrAccountUnavailable
	}
	login, err := accounts.ParseLoginName(loginName)
	if err != nil {
		return InvitationRecord{}, ErrAccountUnavailable
	}
	_, key, err := login.Values()
	if err != nil || key != loginKey {
		return InvitationRecord{}, ErrAccountUnavailable
	}
	admittedRole, err := admittedSQLRole(role)
	if err != nil || !expires.After(created) || (accepted != nil && revoked != nil) {
		return InvitationRecord{}, ErrAccountUnavailable
	}
	return InvitationRecord{id: invitationID, login: login, role: admittedRole, expiresAt: expires,
		createdAt: created, acceptedAt: accepted, revokedAt: revoked}, nil
}

func accountInput(input NewAccount) (id, display, key, name string, role int16, profile int16, salt [16]byte, digest [32]byte, err error) {
	id, err = sqlAccountID(input.ID)
	if err != nil {
		return
	}
	display, key, err = input.Login.Values()
	if err != nil {
		err = ErrAccountInvalid
		return
	}
	name, err = input.Display.Value()
	if err != nil {
		err = ErrAccountInvalid
		return
	}
	role, err = sqlRole(input.Role)
	if err != nil {
		return
	}
	profile, salt, digest, err = input.Verifier.StorageValues()
	if err != nil {
		err = ErrAccountInvalid
	}
	return
}

func insertAccount(ctx context.Context, tx pgx.Tx, input NewAccount) error {
	id, _, key, _, _, _, _, _, err := accountInput(input)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.auth_login_claims(login_key,owner_kind,owner_id) VALUES($1,1,$2)`, key, id); err != nil {
		return err
	}
	return insertAccountRow(ctx, tx, input)
}

func insertAccountRow(ctx context.Context, tx pgx.Tx, input NewAccount) error {
	id, display, key, name, role, profile, salt, digest, err := accountInput(input)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.auth_accounts(id,login_name,login_key,display_name,role,status,password_profile,password_salt,password_digest)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, display, key, name, role,
		sqlStatusActive, profile, salt[:], digest[:])
	return err
}

func (s *Store) BootstrapAdmin(ctx context.Context, input NewAccount) (AccountRecord, error) {
	if input.Role != authv1.AccountRole_ACCOUNT_ROLE_ADMIN {
		return AccountRecord{}, ErrAccountInvalid
	}
	return accountTransaction(s, ctx, accountAudit{target: &input.ID, action: authActionBootstrap}, func(tx pgx.Tx) (AccountRecord, error) {
		completed, err := lockAuthSingleton(ctx, tx)
		if err != nil {
			return AccountRecord{}, err
		}
		if completed {
			return AccountRecord{}, ErrBootstrapCompleted
		}
		if err := insertAccount(ctx, tx, input); err != nil {
			return AccountRecord{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_bootstrap SET completed=true WHERE singleton=true`); err != nil {
			return AccountRecord{}, err
		}
		return accountByID(ctx, tx, input.ID)
	})
}

func (s *Store) RecoverAccount(ctx context.Context, id contracts.AccountID, verifier accounts.Verifier) (AccountRecord, error) {
	accountValue, err := sqlAccountID(id)
	if err != nil {
		return AccountRecord{}, err
	}
	profile, salt, digest, err := verifier.StorageValues()
	if err != nil {
		return AccountRecord{}, ErrAccountInvalid
	}
	return accountTransaction(s, ctx, accountAudit{target: &id, action: authActionRecover}, func(tx pgx.Tx) (AccountRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return AccountRecord{}, err
		}
		current, err := accountByID(ctx, tx, id)
		if err != nil {
			return AccountRecord{}, err
		}
		login, err := current.Login()
		if err != nil {
			return AccountRecord{}, err
		}
		command, err := tx.Exec(ctx, `UPDATE public.auth_accounts SET password_profile=$2,password_salt=$3,password_digest=$4,
            revocation_revision=revocation_revision+1,updated_at=now() WHERE id=$1`, accountValue, profile, salt[:], digest[:])
		if err != nil {
			return AccountRecord{}, err
		}
		if command.RowsAffected() != 1 {
			return AccountRecord{}, ErrAccountNotFound
		}
		if err := clearLoginAttempts(ctx, tx, login); err != nil {
			return AccountRecord{}, err
		}
		return accountByID(ctx, tx, id)
	})
}

func (s *Store) RevisionCurrent(ctx context.Context, previous RevocationRevision) (bool, error) {
	if err := accountContextError(ctx); err != nil {
		return false, err
	}
	id, err := sqlAccountID(previous.account)
	if err != nil || previous.number <= 0 {
		return false, ErrAccountInvalid
	}
	if s == nil || s.pool == nil {
		return false, ErrAccountUnavailable
	}
	var revision int64
	err = s.pool.QueryRow(ctx, `SELECT revocation_revision FROM public.auth_accounts WHERE id=$1 AND status=$2`,
		id, sqlStatusActive).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, ErrAccountUnavailable
	}
	return revision == previous.number, nil
}

func (s *Store) CredentialByLogin(ctx context.Context, login accounts.LoginName) (AccountRecord, error) {
	if err := accountContextError(ctx); err != nil {
		return AccountRecord{}, err
	}
	_, key, err := login.Values()
	if err != nil || s == nil || s.pool == nil {
		return AccountRecord{}, ErrAccountInvalid
	}
	record, err := scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM public.auth_accounts WHERE login_key=$1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountRecord{}, ErrAccountNotFound
	}
	if err != nil {
		return AccountRecord{}, ErrAccountUnavailable
	}
	return record, nil
}

func (s *Store) RecordAuthTransportAudit(ctx context.Context, actor *contracts.AccountID, route string, admitted bool) error {
	if err := accountContextError(ctx); err != nil {
		return err
	}
	if s == nil || s.pool == nil || route == "" || len(route) > 256 {
		return ErrAccountInvalid
	}
	actorValue, err := nullableAccountID(actor)
	if err != nil {
		return err
	}
	outcome := authOutcomeDenied
	if admitted {
		outcome = authOutcomeAdmitted
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO public.auth_audit(actor_account_id,action,outcome,route_path) VALUES($1,$2,$3,$4)`,
		actorValue, int16(authActionTransport), outcome, route)
	if err != nil {
		return ErrAccountUnavailable
	}
	return nil
}
