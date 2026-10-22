package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	"github.com/damione1/Homeward/gen/home/configurationapply/v1/configurationapplyv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func nha055UncertainSelfPolicyFixture(t *testing.T, abort bool) (nha055TLSFixture, *configactivation.Owner, *nha029OwnerCommitACKProxy, *pgx.Conn, *nha150StepClock) {
	t.Helper()
	cluster := testpg.Start(t)
	proxy := nha029OwnerStartCommitACKProxy(t, cluster.Port)
	proxy.abort = abort
	observer := nha030ServerObserver(t, cluster.Port)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, proxy.port(t), data, "127.0.0.1:0", nha181TLSYAML())
	source, err := cfg.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha150StepClock{now: time.Now(), waiting: make(chan struct{}, 1), advance: make(chan struct{})}
	runCtx, stop := context.WithCancel(context.Background())
	run := &nha162Running{stop: stop, done: make(chan error, 1)}
	ready := make(chan net.Addr, 1)
	observed := make(chan nha162ObservedRun, 1)
	owners := make(chan *configactivation.Owner, 1)
	dependencies := runDependencies{
		configurationWatcherClock: clock,
		observeLists: func(lists *listComposition, sessions *sessionCoordinator) {
			observed <- nha162ObservedRun{lists, sessions}
		},
		observeRemoteActivation: func(owner *configactivation.Owner) { owners <- owner },
	}
	go func() {
		run.done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, nil, dependencies)
	}()
	t.Cleanup(func() {
		if !run.joined {
			if err := run.join(t); err != nil {
				t.Error("uncertain self-policy Run cleanup", err)
			}
		}
	})
	select {
	case run.address = <-ready:
	case err := <-run.done:
		run.joined = true
		t.Fatal("actual default Run exited before ready", err)
	case <-time.After(12 * time.Second):
		t.Fatal("actual default Run did not become ready")
	}
	select {
	case run.observed = <-observed:
	default:
		t.Fatal("actual session coordinator was not observed")
	}
	var owner *configactivation.Owner
	select {
	case owner = <-owners:
	default:
		t.Fatal("actual Owner was not observed")
	}
	// Keep the same real watcher paused throughout the ambiguous request.
	// No background reconciliation can release the policy barrier for it.
	clock.paused(t)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	const password = "NHA055 uncertain self-policy password"
	nha162RunBootstrap(t, ctx, data, "NHA055 uncertain admin", password)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA055 uncertain admin", password)
	f := nha055TLSFixture{nha053TLSFixture: nha053TLSFixture{nha149TLSFixture: nha149TLSFixture{
		api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication,
		accounts: authv1connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken,
		path: path, raw: source.Bytes(), run: run, ctx: ctx,
	}, policies: authv1connect.NewAuthorizationPolicyServiceClient(client, base)}, apply: configurationapplyv1connect.NewConfigurationApplyServiceClient(client, base)}
	return f, owner, proxy, observer, clock
}

func TestNHA055ActualRunTLSSelfPolicyUnknownReturnsBeforeReconciliation(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprintf("backend-rollback=%v", abort), func(t *testing.T) {
			f, owner, proxy, observer, _ := nha055UncertainSelfPolicyFixture(t, abort)
			// Resolve even a failing regression before Run cleanup, so a blocked
			// old handler cannot retain the account barrier or an HTTP goroutine.
			defer func() {
				proxy.allowDrop()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = owner.Reconcile(ctx)
			}()
			account := f.tokens.Session.AccountId
			policy, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
			if err != nil || policy.Msg.GetPresent() == nil {
				t.Fatal("initial own policy", err)
			}
			prior := policy.Msg.GetPresent()
			before := f.read(t)
			raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n", account.Value))
			request := nha055Request(before, nha055PolicyCandidate(t, account, raw), nil)
			request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, prior.Version, nil)}
			type answer struct {
				response *connect.Response[wire.ApplyResponse]
				err      error
			}
			completed := make(chan answer, 1)
			proxy.armed.Store(true)
			go func() {
				response, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
				completed <- answer{response, err}
			}()
			nha034WaitSignal(t, proxy.held, "055 real policy transaction ACK withheld")
			var sqlRaw []byte
			var sqlRevision, generation, commands int64
			err = observer.QueryRow(f.ctx, `SELECT v.yaml_source,p.version_revision,
			 (SELECT generation_revision FROM public.config_active WHERE singleton),
			 (SELECT count(*) FROM public.command_intents)
			 FROM public.authorization_policy_active p JOIN public.config_versions v
			 ON v.document_id=p.document_id AND v.revision=p.version_revision WHERE p.account_id=$1`, account.Value).Scan(&sqlRaw, &sqlRevision, &generation, &commands)
			if err != nil || commands != 0 {
				t.Fatal("independent SQL observer", err)
			}
			if abort {
				if !bytes.Equal(sqlRaw, prior.YamlSource) || sqlRevision != int64(prior.Version.Revision) || generation != before.ActiveGeneration.Revision {
					t.Fatal("proxy rollback did not preserve original SQL authority")
				}
			} else if !bytes.Equal(sqlRaw, raw) || sqlRevision != int64(prior.Version.Revision)+1 || generation != before.ActiveGeneration.Revision+1 {
				t.Fatal("backend did not commit policy and generation before withholding ACK")
			}
			proxy.allowDrop()
			var result answer
			select {
			case result = <-completed:
			case <-time.After(3 * time.Second):
				t.Fatal("self-policy unknown response waited on its own retained policy barrier")
			}
			if result.err != nil || result.response == nil || result.response.Msg.Activation == nil {
				t.Fatal("unknown attempt was hidden by authorization failure", result.err)
			}
			if result.response.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_COMMIT_UNKNOWN || result.response.Msg.Activation.Transaction != configuration.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN || result.response.Msg.Current != nil {
				t.Fatal("ambiguous attempt was reported as known or exposed a new snapshot")
			}
			if _, err := owner.CurrentConfig(f.ctx); err == nil {
				t.Fatal("response depended on prior external reconciliation")
			}
			accountID, err := contracts.ParseAccountID(account.Value)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := storage.AuthorizationPolicyBinding(accountID)
			if err != nil {
				t.Fatal(err)
			}
			group := sha256.Sum256([]byte(binding.ID().String()))
			staged, err := os.ReadFile(filepath.Join(filepath.Dir(f.path), "configuration.d", hex.EncodeToString(group[:]), filepath.FromSlash(binding.Path())))
			if err != nil || !bytes.Equal(staged, raw) {
				t.Fatal("ambiguous transaction inverted or lost staged policy bytes", err)
			}
			resolved, err := owner.Reconcile(f.ctx)
			if err != nil {
				t.Fatal("real Owner could not reconcile actual SQL outcome", err)
			}
			if abort {
				if resolved.Outcome() != configactivation.NotCommitted || resolved.AttemptOutcome() != storage.TransactionNotCommitted {
					t.Fatal("actual rollback did not reconcile as not committed")
				}
			} else if resolved.Outcome() != configactivation.Published || resolved.AttemptOutcome() != storage.TransactionCommitted {
				t.Fatal("actual commit did not reconcile as published")
			}
			operational, err := os.ReadFile(f.path)
			if err != nil || !bytes.Equal(operational, f.raw) {
				t.Fatal("policy request changed operational YAML", err)
			}
		})
	}
}
