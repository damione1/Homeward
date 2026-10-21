package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/recorder"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func TestNHA141FactoryProtectedRouteCannotWeakenSessionTLS(t *testing.T) {
	settings := nha181Config(t, 1, shortPrivateDataDir(t), "127.0.0.1:0", "")
	public, err := settings.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	store := &storage.Store{} // No DB call is permitted on these plaintext paths.
	coordinator, err := newSessionCoordinator(store)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPublicPlan(nil, nil, &nha139Auditor{})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/factory-session-route"
	if err := plan.Register(RouteKindMCP, RouteMethodPost, path, RouteAccessProtected, RouteFlowUnary,
		RouteTransportOptionalTLS, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("weakened route reached handler") })); err != nil {
		t.Fatal(err)
	}
	if _, err := registerAuthRoutes(plan, store, coordinator, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := compilePublicPlan(plan, public); !errors.Is(err, ErrInvalidPublicPlan) {
		t.Fatalf("factory protected route without TLS admitted: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, path, nha181UnreadBody{t})
	request.Header.Set("Authorization", "Bearer fixture-sensitive-canary")
	request.Header.Set("Forwarded", "proto=https")
	request.Header.Set("X-Forwarded-Proto", "https")
	provider := &sessionAuth{store: store, coordinator: coordinator}
	if _, _, release, err := provider.AuthenticateWithLease(context.Background(), request); !errors.Is(err, errSessionUnauthenticated) || release != nil {
		t.Fatalf("plaintext bearer reached session admission: %v", err)
	}
}

func TestNHA141CredentialRoutesRejectH2CBeforeBody(t *testing.T) {
	settings := nha181Config(t, 1, shortPrivateDataDir(t), "127.0.0.1:0", "")
	public, err := settings.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	store := &storage.Store{} // Plaintext must stop before reaching PostgreSQL.
	coordinator, err := newSessionCoordinator(store)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPublicPlan(nil, nil, &nha139Auditor{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registerAuthRoutes(plan, store, coordinator, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := registerAccountRoutes(plan, store, coordinator); err != nil {
		t.Fatal(err)
	}
	compiled, err := compilePublicPlan(plan, public)
	if err != nil {
		t.Fatal(err)
	}
	handler := compiled.handler(public, newPublicAdmissions())
	const canary = "NHA141 plaintext credential canary"
	for _, path := range []string{
		authv1connect.AuthServiceStartLoginProcedure,
		authv1connect.AuthServiceCompleteLoginProcedure,
		authv1connect.AuthServiceRefreshSessionProcedure,
		authv1connect.AuthServiceListSessionsProcedure,
		authv1connect.AuthServiceRevokeSessionProcedure,
		authv1connect.AuthServiceBeginFactorEnrollmentProcedure,
		authv1connect.AuthServiceCompleteFactorEnrollmentProcedure,
		authv1connect.AuthServiceListFactorsProcedure,
		authv1connect.AuthServiceRemoveFactorProcedure,
		authv1connect.AuthServiceReplaceRecoveryCodesProcedure,
		authv1connect.AccountServiceGetSecondFactorPolicyProcedure,
		authv1connect.AccountServiceSetSecondFactorPolicyProcedure,
	} {
		request := httptest.NewRequest(http.MethodPost, path, nha181UnreadBody{t})
		request.Header.Set("Authorization", "Bearer "+canary)
		request.Header.Set("Forwarded", "proto=https")
		request.Header.Set("X-Forwarded-Proto", "https")
		writer := nha181DeadlineRecorder{httptest.NewRecorder()}
		handler.ServeHTTP(writer, request)
		if writer.Code != http.StatusForbidden {
			t.Fatalf("plaintext AuthService procedure %s returned %d", path, writer.Code)
		}
		nha049NoCanaries(t, "plaintext AuthService error", writer.Body.String(), canary)
	}
}

func TestNHA141GlobalLoginBucketPolicy(t *testing.T) {
	var bucket loginBucket
	now := time.Date(2026, time.October, 18, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		if !bucket.take(now) {
			t.Fatalf("burst ended after %d starts", i)
		}
	}
	if bucket.take(now) {
		t.Fatal("21st simultaneous login start escaped burst limit")
	}
	if !bucket.take(now.Add(time.Second)) || bucket.take(now.Add(time.Second)) {
		t.Fatal("60 per minute refill admitted too many or too few starts")
	}
	for i := 0; i < 20; i++ {
		if !bucket.take(now.Add(time.Minute)) {
			t.Fatalf("burst did not recover after one minute at slot %d", i)
		}
	}
	if bucket.take(now.Add(time.Minute)) {
		t.Fatal("refilled bucket exceeded the burst of 20")
	}
}

type nha141Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*nha141Timer]struct{}
}
type nha141Timer struct {
	clock    *nha141Clock
	deadline time.Time
	fire     func()
	finished bool
}

func newNHA141Clock(now time.Time) *nha141Clock {
	return &nha141Clock{now: now, timers: make(map[*nha141Timer]struct{})}
}
func (c *nha141Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *nha141Clock) At(deadline time.Time, fire func()) sessionTimer {
	timer := &nha141Timer{clock: c, deadline: deadline, fire: fire}
	c.mu.Lock()
	immediate := !c.now.Before(deadline)
	if immediate {
		timer.finished = true
	} else {
		c.timers[timer] = struct{}{}
	}
	c.mu.Unlock()
	if immediate {
		fire()
	}
	return timer
}
func (timer *nha141Timer) Stop() bool {
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	if timer.finished {
		return false
	}
	timer.finished = true
	delete(timer.clock.timers, timer)
	return true
}
func (c *nha141Clock) Advance(to time.Time) {
	var due []func()
	c.mu.Lock()
	if to.Before(c.now) {
		c.mu.Unlock()
		panic("NHA141 clock moved backward")
	}
	c.now = to
	for timer := range c.timers {
		if !to.Before(timer.deadline) {
			timer.finished = true
			delete(c.timers, timer)
			due = append(due, timer.fire)
		}
	}
	c.mu.Unlock()
	for _, fire := range due {
		fire()
	}
}

func nha141BearerRequest[T any](message *T, access string) *connect.Request[T] {
	request := connect.NewRequest(message)
	request.Header().Set("Authorization", "Bearer "+access)
	return request
}

func TestNHA141FactorDecisionsAndProofReservations(t *testing.T) {
	// NHA142 replaces the declarative bool with actual proof verification and
	// atomically consumed credentials, retaining these141 regression contracts.
	nha142ExerciseFactorReservations(t)
}

func TestNHA141RunTLSLoginRefreshExpiryAndOperatorRecovery(t *testing.T) {
	const firstPassword = "NHA141 synthetic first password canary"
	const replacementPassword = "NHA141 synthetic replacement password canary"
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha049Config(t, cluster.Port, dataDir)
	fakeClock := newNHA141Clock(time.Now().UTC().Truncate(time.Microsecond))
	streamEntered := make(chan struct{}, 2)
	streamCanceled := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listen net.ListenConfig
	go func() {
		done <- RunWithStartupFactory(ctx, settings.startup, listen.Listen, func(addr net.Addr) { ready <- addr }, func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
			plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: store})
			if err != nil {
				return RuntimePlan{}, err
			}
			plan.sessionClock = fakeClock
			err = plan.Register(RouteKindMedia, RouteMethodGet, "/nha141/stream", RouteAccessProtected, RouteFlowServerStream,
				RouteTransportTLSRequired, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					streamEntered <- struct{}{}
					<-r.Context().Done()
					streamCanceled <- struct{}{}
				}))
			if err == nil {
				// This fixture emits no object data. Its explicit READ binding
				// exercises native lease cancellation without an unknown-route
				// allow-all or an invented operational media permission.
				permission, admissionErr := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
				if admissionErr != nil {
					return RuntimePlan{}, admissionErr
				}
				err = plan.BindPermission(RouteKindMedia, RouteMethodGet, "/nha141/stream", permission)
			}
			return RuntimePlan{Identity: unconfiguredIdentity{}, Public: plan}, err
		})
	}()
	var address net.Addr
	expectRecorderDrainFailure := false
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not become ready")
	}
	defer func() {
		cancel()
		select {
		case err := <-done:
			if expectRecorderDrainFailure && !errors.Is(err, recorder.ErrDrainIncomplete) {
				t.Errorf("Run shutdown after PostgreSQL loss did not report incomplete Recorder drain: %v", err)
			} else if !expectRecorderDrainFailure && err != nil {
				t.Errorf("Run shutdown: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("Run did not drain")
		}
	}()
	operatorHTTP := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", filepath.Join(dataDir, operatorSocketName))
	}}}
	operator := authv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	publicHTTP := nha181Client(roots)
	publicHTTP.Timeout = 10 * time.Second
	defer publicHTTP.CloseIdleConnections()
	url := "https://" + address.String()
	auth := authv1connect.NewAuthServiceClient(publicHTTP, url)
	admin, err := operator.BootstrapAdmin(context.Background(), connect.NewRequest(&authv1.BootstrapAdminRequest{
		LoginName: "NHA141 Admin", DisplayName: "NHA141 Admin", Password: firstPassword}))
	if err != nil {
		t.Fatal(err)
	}
	loginAs := func(name, password string) *authv1.SessionTokens {
		t.Helper()
		response, err := auth.StartLogin(context.Background(), connect.NewRequest(&authv1.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil {
			t.Fatal(err)
		}
		tokens := response.Msg.Decision.GetAuthenticated()
		if tokens == nil {
			t.Fatal("password login did not authenticate")
		}
		return tokens
	}
	login := func(password string) *authv1.SessionTokens { return loginAs("NHA141 Admin", password) }
	listWith := func(access string) error {
		t.Helper()
		request := connect.NewRequest(&authv1.ListSessionsRequest{AccountId: admin.Msg.Account.Id})
		request.Header().Set("Authorization", "Bearer "+access)
		_, err := auth.ListSessions(context.Background(), request)
		return err
	}
	openStream := func(access string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, url+"/nha141/stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+access)
		response, err := publicHTTP.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || response.ProtoMajor != 2 {
			t.Fatalf("stream status=%d protocol=%s", response.StatusCode, response.Proto)
		}
		select {
		case <-streamEntered:
		case <-time.After(3 * time.Second):
			t.Fatal("stream handler not entered")
		}
		return response
	}
	const wrongPassword = "NHA141 synthetic wrong password canary"
	_, knownDenial := auth.StartLogin(context.Background(), connect.NewRequest(&authv1.StartLoginRequest{
		LoginName: "NHA141 Admin", Password: wrongPassword}))
	_, unknownDenial := auth.StartLogin(context.Background(), connect.NewRequest(&authv1.StartLoginRequest{
		LoginName: "NHA141 Unknown", Password: wrongPassword}))
	nha049Code(t, knownDenial, connect.CodeUnauthenticated)
	nha049Code(t, unknownDenial, connect.CodeUnauthenticated)
	if knownDenial.Error() != unknownDenial.Error() {
		t.Fatalf("known and unknown login denials differ: known=%v unknown=%v", knownDenial, unknownDenial)
	}
	nha049NoCanaries(t, "login denial", knownDenial.Error(), firstPassword, wrongPassword)
	first := login(firstPassword)
	if first.Session == nil || first.AccessExpiresAt == nil {
		t.Fatal("login omitted session metadata")
	}
	if !first.AccessExpiresAt.AsTime().Equal(fakeClock.Now().Add(15*time.Minute)) ||
		!first.Session.ExpiresAt.AsTime().Equal(fakeClock.Now().Add(30*24*time.Hour)) {
		t.Fatal("login lifetimes changed")
	}
	if err := listWith(first.AccessToken); err != nil {
		t.Fatalf("issued access denied: %v", err)
	}
	nha049Code(t, listWith(first.RefreshToken), connect.CodeUnauthenticated)
	unknownRefresh, err := accounts.NewRefreshSecret()
	if err != nil {
		t.Fatal(err)
	}
	unknownRefreshText, err := unknownRefresh.Text()
	if err != nil {
		t.Fatal(err)
	}
	_, err = auth.RefreshSession(context.Background(), connect.NewRequest(&authv1.RefreshSessionRequest{RefreshToken: unknownRefreshText}))
	nha049Code(t, err, connect.CodeUnauthenticated)
	if err := listWith(first.AccessToken); err != nil {
		t.Fatalf("unknown refresh revoked another session: %v", err)
	}
	fakeClock.Advance(fakeClock.Now().Add(time.Minute))
	refresh, err := auth.RefreshSession(context.Background(), connect.NewRequest(&authv1.RefreshSessionRequest{RefreshToken: first.RefreshToken}))
	if err != nil {
		t.Fatal(err)
	}
	second := refresh.Msg.Tokens
	if second == nil || !second.Session.ExpiresAt.AsTime().Equal(first.Session.ExpiresAt.AsTime()) {
		t.Fatal("refresh slid absolute session expiry")
	}
	nha049Code(t, listWith(first.AccessToken), connect.CodeUnauthenticated)
	_, err = auth.RefreshSession(context.Background(), connect.NewRequest(&authv1.RefreshSessionRequest{RefreshToken: first.RefreshToken}))
	nha049Code(t, err, connect.CodeUnauthenticated)
	nha049Code(t, listWith(second.AccessToken), connect.CodeUnauthenticated)
	third := login(firstPassword)
	stream := openStream(third.AccessToken)
	fakeClock.Advance(third.AccessExpiresAt.AsTime())
	select {
	case <-streamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not cancel at access expiry")
	}
	stream.Body.Close()
	nha049Code(t, listWith(third.AccessToken), connect.CodeUnauthenticated)
	fourth := login(firstPassword)
	stream = openStream(fourth.AccessToken)
	_, err = operator.RecoverAccount(context.Background(), connect.NewRequest(&authv1.RecoverAccountRequest{
		AccountId: admin.Msg.Account.Id, ReplacementPassword: replacementPassword}))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("operator recovery did not cancel stream")
	}
	stream.Body.Close()
	nha049Code(t, listWith(fourth.AccessToken), connect.CodeUnauthenticated)
	_, err = auth.StartLogin(context.Background(), connect.NewRequest(&authv1.StartLoginRequest{LoginName: "NHA141 Admin", Password: firstPassword}))
	nha049Code(t, err, connect.CodeUnauthenticated)
	adminAfterRecovery := login(replacementPassword)
	secondAdminSession := login(replacementPassword)
	stream = openStream(adminAfterRecovery.AccessToken)
	_, err = auth.RevokeSession(context.Background(), nha141BearerRequest(&authv1.RevokeSessionRequest{SessionId: adminAfterRecovery.Session.Id}, secondAdminSession.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("individual session revocation did not cancel its stream")
	}
	stream.Body.Close()
	nha049Code(t, listWith(adminAfterRecovery.AccessToken), connect.CodeUnauthenticated)
	if err := listWith(secondAdminSession.AccessToken); err != nil {
		t.Fatalf("individual revocation canceled the other session: %v", err)
	}
	accountsClient := authv1connect.NewAccountServiceClient(publicHTTP, url)
	const userPassword = "NHA141 synthetic user password canary"
	user, err := accountsClient.CreateAccount(context.Background(), nha141BearerRequest(&authv1.CreateAccountRequest{
		LoginName: "NHA141 User", DisplayName: "NHA141 User", Role: authv1.AccountRole_ACCOUNT_ROLE_USER,
		InitialPassword: userPassword}, secondAdminSession.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	userSession := loginAs("NHA141 User", userPassword)
	stream = openStream(userSession.AccessToken)
	elevated := authv1.AccountRole_ACCOUNT_ROLE_ADMIN
	_, err = accountsClient.UpdateAccount(context.Background(), nha141BearerRequest(&authv1.UpdateAccountRequest{
		AccountId: user.Msg.Account.Id, Role: &elevated}, secondAdminSession.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("role change did not cancel user stream")
	}
	stream.Body.Close()
	nha049Code(t, listWith(userSession.AccessToken), connect.CodeUnauthenticated)
	userSession = loginAs("NHA141 User", userPassword)
	stream = openStream(userSession.AccessToken)
	disabled := authv1.AccountStatus_ACCOUNT_STATUS_DISABLED
	_, err = accountsClient.UpdateAccount(context.Background(), nha141BearerRequest(&authv1.UpdateAccountRequest{
		AccountId: user.Msg.Account.Id, Status: &disabled}, secondAdminSession.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("status change did not cancel user stream")
	}
	stream.Body.Close()
	nha049Code(t, listWith(userSession.AccessToken), connect.CodeUnauthenticated)
	_, err = auth.StartLogin(context.Background(), connect.NewRequest(&authv1.StartLoginRequest{LoginName: "NHA141 User", Password: userPassword}))
	nha049Code(t, err, connect.CodeUnauthenticated)
	active := authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE
	_, err = accountsClient.UpdateAccount(context.Background(), nha141BearerRequest(&authv1.UpdateAccountRequest{
		AccountId: user.Msg.Account.Id, Status: &active}, secondAdminSession.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	userSession = loginAs("NHA141 User", userPassword)
	stream = openStream(userSession.AccessToken)
	_, err = accountsClient.DeleteAccount(context.Background(), nha141BearerRequest(&authv1.DeleteAccountRequest{
		AccountId: user.Msg.Account.Id}, secondAdminSession.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("account deletion did not cancel user stream")
	}
	stream.Body.Close()
	nha049Code(t, listWith(userSession.AccessToken), connect.CodeUnauthenticated)
	concurrent := login(replacementPassword)
	var refreshWait sync.WaitGroup
	refreshStart := make(chan struct{})
	refreshResults := make([]*authv1.SessionTokens, 2)
	refreshErrors := make([]error, 2)
	for i := range refreshResults {
		refreshWait.Add(1)
		go func(i int) {
			defer refreshWait.Done()
			<-refreshStart
			response, err := auth.RefreshSession(context.Background(), connect.NewRequest(&authv1.RefreshSessionRequest{
				RefreshToken: concurrent.RefreshToken}))
			refreshErrors[i] = err
			if err == nil {
				refreshResults[i] = response.Msg.Tokens
			}
		}(i)
	}
	close(refreshStart)
	refreshWait.Wait()
	winners := 0
	for i, result := range refreshResults {
		if refreshErrors[i] == nil {
			winners++
			if result == nil {
				t.Fatal("refresh returned success without credentials")
			}
			nha049Code(t, listWith(result.AccessToken), connect.CodeUnauthenticated)
		} else {
			nha049Code(t, refreshErrors[i], connect.CodeUnauthenticated)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent CAS refresh winners=%d, want one", winners)
	}
	storeSettings, err := settings.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	verificationStore, err := storage.Open(context.Background(), storeSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer verificationStore.Close()
	var persisted []string
	if err := verificationStore.InTransaction(context.Background(), func(tx pgx.Tx) error {
		for _, table := range []string{"auth_accounts", "auth_audit", "auth_login_attempts", "auth_sessions", "auth_refresh_generations", "auth_access_credentials", "auth_login_challenges", "auth_enrollment_grants"} {
			rows, err := tx.Query(context.Background(), "SELECT row_to_json(t)::text FROM public."+table+" t")
			if err != nil {
				return err
			}
			for rows.Next() {
				var row string
				if err := rows.Scan(&row); err != nil {
					rows.Close()
					return err
				}
				persisted = append(persisted, row)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, row := range persisted {
		for _, secret := range []string{firstPassword, replacementPassword, wrongPassword, userPassword,
			first.AccessToken, first.RefreshToken, concurrent.RefreshToken} {
			if strings.Contains(row, secret) {
				t.Fatal("persisted authentication row contained a raw credential canary")
			}
		}
	}
	if err := cluster.Stop(); err != nil {
		t.Fatal(err)
	}
	expectRecorderDrainFailure = true
	if err := listWith(secondAdminSession.AccessToken); err == nil {
		t.Fatal("database outage admitted an access credential")
	} else if connect.CodeOf(err) != connect.CodeUnauthenticated && connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("database outage returned unexpected code: %v", err)
	}
}

func TestNHA141DefaultRunComposesSessionProvider(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha049Config(t, cluster.Port, dataDir)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listen net.ListenConfig
	go func() {
		done <- RunWithStartup(ctx, settings.startup, listen.Listen, func(addr net.Addr) { ready <- addr })
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("default Run exited before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("default Run did not become ready")
	}
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("default Run shutdown: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("default Run did not drain")
		}
	}()
	operatorHTTP := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", filepath.Join(dataDir, operatorSocketName))
	}}}
	operator := authv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	const password = "NHA141 synthetic default Run password"
	admin, err := operator.BootstrapAdmin(context.Background(), connect.NewRequest(&authv1.BootstrapAdminRequest{
		LoginName: "NHA141 Default Admin", DisplayName: "NHA141 Default Admin", Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	publicHTTP := nha181Client(roots)
	defer publicHTTP.CloseIdleConnections()
	url := "https://" + address.String()
	auth := authv1connect.NewAuthServiceClient(publicHTTP, url)
	login, err := auth.StartLogin(context.Background(), connect.NewRequest(&authv1.StartLoginRequest{
		LoginName: "NHA141 Default Admin", Password: password}))
	if err != nil || login == nil || login.Msg.Decision.GetAuthenticated() == nil {
		t.Fatalf("default Run session provider did not authenticate: %v", err)
	}
	_, err = auth.ListSessions(context.Background(), nha141BearerRequest(&authv1.ListSessionsRequest{
		AccountId: admin.Msg.Account.Id}, login.Msg.Decision.GetAuthenticated().AccessToken))
	if err != nil {
		t.Fatalf("default Run did not admit issued access: %v", err)
	}
}
