package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	integration "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/gen/home/integrationadmin/v1/integrationadminv1connect"
	"github.com/damione1/Homeward/gen/home/notifications/v1/notificationsv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type nhf022FixtureControl uint8

const (
	nhf022ControlSeedNominal nhf022FixtureControl = iota + 1
	nhf022ControlSeedEntity
	nhf022ControlSeedExpiring
	nhf022ControlWithdrawTarget
	nhf022ControlObserve
)

var nhf022Controls = []struct {
	file string
	kind nhf022FixtureControl
}{
	{"seed-nominal", nhf022ControlSeedNominal},
	{"seed-entity", nhf022ControlSeedEntity},
	{"seed-expiring", nhf022ControlSeedExpiring},
	{"withdraw-target", nhf022ControlWithdrawTarget},
	{"capture-state", nhf022ControlObserve},
}

func TestNHF022NotificationServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF022_FIXTURE")
	if out == "" {
		t.Skip("explicit isolated notification fixture only")
	}
	if os.Getenv("HOMEWARD_NHF022_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("fresh absolute synthetic fixture required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	captured, err := settings.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(source, captured.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(source, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	run := nha162StartDefaultRun(t, startup)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const adminLogin = "NHF022 isolated administrator"
	const adminPassword = "NHF022 isolated administrator password"
	nha162RunBootstrap(t, ctx, data, adminLogin, adminPassword)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	accounts := authv1connect.NewAccountServiceClient(client, base)
	admin := nha162RunLogin(t, ctx, authentication, adminLogin, adminPassword)
	const login = "NHF022 synthetic ordinary user"
	const password = "NHF022 synthetic ordinary password"
	user, err := accounts.CreateAccount(ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: login, DisplayName: login, InitialPassword: password, Role: auth.AccountRole_ACCOUNT_ROLE_USER}, admin.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	documents := configurationv1connect.NewConfigurationServiceClient(client, base)
	before, err := documents.ReadDocument(ctx, nha056SessionRequest(&cfg.ReadDocumentRequest{Id: &cfg.DocumentId{Value: "operational.main"}}, admin.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	handler := nhf021NewCompanion(run.observed.sessions.authority)
	notifications := nhf022NewNotifications(handler)
	notificationPath, notificationHandler := notificationsv1connect.NewNotificationServiceHandler(notifications, connect.WithReadMaxBytes(128*1024), connect.WithSendMaxBytes(128*1024))
	origin := nhf021StartProxy(t, out, data, base, roots, run, handler, nhf021TestRoute{
		path: notificationPath, handler: notificationHandler,
		isRead: func(procedure string) bool {
			switch procedure {
			case notificationsv1connect.NotificationServiceDescribeNotificationsProcedure,
				notificationsv1connect.NotificationServiceGetDestinationProcedure,
				notificationsv1connect.NotificationServiceReadInboxProcedure,
				notificationsv1connect.NotificationServiceGetDeliveryProcedure,
				notificationsv1connect.NotificationServiceResolveOpenProcedure:
				return true
			default:
				return false
			}
		},
		onLostReply: func() { notifications.mu.Lock(); notifications.counts.LostReplies++; notifications.mu.Unlock() },
	})
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": origin, "ca_pem": string(certificate),
		"user": map[string]string{"login_name": login, "password": password}, "admin": map[string]string{"login_name": adminLogin, "password": adminPassword},
		"production_configuration": false, "no_physical_providers": true})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	revoked := false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("notification fixture deadline", ctx.Err())
		case <-ticker.C:
			for _, control := range nhf022Controls {
				marker := filepath.Join(out, control.file)
				if _, err := os.Stat(marker); os.IsNotExist(err) {
					continue
				} else if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				switch control.kind {
				case nhf022ControlSeedNominal:
					if err := notifications.seed(nhf022SeedNominal); err != nil {
						t.Fatal(err)
					}
				case nhf022ControlSeedEntity:
					if err := notifications.seed(nhf022SeedEntity); err != nil {
						t.Fatal(err)
					}
				case nhf022ControlSeedExpiring:
					if err := notifications.seed(nhf022SeedExpiring); err != nil {
						t.Fatal(err)
					}
				case nhf022ControlObserve:
					handler.mu.Lock()
					installations := make([]proto.Message, 0, len(handler.records))
					for _, record := range handler.records {
						installations = append(installations, proto.Clone(record.value))
					}
					companionCounts := handler.counts
					handler.mu.Unlock()
					notifications.mu.Lock()
					destinations := make([]proto.Message, 0, len(notifications.records))
					deliveries := make([]proto.Message, 0)
					for _, record := range notifications.records {
						destinations = append(destinations, proto.Clone(record.value))
						for _, entry := range record.entries {
							deliveries = append(deliveries, proto.Clone(entry))
						}
					}
					counts := notifications.counts
					notifications.mu.Unlock()
					encode := func(messages []proto.Message) []json.RawMessage {
						values := make([]json.RawMessage, 0, len(messages))
						for _, message := range messages {
							raw, err := (protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}).Marshal(message)
							if err != nil {
								t.Fatal(err)
							}
							values = append(values, raw)
						}
						return values
					}
					// Private test output only. No access/refresh credentials,
					// public RPC, provider or installation mutation is added.
					nhf003WritePrivate(t, out, "observed-state.json", map[string]any{"installations": encode(installations), "destinations": encode(destinations), "deliveries": encode(deliveries), "companion_counts": companionCounts, "counts": counts})
				case nhf022ControlWithdrawTarget:
					notifications.mu.Lock()
					notifications.targetReadable = false
					notifications.mu.Unlock()
				}
				nhf003WritePrivate(t, out, control.file+"-done.json", map[string]bool{"applied": true})
			}
			if _, err := os.Stat(filepath.Join(out, "revoke-account")); err == nil && !revoked {
				_, err := accounts.UpdateAccount(ctx, nha056SessionRequest(&auth.UpdateAccountRequest{AccountId: user.Msg.Account.Id, Status: auth.AccountStatus_ACCOUNT_STATUS_DISABLED.Enum()}, admin.AccessToken))
				if err != nil {
					t.Fatal(err)
				}
				revoked = true
				nhf003WritePrivate(t, out, "account-revoked.json", map[string]bool{"revoked": true})
			}
			if _, err := os.Stat(filepath.Join(out, "stop")); err != nil {
				continue
			}
			after, err := documents.ReadDocument(ctx, nha056SessionRequest(&cfg.ReadDocumentRequest{Id: before.Msg.Document.Id}, admin.AccessToken))
			if err != nil || !proto.Equal(before.Msg.Document, after.Msg.Document) || !proto.Equal(before.Msg.ActiveGeneration, after.Msg.ActiveGeneration) {
				t.Fatal("notification changed configuration", err)
			}
			instances, err := integrationadminv1connect.NewIntegrationServiceClient(client, base).ListInstances(ctx, nha056SessionRequest(&integration.ListInstancesRequest{}, admin.AccessToken))
			if err != nil || len(instances.Msg.Instances) != 0 {
				t.Fatal("physical provider introduced", err)
			}
			if err := run.join(t); err != nil {
				t.Fatal(err)
			}
			handler.mu.Lock()
			companionCounts := handler.counts
			handler.mu.Unlock()
			notifications.mu.Lock()
			counts := notifications.counts
			notifications.mu.Unlock()
			nhf003WritePrivate(t, out, "result.json", map[string]any{"synthetic_only": true, "actual_run_joined": true, "production_configuration": false, "configuration_unchanged": true, "no_physical_providers": true, "account_revoked": revoked, "counts": counts, "companion_counts": companionCounts})
			return
		}
	}
}
