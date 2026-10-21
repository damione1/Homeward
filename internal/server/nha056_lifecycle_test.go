package server

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func nha056SessionRequest[T any](message *T, token string) *connect.Request[T] {
	request := connect.NewRequest(message)
	request.Header().Set("Authorization", "Bearer "+token)
	return request
}
func TestNHA056ActualTLSClientReadsAdmittedFactsAndPersistedAudit(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	run, runtime, _ := nha056ObservedDefaultRun(t, cfg)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const login = "NHA056 facts admin"
	const password = "NHA056_ADMIN_PASSWORD_CANARY"
	account := nha162RunBootstrap(t, ctx, data, login, password)
	token := nha162RunLogin(t, ctx, authv1connect.NewAuthServiceClient(client, base), login, password).AccessToken
	accounts := authv1connect.NewAccountServiceClient(client, base)
	created, err := accounts.CreateAccount(ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA056 audit user", DisplayName: "NHA056 audit user", Role: auth.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}, token))
	if err != nil {
		t.Fatal(err)
	}
	nha162RunPolicy(t, ctx, authv1connect.NewAuthorizationPolicyServiceClient(client, base), created.Msg.Account.Id, token, "  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
	mfaPolicy, err := accounts.GetSecondFactorPolicy(ctx, nha056SessionRequest(&auth.GetSecondFactorPolicyRequest{}, token))
	if err != nil {
		t.Fatal("real MFA policy read failed", err)
	}
	// GetSecondFactorPolicy emits a transport audit, without a dedicated action29
	// row. The real SetMFAPolicy producer persists action28 even for the same
	// value, so exercise that operation and retain a positive MFA audit oracle.
	if _, err := accounts.SetSecondFactorPolicy(ctx, nha056SessionRequest(&auth.SetSecondFactorPolicyRequest{Expected: mfaPolicy.Msg.Policy.Version, RequireAdminFactor: mfaPolicy.Msg.Policy.RequireAdminFactor}, token)); err != nil {
		t.Fatal("real MFA policy replacement failed", err)
	}
	requester, err := domain.ParseIntegrationInstanceID("NHA056_REQUESTER_CANARY")
	if err != nil {
		t.Fatal(err)
	}
	session, err := runtime.host.OpenSession(requester)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.host.Revoke(session)
	provider, err := session.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Read(ctx, nha051ServerHandle(t, "NHA056_FOREIGN_INSTANCE_CANARY", "NHA056_SECRET_NAME_CANARY")); !errors.Is(err, integrations.ErrForeignSecretHandle) {
		t.Fatal("real foreign secret read was not refused", err)
	}
	if err := runtime.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO public.auth_audit(action,outcome,route_path) VALUES(12,1,'/NHA056_HISTORICAL_SIGNED_URI_CANARY')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reader := observabilityv1connect.NewObservabilityServiceClient(client, base)
	health, err := reader.GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, token))
	if err != nil || contracts.AdmitObservabilityResponse(health.Msg) != nil {
		t.Fatal("real TLS health not admitted", err)
	}
	if health.Msg.Health.Liveness != obs.Liveness_LIVENESS_LIVE || health.Msg.Health.Server.Generation.GetNotLoaded() == nil || health.Msg.Health.Server.Readiness == obs.ServerReadiness_SERVER_READINESS_READY {
		t.Fatal("default fixture invented a generation")
	}
	if health.Msg.Health.Durable.GetObservedAt() == nil || health.Msg.Health.Durable.Availability != obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE {
		t.Fatal("actual Recorder startup commit was not a durable fact")
	}
	metrics, err := reader.GetMetrics(ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, token))
	if err != nil || contracts.AdmitObservabilityResponse(metrics.Msg) != nil {
		t.Fatal("real TLS metrics not admitted", err)
	}
	if len(metrics.Msg.Metrics.Queues) != 9 || len(metrics.Msg.Metrics.QueueLags) != 4 || len(observability.MetricCatalogue()) != 124 {
		t.Fatal("real catalogue drift")
	}
	size := uint32(100)
	logs, err := reader.QueryRuntimeLogs(ctx, nha056SessionRequest(&obs.QueryRuntimeLogsRequest{PageSize: &size}, token))
	if err != nil {
		t.Fatal(err)
	}
	positive := false
	for _, row := range logs.Msg.GetPage().Rows {
		if row.Event.Code == obs.LogCode_LOG_CODE_PROCESS_READY && row.Event.Author.GetProcess() != nil && row.Event.OccurredAt != nil {
			positive = true
		}
	}
	if !positive {
		t.Fatal("real startup emitter did not publish its safe code")
	}
	audits, err := reader.QueryAudits(ctx, nha056SessionRequest(&obs.QueryAuditsRequest{Source: obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION, PageSize: &size}, token))
	if err != nil || contracts.AdmitObservabilityResponse(audits.Msg) != nil {
		t.Fatal("real persisted audits unavailable", err)
	}
	positive = false
	for _, row := range audits.Msg.GetPage().Rows {
		if audit := row.GetAuthentication(); audit != nil && audit.Action == obs.AuthAuditAction_AUTH_AUDIT_ACTION_TRANSPORT && audit.Author.GetHumanAccount().GetValue() == account.Value && audit.OccurredAt != nil && audit.Route.GetRegisteredProcedure() == observabilityv1connect.ObservabilityServiceQueryAuditsProcedure {
			positive = true
		}
	}
	if !positive {
		t.Fatal("transport audit lost its real persisted actor/date/registered route")
	}
	// These rows come from the real account, session, policy and MFA services
	// above, then through the common TLS reader. Match persisted dates/actors by
	// PK instead of inferring them from the response time or target account.
	seen := make(map[obs.AuthAuditAction]bool)
	historical := false
	mfaReadTransport := false
	mfaReplaceTransport := false
	for _, row := range audits.Msg.GetPage().Rows {
		audit := row.GetAuthentication()
		if audit == nil {
			continue
		}
		var at time.Time
		var actor *string
		var target *string
		var persistedAction int16
		if err := runtime.store.InTransaction(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT occurred_at,actor_account_id,target_account_id,action FROM public.auth_audit WHERE id=$1`, row.Id).Scan(&at, &actor, &target, &persistedAction)
		}); err != nil {
			t.Fatal(err)
		}
		if !audit.OccurredAt.AsTime().Equal(at) {
			t.Fatal("audit date differs from its persisted source")
		}
		if actor == nil {
			if audit.Author.GetNotRecorded() == nil {
				t.Fatal("absent audit author was invented")
			}
		} else if audit.Author.GetHumanAccount().GetValue() != *actor {
			t.Fatal("audit actor differs from its persisted source")
		}
		seen[audit.Action] = true
		if audit.Action == obs.AuthAuditAction_AUTH_AUDIT_ACTION_MFA_POLICY_REPLACE {
			// insertSessionAudit stores this operation's native account as the
			// target, without an actor. Preserve that absence; the real TLS
			// transport row below supplies the authenticated actor independently.
			if persistedAction != 28 || actor != nil || audit.Author.GetNotRecorded() == nil || target == nil || *target != account.Value {
				t.Fatal("MFA audit differs from its persisted action, absent actor or native target")
			}
			t.Logf("actual MFA replacement audit PK=%d persisted_action=%d actor_not_recorded=true target_matches_native=true", row.Id, persistedAction)
		}
		if audit.Action == obs.AuthAuditAction_AUTH_AUDIT_ACTION_TRANSPORT && audit.Author.GetHumanAccount().GetValue() == account.Value && audit.Outcome == obs.AuditOutcome_AUDIT_OUTCOME_ADMITTED {
			switch audit.Route.GetRegisteredProcedure() {
			case authv1connect.AccountServiceGetSecondFactorPolicyProcedure:
				mfaReadTransport = true
			case authv1connect.AccountServiceSetSecondFactorPolicyProcedure:
				mfaReplaceTransport = true
			}
		}
		if audit.Route.GetOtherRegisteredRoute() != nil {
			historical = true
		}
	}
	for _, action := range []obs.AuthAuditAction{obs.AuthAuditAction_AUTH_AUDIT_ACTION_BOOTSTRAP, obs.AuthAuditAction_AUTH_AUDIT_ACTION_CREATE_ACCOUNT, obs.AuthAuditAction_AUTH_AUDIT_ACTION_SESSION_CREATE, obs.AuthAuditAction_AUTH_AUDIT_ACTION_POLICY_REPLACE, obs.AuthAuditAction_AUTH_AUDIT_ACTION_MFA_POLICY_REPLACE} {
		if !seen[action] {
			t.Fatal("real service audit missing from TLS reader", action)
		}
	}
	if !historical {
		t.Fatal("historical unregistered route did not become the closed OtherRegisteredRoute variant")
	}
	if !mfaReadTransport {
		t.Fatal("real MFA policy read transport audit missing from TLS reader")
	}
	if !mfaReplaceTransport {
		t.Fatal("real MFA policy replacement transport lost its authenticated native actor")
	}
	secrets, err := reader.QueryAudits(ctx, nha056SessionRequest(&obs.QueryAuditsRequest{Source: obs.AuditSource_AUDIT_SOURCE_SECRET_ACCESS, PageSize: &size}, token))
	if err != nil || contracts.AdmitObservabilityResponse(secrets.Msg) != nil || len(secrets.Msg.GetPage().Rows) != 1 {
		t.Fatal("real secret refusal missing from TLS reader", err)
	}
	secretAudit := secrets.Msg.GetPage().Rows[0].GetSecretAccess()
	if secretAudit == nil || secretAudit.Author.GetInternalIntegration() == nil || secretAudit.OccurredAt == nil || secretAudit.Reason != obs.SecretDenialReason_SECRET_DENIAL_REASON_FOREIGN_HANDLE {
		t.Fatal("secret refusal lost its closed reason, date or opaque internal author")
	}
	var secretAt time.Time
	if err := runtime.store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT recorded_at FROM public.secret_access_audit WHERE id=$1`, secrets.Msg.GetPage().Rows[0].Id).Scan(&secretAt)
	}); err != nil {
		t.Fatal(err)
	}
	reference, err := runtime.codec.IntegrationReference(requester)
	if err != nil {
		t.Fatal(err)
	}
	opaque, err := reference.Wire()
	if err != nil || !proto.Equal(opaque, secretAudit.Author.GetInternalIntegration()) || !secretAudit.OccurredAt.AsTime().Equal(secretAt) {
		t.Fatal("secret refusal changed its persisted date or actual requester", err)
	}
	for _, message := range []proto.Message{health.Msg, metrics.Msg, logs.Msg, audits.Msg, secrets.Msg} {
		encoded, err := protojson.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{password, "NHA056_HISTORICAL_SIGNED_URI_CANARY", "NHA056_REQUESTER_CANARY", "NHA056_FOREIGN_INSTANCE_CANARY", "NHA056_SECRET_NAME_CANARY"} {
			if bytes.Contains(encoded, []byte(canary)) {
				t.Fatal("sensitive payload escaped a real reader")
			}
		}
	}
	orphanEpoch := bytes.Repeat([]byte{0xa5}, 16)
	coverage, err := reader.ReadRecorderCoverage(ctx, nha056SessionRequest(&obs.ReadRecorderCoverageRequest{From: &obs.AdmissionPosition{Epoch: orphanEpoch, Sequence: 0}, Through: &obs.ReadRecorderCoverageRequest_LatestInEpoch{LatestInEpoch: &obs.RecorderEpochIdentity{Value: orphanEpoch}}}, token))
	if err != nil || coverage.Msg.GetNotRetained() == nil {
		t.Fatal("absent retained Recorder epoch lost its explicit result", err)
	}
	for _, invalid := range []uint32{0, 101} {
		if _, err := reader.QueryRuntimeLogs(ctx, nha056SessionRequest(&obs.QueryRuntimeLogsRequest{PageSize: &invalid}, token)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatal("invalid page boundary admitted", err)
		}
	}
	if _, err := reader.GetHealth(ctx, connect.NewRequest(&obs.GetHealthRequest{})); err == nil {
		t.Fatal("anonymous reader admitted")
	}
}
