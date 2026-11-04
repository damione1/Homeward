package cli

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	nativeauth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/operator/v1/operatorv1connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"golang.org/x/term"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Terminal is injectable for argv tests. SystemTerminal never reads passwords
// from stdin pipes, environment variables, flags or positional arguments.
type Terminal interface {
	ReadPassword(prompt string) ([]byte, error)
}
type SystemTerminal struct{}

func (SystemTerminal) ReadPassword(prompt string) ([]byte, error) {
	file, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fail("interactive_terminal_required", ExitUsage)
	}
	defer file.Close()
	fd := int(file.Fd())
	if !term.IsTerminal(fd) {
		return nil, fail("interactive_terminal_required", ExitUsage)
	}
	if _, err := io.WriteString(file, prompt); err != nil {
		return nil, fail("terminal_unavailable", ExitLocal)
	}
	password, err := term.ReadPassword(fd)
	_, _ = fmt.Fprintln(file)
	if err != nil {
		clear(password)
		return nil, fail("terminal_unavailable", ExitLocal)
	}
	return password, nil
}

func (v *invocation) password(prompt string, confirm bool) (string, error) {
	value, err := v.terminal.ReadPassword(prompt)
	if err != nil {
		return "", fail("interactive_password_unavailable", ExitUsage)
	}
	defer clear(value)
	if len(value) == 0 || len(value) > 1024 {
		return "", fail("invalid_password_length", ExitUsage)
	}
	if confirm {
		second, err := v.terminal.ReadPassword("Confirm password: ")
		if err != nil {
			clear(second)
			return "", fail("interactive_password_unavailable", ExitUsage)
		}
		defer clear(second)
		if string(value) != string(second) {
			return "", fail("password_confirmation_mismatch", ExitUsage)
		}
	}
	return string(value), nil
}

type credentials struct {
	Origin     string    `json:"origin"`
	Session    string    `json:"session"`
	Account    string    `json:"account"`
	Access     string    `json:"access_token"`
	Refresh    string    `json:"refresh_token"`
	Expires    time.Time `json:"access_expires_at"`
	RenewalKey []byte    `json:"renewal_key,omitempty"`
}

func (credentials) String() string   { return "[REDACTED CREDENTIALS]" }
func (credentials) GoString() string { return "[REDACTED CREDENTIALS]" }

func admitTokens(origin string, tokens *auth.SessionTokens) (credentials, error) {
	if contracts.AdmitAPISessionTokens(tokens, time.Now()) != nil {
		return credentials{}, fail("invalid_session_response", ExitAuthentication)
	}

	session, err := apitypes.ParseSessionName(tokens.Session.Name)
	if err != nil {
		return credentials{}, fail("invalid_session_response", ExitAuthentication)
	}
	return credentials{Origin: origin, Session: session.String(), Account: session.Account().String(), Access: tokens.AccessToken, Refresh: tokens.RefreshToken, Expires: tokens.AccessExpireTime.AsTime()}, nil
}

type remote struct {
	client     *http.Client
	base       string
	auth       apiv2connect.AuthServiceClient
	accounts   apiv2connect.AccountServiceClient
	credential credentials
	name       profileName
}

func (r *remote) authHome() string {
	name, err := apitypes.ParseAccountName(r.credential.Account)
	if err != nil {
		return ""
	}
	return name.Home().String()
}

func (r *remote) close() { r.client.CloseIdleConnections() }
func bearer[T any](value *T, token string) *connect.Request[T] {
	r := connect.NewRequest(value)
	r.Header().Set("Authorization", "Bearer "+token)
	return r
}
func (v *invocation) authenticated() (*remote, error) {
	name, p, err := v.selected()
	if err != nil {
		return nil, err
	}
	if p.Kind != endpointHTTPS {
		return nil, fail("remote_command_requires_https_profile", ExitUsage)
	}
	var credential credentials
	if err := v.dir.read(name.value+".credentials.json", &credential); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fail("login_required", ExitAuthentication)
		}
		return nil, err
	}
	if credential.Origin != p.URL || credential.Session == "" || credential.Account == "" || credential.Access == "" || credential.Refresh == "" || credential.Expires.IsZero() {
		return nil, fail("invalid_profile_credentials", ExitAuthentication)
	}
	accountName, accountErr := apitypes.ParseAccountName(credential.Account)
	sessionName, sessionErr := apitypes.ParseSessionName(credential.Session)
	if accountErr != nil || sessionErr != nil || sessionName.Account() != accountName {
		return nil, fail("invalid_profile_credentials", ExitAuthentication)
	}
	client, base, err := p.client()
	if err != nil {
		return nil, err
	}
	r := &remote{client: client, base: base, auth: apiv2connect.NewAuthServiceClient(client, base), accounts: apiv2connect.NewAccountServiceClient(client, base), credential: credential, name: name}
	if len(credential.RenewalKey) != 0 || !credential.Expires.After(time.Now().Add(30*time.Second)) {
		if err := v.refresh(r); err != nil {
			r.close()
			return nil, err
		}
	}
	return r, nil
}

// Only an admitted canonical credential refusal at the renewal boundary
// retires the session. Bare proxy statuses preserve the saved renewal operation.
func (v *invocation) refresh(r *remote) error {
	if len(r.credential.RenewalKey) == 0 {
		r.credential.RenewalKey = make([]byte, 32)
		if _, err := rand.Read(r.credential.RenewalKey); err != nil {
			return fail("renewal_entropy_unavailable", ExitLocal)
		}
		if err := v.dir.write(r.name.value+".credentials.json", r.credential, true); err != nil {
			return err
		}
	}
	if len(r.credential.RenewalKey) != 32 {
		return fail("invalid_pending_renewal", ExitLocal)
	}
	response, err := r.auth.RefreshSession(v.ctx, connect.NewRequest(&auth.RefreshSessionRequest{RefreshToken: r.credential.Refresh, RenewalKey: r.credential.RenewalKey}))
	if err != nil {
		var remote *connect.Error
		definitive := false
		if errors.As(err, &remote) {
			fault, known := admitRemoteAPIFailure(remote)
			definitive = known && (fault.Reason() == auth.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED || fault.Reason() == auth.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
		}
		if definitive {
			if removeErr := v.dir.remove(r.name.value + ".credentials.json"); removeErr != nil {
				return removeErr
			}
			return fail("session_invalid_local_credentials_forgotten_relogin_required", ExitAuthentication)
		}
		return err
	}
	updated, err := admitTokens(r.base, response.Msg.Tokens)
	if err != nil {
		return err
	}
	if updated.Session != r.credential.Session || updated.Account != r.credential.Account {
		return fail("refreshed_session_binding_changed", ExitAuthentication)
	}
	if err := v.dir.write(r.name.value+".credentials.json", updated, true); err != nil {
		return err
	}
	r.credential = updated
	return nil
}

type loginCommand struct {
	Login string `arg:""`
}

func (c *loginCommand) Run(v *invocation) error {
	name, p, err := v.selected()
	if err != nil {
		return err
	}
	if p.Kind != endpointHTTPS {
		return fail("login_requires_https_profile", ExitUsage)
	}
	var existing credentials
	if err := v.dir.read(name.value+".credentials.json", &existing); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fail("logout_existing_session_before_login", ExitConflict)
	}
	client, base, err := p.client()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	password, err := v.password("Password: ", false)
	if err != nil {
		return err
	}
	request := &auth.StartLoginRequest{LoginName: c.Login, Password: password}
	response, err := apiv2connect.NewAuthServiceClient(client, base).StartLogin(v.ctx, connect.NewRequest(request))
	request.Password = ""
	password = ""
	if err != nil {
		return err
	}
	decision := response.Msg.Decision
	if decision == nil {
		return fail("invalid_login_outcome", ExitAuthentication)
	}
	switch outcome := decision.Outcome.(type) {
	case *auth.LoginDecision_Authenticated:
		credential, err := admitTokens(p.URL, outcome.Authenticated)
		if err != nil {
			return err
		}
		if err := v.dir.write(name.value+".credentials.json", credential, false); err != nil {
			// Do not leave an issued session live after failed persistence.
			_, _ = apiv2connect.NewAuthServiceClient(client, base).RevokeSession(v.ctx, bearer(&auth.RevokeSessionRequest{Name: credential.Session}, credential.Access))
			return err
		}
		return v.emit(outcome.Authenticated.Session)
	case *auth.LoginDecision_Challenge:
		return fail("factor_challenge_requires_NHA144_client", ExitChallenge)
	case *auth.LoginDecision_EnrollmentRequired:
		return fail("factor_enrollment_requires_NHA144_client", ExitChallenge)
	default:
		return fail("invalid_login_outcome", ExitAuthentication)
	}
}

type logoutCommand struct {
	ForgetLocal bool `help:"Explicitly forget local credentials without contacting the server; does not revoke the remote session."`
}

func (c *logoutCommand) Run(v *invocation) error {
	if c.ForgetLocal {
		name, _, err := v.selected()
		if err != nil {
			return err
		}
		if err := v.dir.remove(name.value + ".credentials.json"); err != nil {
			return err
		}
		return v.emitJSON(struct {
			LocalCredentialsForgotten bool `json:"local_credentials_forgotten"`
			RemoteRevocationConfirmed bool `json:"remote_revocation_confirmed"`
		}{true, false})
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	revoke := func() error {
		_, err := r.auth.RevokeSession(v.ctx, bearer(&auth.RevokeSessionRequest{Name: r.credential.Session}, r.credential.Access))
		return err
	}
	err = revoke()
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		if err := v.refresh(r); err != nil {
			return err
		}
		err = revoke()
	}
	if err != nil {
		return err
	}
	if err := v.dir.remove(r.name.value + ".credentials.json"); err != nil {
		return err
	}
	return v.emitJSON(struct {
		Revoked bool `json:"revoked"`
	}{true})
}

type sessionsList struct {
	PageSize  int32  `help:"Requested page size; zero selects the default."`
	PageToken string `help:"Opaque continuation from the same listing."`
	Account   string `arg:"" optional:"" help:"Canonical account name; omitted means the signed-in account."`
}

func (c *sessionsList) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	account := c.Account
	if account == "" {
		account = r.credential.Account
	}
	response, err := r.auth.ListSessions(v.ctx, bearer(&auth.ListSessionsRequest{Parent: account, PageSize: c.PageSize, PageToken: c.PageToken}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type sessionsRevoke struct {
	ID string `arg:""`
}

func (c *sessionsRevoke) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.auth.RevokeSession(v.ctx, bearer(&auth.RevokeSessionRequest{Name: c.ID}, r.credential.Access))
	if err != nil {
		return err
	}
	if c.ID == r.credential.Session {
		if err := v.dir.remove(r.name.value + ".credentials.json"); err != nil {
			return err
		}
	}
	return v.emit(response.Msg)
}

type accountsList struct {
	PageSize  int32  `help:"Requested page size; zero selects the default."`
	PageToken string `help:"Opaque continuation from the same listing."`
}

func (c *accountsList) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.accounts.ListAccounts(v.ctx, bearer(&auth.ListAccountsRequest{Parent: r.authHome(), PageSize: c.PageSize, PageToken: c.PageToken}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type accountsGet struct {
	ID string `arg:""`
}

func (c *accountsGet) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := r.accounts.GetAccount(v.ctx, bearer(&auth.GetAccountRequest{Name: c.ID}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type accountRole string

func (r accountRole) wire() (auth.AccountRole, error) {
	switch r {
	case "admin":
		return auth.AccountRole_ACCOUNT_ROLE_ADMIN, nil
	case "user":
		return auth.AccountRole_ACCOUNT_ROLE_USER, nil
	default:
		return 0, fail("invalid_account_role", ExitUsage)
	}
}

type accountsCreate struct {
	Login       string      `arg:""`
	DisplayName string      `required:""`
	Role        accountRole `enum:"admin,user" required:""`
}

func (c *accountsCreate) Run(v *invocation) error {
	role, err := c.Role.wire()
	if err != nil {
		return err
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	password, err := v.password("Initial password: ", true)
	if err != nil {
		return err
	}
	request := &auth.CreateAccountRequest{Parent: r.authHome(), Account: &auth.Account{LoginName: c.Login, DisplayName: c.DisplayName, Role: role, InitialPassword: password}}
	response, err := r.accounts.CreateAccount(v.ctx, bearer(request, r.credential.Access))
	request.Account.InitialPassword, password = "", ""
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type accountsInvite struct {
	Login     string      `arg:""`
	Role      accountRole `enum:"admin,user" required:""`
	Expires   string      `required:"" help:"RFC3339 expiration timestamp."`
	SaveToken string      `required:"" help:"Unique safe file label; token is written under the private profile directory, never stdout."`
}

func (c *accountsInvite) Run(v *invocation) error {
	role, err := c.Role.wire()
	if err != nil {
		return err
	}
	expires, err := time.Parse(time.RFC3339, c.Expires)
	if err != nil || !expires.After(time.Now()) {
		return fail("invalid_invitation_expiration", ExitUsage)
	}
	label, err := parseProfileName(c.SaveToken)
	if err != nil {
		return fail("invalid_token_file_label", ExitUsage)
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	filename := r.name.value + ".invitation-" + label.value + ".json"
	var existing any
	if err := v.dir.read(filename, &existing); !errors.Is(err, os.ErrNotExist) {
		return fail("token_file_already_exists_or_unsafe", ExitConflict)
	}
	response, err := r.accounts.IssueInvitation(v.ctx, bearer(&auth.IssueInvitationRequest{Parent: r.authHome(), LoginName: c.Login, Role: role, ExpireTime: timestamppb.New(expires)}, r.credential.Access))
	if err != nil {
		return err
	}
	if response.Msg.Invitation == nil || response.Msg.Invitation.Name == "" || response.Msg.InvitationToken == "" {
		return fail("invalid_invitation_response", ExitLocal)
	}
	if err := v.dir.write(filename, struct {
		Token string `json:"invitation_token"`
	}{response.Msg.InvitationToken}, false); err != nil {
		_, _ = r.accounts.RevokeInvitation(v.ctx, bearer(&auth.RevokeInvitationRequest{Name: response.Msg.Invitation.Name}, r.credential.Access))
		return err
	}
	return v.emitJSON(struct {
		Invitation jsonMessage `json:"invitation"`
		TokenFile  string      `json:"token_file"`
	}{jsonMessage{response.Msg.Invitation}, filename})
}

type bootstrapCommand struct {
	Login       string `arg:""`
	DisplayName string `required:""`
}
type recoverCommand struct {
	Account string `arg:""`
}

func (v *invocation) operator() (*http.Client, operatorv1connect.OperatorAccountServiceClient, error) {
	_, p, err := v.selected()
	if err != nil {
		return nil, nil, err
	}
	if p.Kind != endpointUnix {
		return nil, nil, fail("local_operator_socket_required_remote_profile_refused", ExitPermission)
	}
	client, base, err := p.client()
	if err != nil {
		return nil, nil, err
	}
	return client, operatorv1connect.NewOperatorAccountServiceClient(client, base), nil
}
func (c *bootstrapCommand) Run(v *invocation) error {
	client, operator, err := v.operator()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	password, err := v.password("Admin password: ", true)
	if err != nil {
		return err
	}
	request := &nativeauth.BootstrapAdminRequest{LoginName: c.Login, DisplayName: c.DisplayName, Password: password}
	response, err := operator.BootstrapAdmin(v.ctx, connect.NewRequest(request))
	request.Password, password = "", ""
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}
func (c *recoverCommand) Run(v *invocation) error {
	client, operator, err := v.operator()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	password, err := v.password("Replacement password: ", true)
	if err != nil {
		return err
	}
	request := &nativeauth.RecoverAccountRequest{AccountId: &nativeauth.AccountId{Value: c.Account}, ReplacementPassword: password}
	response, err := operator.RecoverAccount(v.ctx, connect.NewRequest(request))
	request.ReplacementPassword, password = "", ""
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}
