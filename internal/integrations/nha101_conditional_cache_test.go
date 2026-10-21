package integrations_test

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

func nha101WireFactory(t *testing.T, raw string, key integrations.SourceKey, auth integrations.ConditionalAuth, resource *integrations.ConditionalResource, policy *integrations.CloudPolicy) integrations.ConditionalFactory {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := integrations.ExplicitConditionalDestination("http://calendar.invalid:"+u.Port(), []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := integrations.PublicConditionalResource("http://calendar.invalid:" + u.Port() + "/feed?b=2&a=1&a=0")
	if err != nil {
		t.Fatal(err)
	}
	if resource != nil {
		selected = *resource
	}
	settings, err := integrations.NewConditionalSettings(key, selected, auth, destination, sha256.Sum256([]byte("ics profile and explicit UTC projection")))
	if err != nil {
		t.Fatal(err)
	}
	factory, err := integrations.NHA101ConditionalFactoryForTest(settings, policy, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != net.JoinHostPort("203.0.113.8", u.Port()) {
			return nil, fmt.Errorf("test refused unexpected numeric destination")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	})
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

// Uses ordinary migration(s) for the real SecretProvider/Session. No remote
// cache row, selection or receipt is simulated; this is a carrier-only test.
func TestNHA101ConditionalCarrierWithoutCacheMigration(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	key, _ := integrations.NewSourceKey("calendar-source", "calendar")
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.RawQuery != "b=2&a=1&a=0" || r.Header.Get("Accept") != "text/calendar" || r.Header.Get("User-Agent") != "Homeward-RemoteCalendar/1" || r.Header.Get("Accept-Encoding") != "gzip" {
			t.Error("representation request profile changed")
		}
		switch count.Add(1) {
		case 1:
			w.Header().Set("Etag", `"carrier"`)
			_, _ = w.Write([]byte("synthetic calendar bytes"))
		case 2:
			if r.Header.Get("If-None-Match") != `"carrier"` {
				t.Error("conditional validator missing")
			}
			w.WriteHeader(304)
		default:
			w.WriteHeader(206)
		}
	}))
	defer server.Close()
	factory := nha101WireFactory(t, server.URL, key, integrations.NoConditionalAuth(), nil, rig.policy)
	port, err := factory.Open(context.Background(), rig.session)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	entity, _ := domain.ParseEntityID("carrier-only-identity")
	none, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsNone, "", time.Time{})
	tag, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsETag, `"carrier"`, time.Time{})
	for i, sent := range []storage.RemoteCalendarValidators{none, tag, none} {
		attempt, e := port.Prepare(context.Background(), entity)
		if e != nil {
			t.Fatal(e)
		}
		status, body, e := integrations.NHA101ConditionalWireForTest(context.Background(), port, attempt, sent)
		if i == 0 && (e != nil || status != 200 || string(body) != "synthetic calendar bytes") {
			t.Fatal("carrier200", e)
		}
		if i == 1 && (e != nil || status != 304 || len(body) != 0) {
			t.Fatal("carrier304", e)
		}
		if i == 2 && e == nil {
			t.Fatal("conditional carrier accepted206")
		}
	}
}

func TestNHA101ConditionalCarrierLimitsWithoutCacheMigration(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	key, _ := integrations.NewSourceKey("bounded-calendar", "calendar")
	entity, _ := domain.ParseEntityID("bounded-carrier-identity")
	none, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsNone, "", time.Time{})
	for _, scenario := range []string{"decoded-gzip", "headers", "truncated", "429-before-body"} {
		t.Run(scenario, func(t *testing.T) {
			var count atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				switch scenario {
				case "decoded-gzip":
					w.Header().Set("Content-Encoding", "gzip")
					compressed := gzip.NewWriter(w)
					_, _ = compressed.Write([]byte(strings.Repeat("x", (1<<20)+1)))
					_ = compressed.Close()
				case "headers":
					w.Header().Set("X-Oversized", strings.Repeat("x", (32<<10)+1))
					_, _ = w.Write([]byte("unreachable"))
				case "truncated":
					w.Header().Set("Content-Length", "1000")
					_, _ = w.Write([]byte("short"))
				case "429-before-body":
					w.Header().Set("Retry-After", "86400")
					w.WriteHeader(429)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, nha096NoJitter{})
			if err != nil {
				t.Fatal(err)
			}
			factory := nha101WireFactory(t, server.URL, key, integrations.NoConditionalAuth(), nil, policy)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			port, err := factory.Open(ctx, rig.session)
			if err != nil {
				t.Fatal(err)
			}
			defer port.Close()
			attempt, err := port.Prepare(ctx, entity)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = integrations.NHA101ConditionalWireForTest(ctx, port, attempt, none)
			if err == nil || count.Load() < 1 || count.Load() > 3 {
				t.Fatal("conditional carrier limit/retry bound failed", err)
			}
			if scenario == "decoded-gzip" {
				nha096Kind(t, err, integrations.CloudResponseTooLarge)
				if count.Load() != 1 {
					t.Fatal("oversized representation retried")
				}
			}
			if scenario == "429-before-body" {
				nha096Kind(t, err, integrations.CloudQuotaDeferred)
				if count.Load() != 1 {
					t.Fatal("Retry-After retried or waited for body")
				}
			}
		})
	}
}

func TestNHA101ConditionalSQL200304ReopenAndPolicyRejection(t *testing.T) {
	ctx := context.Background()
	store, connection := nha024Store(t)
	private := filepath.Join(t.TempDir(), "private")
	instance := nha016Instance(t, "nha101-conditional")
	key, _ := integrations.NewSourceKey("remote-family", "calendar")
	manifestBytes, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := identities.ConfigureBinding(ctx, instance, key, "calendar.famille")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	modified := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Etag", `"durable"`)
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		w.Header().Set("Cache-Control", "max-age=3600")
		if n == 2 {
			if r.Header.Get("If-None-Match") != `"durable"` || r.Header.Get("If-Modified-Since") != modified.Format(http.TimeFormat) {
				t.Error("reopen did not send durable validators")
			}
			// Omitted Cache-Control on304 must preserve the stored policy.
			w.Header().Del("Cache-Control")
			w.WriteHeader(304)
			return
		}
		if n == 3 {
			w.Header().Set("Etag", `"invalid-body"`)
			_, _ = w.Write([]byte("not ICS"))
			return
		}
		if n == 4 {
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte("new representation must not be persisted"))
			return
		}
		if n == 5 && (r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "") {
			t.Error("RequireFull was not durable")
		}
		_, _ = w.Write([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:stable uid\r\nDTSTART:20261019T120000Z\r\nDURATION:PT30M\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	}))
	defer server.Close()
	open := func(s *storage.Store) (*calendars.Service, integrations.ConditionalHTTP, *fake.Fixture) {
		t.Helper()
		secrets, e := s.BootstrapSecrets(ctx, private)
		if e != nil {
			t.Fatal(e)
		}
		host, fixture, e := fake.NewCommandFixture()
		if e != nil {
			t.Fatal(e)
		}
		if e = host.AttachSecrets(secrets); e != nil {
			t.Fatal(e)
		}
		session, e := host.OpenSession(instance)
		if e != nil {
			t.Fatal(e)
		}
		policy, e := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
		if e != nil {
			t.Fatal(e)
		}
		factory := nha101WireFactory(t, server.URL, key, integrations.NoConditionalAuth(), nil, policy)
		port, e := factory.Open(ctx, session)
		if e != nil {
			t.Fatal(e)
		}
		repo, e := calendars.NewRepository(s, manifest)
		if e != nil {
			t.Fatal(e)
		}
		service, e := calendars.NewService(repo)
		if e != nil {
			t.Fatal(e)
		}
		return service, port, fixture
	}
	service, port, fixture := open(store)
	id, err := service.Bind(ctx, bound.Entity)
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(svc *calendars.Service, p integrations.ConditionalHTTP) (calendars.RemoteWriter, integrations.ConditionalAttempt, calendars.RemoteCacheSnapshot) {
		t.Helper()
		a, e := p.Prepare(ctx, bound.Entity)
		if e != nil {
			t.Fatal(e)
		}
		scope, e := a.Scope()
		if e != nil {
			t.Fatal(e)
		}
		source, e := calendars.NewRemoteSource(scope, "Family")
		if e != nil {
			t.Fatal(e)
		}
		writer, e := svc.ConfigureRemoteSource(ctx, id, source)
		if e != nil {
			t.Fatal(e)
		}
		cache, e := writer.Read(ctx)
		if e != nil {
			t.Fatal(e)
		}
		return writer, a, cache
	}
	writer, attempt, before := prepare(service, port)
	observation, err := port.ObserveConditional(ctx, attempt, before.Selection())
	if err != nil {
		t.Fatal(err)
	}
	if err = port.ConfirmConditionalAdmitted(ctx, observation, storage.RemoteCalendarCommit{}); err == nil {
		t.Fatal("zero receipt confirmedReady")
	}
	admitted, err := observation.StoreObservation()
	if err != nil {
		t.Fatal(err)
	}
	body, err := observation.TakeRepresentationBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, e := observation.TakeRepresentationBytes(); e == nil {
		t.Fatal("body transferred twice")
	}
	if _, e := observation.FreshUntil(); e != nil {
		t.Fatal("taken body lost scalar deadline", e)
	}
	original := body[0]
	body[0] ^= 1
	if _, e := writer.Replace(ctx, before.Calendar().Revision(), admitted, body, nil); e == nil {
		t.Fatal("mutated transferred body admitted")
	}
	body[0] = original
	receipt, err := writer.Replace(ctx, before.Calendar().Revision(), admitted, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = port.ConfirmConditionalAdmitted(ctx, observation, receipt); err != nil {
		t.Fatal(err)
	}
	if err = port.ConfirmConditionalAdmitted(ctx, observation, receipt); err == nil {
		t.Fatal("receipt replay confirmedReady")
	}
	health, e := fixture.Sequencer().Capture()
	if e != nil {
		t.Fatal(e)
	}
	status, _, e := health.Health[instance].Components()
	if e != nil || status != integrations.HealthReady {
		t.Fatal("known admitted receipt failed to confirmReady", e)
	}
	committed, err := writer.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := committed.Digest()
	metadata, err := committed.Selection().Metadata().Encode()
	if err != nil {
		t.Fatal(err)
	}
	events, err := service.ListEvents(ctx, id)
	if err != nil || len(events) != 1 {
		t.Fatal("UID not admitted", err)
	}
	identity := events[0].Key()
	writer.Retire()
	port.Close()
	service.Close()
	store.Close()
	reopened, err := storage.Open(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service, port, fixture = open(reopened)
	defer service.Close()
	defer port.Close()
	writer, attempt, resumed := prepare(service, port)
	defer writer.Retire()
	resumedMeta, err := resumed.Selection().Metadata().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Calendar().Revision() != committed.Calendar().Revision() || !resumed.Validators().Equal(committed.Validators()) || !reflect.DeepEqual(resumedMeta, metadata) || requests.Load() != 1 {
		t.Fatal("reopen changed SQL selection before GET")
	}
	state, _ := resumed.Calendar().RemoteState()
	freshness, _ := state.Freshness()
	if freshness != calendars.RemoteStale {
		t.Fatal("reopened process already fresh")
	}
	observation, err = port.ObserveConditional(ctx, attempt, resumed.Selection())
	if err != nil || observation.Kind() != integrations.ConditionalNotModified304 {
		t.Fatal("real selected304", err)
	}
	admitted, err = observation.StoreObservation()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = writer.Revalidate(ctx, resumed.Calendar().Revision(), admitted, resumed.Validators(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if err = port.ConfirmConditionalAdmitted(ctx, observation, receipt); err != nil {
		t.Fatal(err)
	}
	after, err := writer.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterDigest, _ := after.Digest()
	events, err = service.ListEvents(ctx, id)
	if err != nil || len(events) != 1 || events[0].Key() != identity || afterDigest != digest || after.Calendar().Revision() == committed.Calendar().Revision() {
		t.Fatal("304 changed identity/digest or failed to advance revision", err)
	}
	attempt, err = port.Prepare(ctx, bound.Entity)
	if err != nil {
		t.Fatal(err)
	}
	observation, err = port.ObserveConditional(ctx, attempt, after.Selection())
	if err != nil {
		t.Fatal(err)
	}
	admitted, err = observation.StoreObservation()
	if err != nil {
		t.Fatal(err)
	}
	body, err = observation.TakeRepresentationBytes()
	if err != nil {
		t.Fatal(err)
	}
	failed, err := writer.Replace(ctx, after.Calendar().Revision(), admitted, body, nil)
	if err == nil || failed.Valid() {
		t.Fatal("invalid ICS committed")
	}
	if err = port.RejectConditionalObservation(ctx, observation, integrations.ConditionalRejectContent); err != nil {
		t.Fatal(err)
	}
	if err = writer.Reject(ctx, after.Calendar().Revision(), time.Now(), calendars.RemoteReasonContent, storage.PreserveAdmittedValidators); err != nil {
		t.Fatal(err)
	}
	current, err := writer.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	currentDigest, _ := current.Digest()
	if currentDigest != digest || !current.Validators().Equal(after.Validators()) {
		t.Fatal("invalid body replaced admitted validators/data")
	}
	attempt, err = port.Prepare(ctx, bound.Entity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = port.ObserveConditional(ctx, attempt, current.Selection())
	failure, ok := err.(*integrations.ConditionalError)
	if !ok || failure.Kind() != integrations.ConditionalCachePolicy {
		t.Fatal("no-store was admitted", err)
	}
	if err = writer.Reject(ctx, current.Calendar().Revision(), time.Now(), calendars.RemoteReasonCachePolicy, failure.Disposition()); err != nil {
		t.Fatal(err)
	}
	writer.Retire()
	port.Close()
	service.Close()
	service, port, fixture = open(reopened)
	defer service.Close()
	defer port.Close()
	writer, attempt, current = prepare(service, port)
	defer writer.Retire()
	if current.Validators().Kind() != storage.RemoteValidatorsNone {
		t.Fatal("RequireFull lost after reopen")
	}
	observation, err = port.ObserveConditional(ctx, attempt, current.Selection())
	if err != nil || observation.Kind() != integrations.ConditionalRepresentation200 {
		t.Fatal("normal full poll after refusal", err)
	}
	if err = port.ConfirmConditionalAdmitted(ctx, observation, receipt); err == nil {
		t.Fatal("genuine receipt from an older observation confirmedReady")
	}
	if err = port.RejectConditionalObservation(ctx, observation, integrations.ConditionalRejectPublication); err != nil {
		t.Fatal(err)
	}
	if err = port.ConfirmConditionalAdmitted(ctx, observation, receipt); err == nil {
		t.Fatal("old observation/receipt confirmedReady")
	}
	// A deferred server failure exercises Store's indeterminate outcome. It
	// is not mislabeled an ACK-loss test (the real wire ACK-loss oracle lives
	// in storage's unchanged TestNHA101RemoteDurableCommitLostACKHasNoReceipt).
	if err = reopened.InTransaction(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `CREATE FUNCTION public.nha101_http_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred commit failure'; END $$; CREATE CONSTRAINT TRIGGER nha101_http_commit_failure AFTER UPDATE ON public.remote_calendar_cache DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha101_http_commit_failure()`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err = port.Prepare(ctx, bound.Entity)
	if err != nil {
		t.Fatal(err)
	}
	observation, err = port.ObserveConditional(ctx, attempt, current.Selection())
	if err != nil {
		t.Fatal(err)
	}
	admitted, err = observation.StoreObservation()
	if err != nil {
		t.Fatal(err)
	}
	body, err = observation.TakeRepresentationBytes()
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := writer.Replace(ctx, current.Calendar().Revision(), admitted, body, nil)
	if failure, ok := err.(*calendars.Error); !ok || failure.Code() != calendars.CommitUnknown || unknown.Valid() {
		t.Fatal("indeterminate commit returned a receipt", err)
	}
	if err = port.RejectConditionalObservation(ctx, observation, integrations.ConditionalRejectCommitUnknown); err != nil {
		t.Fatal(err)
	}
	if err = port.ConfirmConditionalAdmitted(ctx, observation, unknown); err == nil {
		t.Fatal("unknown receipt confirmedReady")
	}
	health, e = fixture.Sequencer().Capture()
	if e != nil {
		t.Fatal(e)
	}
	status, _, e = health.Health[instance].Components()
	if e != nil || status != integrations.HealthDegraded {
		t.Fatal("unknown commit confirmedReady", e)
	}
}

func TestNHA101ConditionalSecretRotationAndRedactionWithoutCacheMigration(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	ctx := context.Background()
	key, _ := integrations.NewSourceKey("secret-calendar", "calendar")
	name, _ := storage.ParseSecretName("remote-url")
	handle, err := storage.NewSecretHandle(rig.instance, name)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer NHA101_AUTH_CANARY" || r.URL.RawQuery != "token=NHA101_URL_CANARY_ONE" {
			t.Error("actual secret URL/query or bearer admission changed")
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	firstURL := "http://calendar.invalid:" + u.Port() + "/private?token=NHA101_URL_CANARY_ONE"
	value, err := storage.NewSecretValue([]byte(firstURL))
	if err != nil {
		t.Fatal(err)
	}
	if err = rig.repo.RotateCredential(ctx, handle, value); err != nil {
		t.Fatal(err)
	}
	resource, err := integrations.SecretConditionalResource(handle)
	if err != nil {
		t.Fatal(err)
	}
	tokenName, _ := storage.ParseSecretName("remote-bearer")
	tokenHandle, err := storage.NewSecretHandle(rig.instance, tokenName)
	if err != nil {
		t.Fatal(err)
	}
	tokenValue, err := storage.NewSecretValue([]byte("NHA101_AUTH_CANARY"))
	if err != nil {
		t.Fatal(err)
	}
	if err = rig.repo.RotateCredential(ctx, tokenHandle, tokenValue); err != nil {
		t.Fatal(err)
	}
	auth, err := integrations.BearerConditionalAuth(tokenHandle)
	if err != nil {
		t.Fatal(err)
	}
	factory := nha101WireFactory(t, server.URL, key, auth, &resource, rig.policy)
	port, err := factory.Open(ctx, rig.session)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	entity, _ := domain.ParseEntityID("secret-resource-identity")
	attempt, err := port.Prepare(ctx, entity)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := attempt.Scope()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]any{factory, port, attempt, resource, scope})
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := string(raw) + fmt.Sprintf("%#v %+v", port, attempt)
	if strings.Contains(diagnostic, "NHA101_URL_CANARY") || strings.Contains(diagnostic, "NHA101_AUTH_CANARY") {
		t.Fatal("URL/auth secret leaked through diagnostics")
	}
	none, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsNone, "", time.Time{})
	if status, _, e := integrations.NHA101ConditionalWireForTest(ctx, port, attempt, none); e != nil || status != 200 {
		t.Fatal("actual secret URL/bearer transport failed", e)
	}
	if err = port.Invalidate(ctx, integrations.ConditionalRejectReconfigured); err != nil {
		t.Fatal(err)
	}
	value, err = storage.NewSecretValue([]byte(strings.Replace(firstURL, "ONE", "TWO", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err = rig.repo.RotateCredential(ctx, handle, value); err != nil {
		t.Fatal(err)
	}
	next, err := port.Prepare(ctx, entity)
	if err != nil {
		t.Fatal(err)
	}
	nextScope, err := next.Scope()
	if err != nil {
		t.Fatal(err)
	}
	oldKey, _ := scope.Key()
	newKey, _ := nextScope.Key()
	if oldKey == newKey {
		t.Fatal("same-handle authenticated rotation reused scope")
	}
}
