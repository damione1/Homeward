package integrations_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

func nha177Profile(t *testing.T, raw string) integrations.CloudAuthorizationProfileID {
	t.Helper()
	profile, err := integrations.ParseCloudAuthorizationProfileID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
func nha177ExchangeRequest(t *testing.T, id, target string, expected integrations.CloudAuthorizationExpected) integrations.CloudAuthorizationRequest {
	t.Helper()
	exchangeID, err := integrations.ParseCloudAuthorizationExchangeID(id)
	if err != nil {
		t.Fatal(err)
	}
	request, err := integrations.NewCloudAuthorizationRequest(exchangeID, http.MethodGet, target, nil, nil, expected)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestNHA177FiniteExchangeCookiesAndOneShot(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	var requests atomic.Int32
	var callback atomic.Int32
	var apiCookie, outsideCookie atomic.Value
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/auth":
			w.Header().Add("Set-Cookie", "sid=private-cookie; Path=/api; Secure; HttpOnly; Max-Age=30")
			w.Header().Set("Location", "https://salc.hiloenergie.com/login-callback?code=private-code")
			w.WriteHeader(http.StatusFound)
		case "/api/step":
			apiCookie.Store(r.Header.Get("Cookie"))
			_, _ = w.Write([]byte("private-body"))
		case "/outside":
			outsideCookie.Store(r.Header.Get("Cookie"))
			_, _ = w.Write([]byte("outside"))
		default:
			callback.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	admission := &nha091Admission{limit: 10}
	transport := server.Client().Transport.(*http.Transport)
	client, err := integrations.NewCloudClient(rig.session, rig.policy, transport, admission)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	handle := nha096Handle(t, rig.instance, "nha177-finite")
	owner, err := integrations.NewCloudAuthorizationOwner(rig.host, client, rig.policy, handle, nha177Profile(t, "hilo-opaque-v1"), integrations.DefaultCloudAuthorizationLimits(), admission)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()
	exchanger := attempt.Exchanger()
	if _, ok := exchanger.(interface {
		Publish(context.Context, integrations.CloudAuthorizedCredential) error
	}); ok {
		t.Fatal("provider exchanger gained publication capability")
	}
	redirect, err := exchanger.Exchange(context.Background(), nha177ExchangeRequest(t, "authorize", server.URL+"/auth", integrations.CloudAuthorizationExpectedRedirect))
	if err != nil {
		t.Fatal(err)
	}
	location, present := redirect.Location()
	if !present || !strings.Contains(location, "private-code") || redirect.StatusCode() != http.StatusFound {
		t.Fatal("302 Location was not captured")
	}
	cookies := redirect.Cookies()
	if len(cookies) != 1 || cookies[0].Name() != "sid" || cookies[0].Value() != "private-cookie" ||
		cookies[0].Path() != "/api" || !cookies[0].Secure() || !cookies[0].HostOnly() || cookies[0].Deleted() {
		t.Fatal("cookie scope or metadata lost")
	}
	for _, diagnostic := range []string{fmt.Sprint(redirect), fmt.Sprintf("%#v", cookies[0])} {
		if strings.Contains(diagnostic, "private-") {
			t.Fatal("response diagnostic leaked secret")
		}
	}
	_, err = exchanger.Exchange(context.Background(), nha177ExchangeRequest(t, "authorize", server.URL+"/auth", integrations.CloudAuthorizationExpectedRedirect))
	nha096Kind(t, err, integrations.CloudInvalid)
	if requests.Load() != 1 || admission.count.Load() != 1 || callback.Load() != 0 {
		t.Fatal("duplicate entered transport or callback was followed")
	}
	body, err := exchanger.Exchange(context.Background(), nha177ExchangeRequest(t, "api", server.URL+"/api/step", integrations.CloudAuthorizationExpectedOK))
	if err != nil {
		t.Fatal(err)
	}
	if string(body.Bytes()) != "private-body" || apiCookie.Load() != "sid=private-cookie" {
		t.Fatal("scoped cookie was not sent")
	}
	copyBytes := body.Bytes()
	copyBytes[0] = 'X'
	if string(body.Bytes()) != "private-body" {
		t.Fatal("response bytes alias caller memory")
	}
	_, err = exchanger.Exchange(context.Background(), nha177ExchangeRequest(t, "outside", server.URL+"/outside", integrations.CloudAuthorizationExpectedOK))
	if err != nil {
		t.Fatal(err)
	}
	if outsideCookie.Load() != "" || requests.Load() != 3 || admission.count.Load() != 3 {
		t.Fatal("cookie leaked outside path or wrong attempt count")
	}
}

func TestNHA177StatusHeaderAndQuotaFences(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/rate":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/flood":
			w.Header().Set("X-Flood", strings.Repeat("x", 33<<10))
			w.WriteHeader(http.StatusOK)
		case "/small-flood":
			w.Header().Set("X-Flood", strings.Repeat("x", 1024))
			w.WriteHeader(http.StatusOK)
		case "/bad-redirect":
			w.Header().Add("Location", "https://example.test/one")
			w.Header().Add("Location", "https://example.test/two")
			w.WriteHeader(http.StatusFound)
		case "/switch":
			w.WriteHeader(http.StatusSwitchingProtocols)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	admission := &nha091Admission{limit: 20}
	client, err := integrations.NewCloudClient(rig.session, rig.policy, server.Client().Transport.(*http.Transport), admission)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	handle := nha096Handle(t, rig.instance, "nha177-fences")
	owner, err := integrations.NewCloudAuthorizationOwner(rig.host, client, rig.policy, handle, nha177Profile(t, "test"), integrations.DefaultCloudAuthorizationLimits(), admission)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()
	exchanger := attempt.Exchanger()
	_, err = exchanger.Exchange(context.Background(), nha177ExchangeRequest(t, "bad-redirect", server.URL+"/bad-redirect", integrations.CloudAuthorizationExpectedRedirect))
	nha096Kind(t, err, integrations.CloudInvalid)
	_, err = exchanger.Exchange(context.Background(), nha177ExchangeRequest(t, "switch", server.URL+"/switch", integrations.CloudAuthorizationExpectedOKOrRedirect))
	nha096Kind(t, err, integrations.CloudHTTPFailed)
	_, err = exchanger.Exchange(context.Background(), nha177ExchangeRequest(t, "flood", server.URL+"/flood", integrations.CloudAuthorizationExpectedOK))
	if err == nil || strings.Contains(fmt.Sprint(err), strings.Repeat("x", 20)) {
		t.Fatal("header flood admitted or leaked")
	}
	strictTransport := server.Client().Transport.(*http.Transport).Clone()
	strictTransport.MaxResponseHeaderBytes = 512
	strictClient, err := integrations.NewCloudClient(rig.session, rig.policy, strictTransport, admission)
	if err != nil {
		t.Fatal(err)
	}
	defer strictClient.Close()
	strictHandle := nha096Handle(t, rig.instance, "nha177-strict-header")
	strictOwner, err := integrations.NewCloudAuthorizationOwner(rig.host, strictClient, rig.policy, strictHandle, nha177Profile(t, "test"), integrations.DefaultCloudAuthorizationLimits(), admission)
	if err != nil {
		t.Fatal(err)
	}
	strictAttempt, err := strictOwner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer strictAttempt.Close()
	_, err = strictAttempt.Exchanger().Exchange(context.Background(), nha177ExchangeRequest(t, "strict-flood", server.URL+"/small-flood", integrations.CloudAuthorizationExpectedOK))
	nha096Kind(t, err, integrations.CloudTransportFailed)
	_, err = exchanger.Exchange(context.Background(), nha177ExchangeRequest(t, "rate", server.URL+"/rate", integrations.CloudAuthorizationExpectedOK))
	nha096Kind(t, err, integrations.CloudQuotaDeferred)
	before := requests.Load()
	observation, err := integrations.NewCloudRequest(http.MethodGet, server.URL+"/ok", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Observe(context.Background(), observation)
	nha096Kind(t, err, integrations.CloudQuotaDeferred)
	if requests.Load() != before {
		t.Fatal("429 did not defer Observe on the shared account scope")
	}
}

func TestNHA177PublicationAbsentRevisionAndProfile(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	profile := nha177Profile(t, "hilo-opaque-v1")
	other := nha177Profile(t, "other-profile")
	handle := nha096Handle(t, rig.instance, "nha177-publish")
	owner, err := integrations.NewCloudAuthorizationOwner(rig.host, rig.client, rig.policy, handle, profile, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := storage.NewSecretValue([]byte("private-initial-token"))
	wrong, _ := integrations.NewCloudAuthorizedCredential(other, secret)
	nha096Kind(t, attempt.Publish(context.Background(), wrong), integrations.CloudInvalid)
	attempt.Close()
	attempt, err = owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := integrations.NewCloudAuthorizedCredential(profile, secret)
	if err := attempt.Publish(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	nha096Kind(t, attempt.Publish(context.Background(), credential), integrations.CloudSessionInactive)
	attempt.Close()
	stored, revision, err := rig.repo.ReadCredentialVersioned(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := stored.Bytes()
	if string(raw) != "private-initial-token" {
		t.Fatal("initial credential was not committed")
	}
	attempt, err = owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manual, _ := storage.NewSecretValue([]byte("manual-winner"))
	if err := rig.repo.RotateCredentialIfRevision(context.Background(), handle, revision, manual); err != nil {
		t.Fatal(err)
	}
	nha096Kind(t, attempt.Publish(context.Background(), credential), integrations.CloudCredentialConflict)
	attempt.Close()
	current, _, err := rig.repo.ReadCredentialVersioned(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	currentRaw, _ := current.Bytes()
	if string(currentRaw) != "manual-winner" {
		t.Fatal("stale authorization overwrote manual rotation")
	}

	absentHandle := nha096Handle(t, rig.instance, "nha177-racing-insert")
	second, err := integrations.NewCloudAuthorizationOwner(rig.host, rig.client, rig.policy, absentHandle, profile, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		t.Fatal(err)
	}
	absent, err := second.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := rig.repo.InitializeCredentialIfAbsent(context.Background(), absentHandle, manual)
	if err != nil || !inserted {
		t.Fatal("fixture insertion failed")
	}
	nha096Kind(t, absent.Publish(context.Background(), credential), integrations.CloudCredentialConflict)
	absent.Close()
	persisted, _, err := rig.repo.ReadCredentialVersioned(context.Background(), absentHandle)
	if err != nil {
		t.Fatal(err)
	}
	persistedRaw, _ := persisted.Bytes()
	if string(persistedRaw) != "manual-winner" {
		t.Fatal("stale initial authorization overwrote concurrent insertion")
	}

	encoded, _ := json.Marshal(credential)
	if strings.Contains(string(encoded), "private-initial-token") {
		t.Fatal("credential JSON leaked secret")
	}
}

func TestNHA177AttemptLimitAndCancellation(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	handle := nha096Handle(t, rig.instance, "nha177-limit")
	limits, err := integrations.AdmitCloudAuthorizationLimits(integrations.CloudAuthorizationLimitsInput{
		TotalTimeout: 200 * time.Millisecond, MaxExchanges: 1, MaxActive: 1, MaxPerHandle: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := integrations.NewCloudAuthorizationOwner(rig.host, rig.client, rig.policy, handle, nha177Profile(t, "test"), limits)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Begin(context.Background())
	nha096Kind(t, err, integrations.CloudQueueFull)
	<-time.After(220 * time.Millisecond)
	_, err = attempt.Exchanger().Exchange(context.Background(), nha177ExchangeRequest(t, "late", "https://example.test/", integrations.CloudAuthorizationExpectedOK))
	nha096Kind(t, err, integrations.CloudBudgetExhausted)
	attempt.Close()
	next, err := owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestNHA177PublicationCommitUnknown(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	ctx := context.Background()
	profile := nha177Profile(t, "synthetic-commit-unknown")
	handle := nha096Handle(t, rig.instance, "nha177-commit-unknown")
	owner, err := integrations.NewCloudAuthorizationOwner(rig.host, rig.client, rig.policy, handle, profile, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		t.Fatal(err)
	}
	old, _ := storage.NewSecretValue([]byte("private-old-credential"))
	if err := rig.repo.RotateCredential(ctx, handle, old); err != nil {
		t.Fatal(err)
	}
	attempt, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()
	// A deferred failure occurs after the CAS statement but at the real commit
	// boundary. The attempt must never report a successful publication or retry.
	if err := rig.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION public.nha177_refuse_secret_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic commit refusal'; END $$;
			CREATE CONSTRAINT TRIGGER nha177_refuse_secret_commit AFTER UPDATE ON public.integration_secrets DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha177_refuse_secret_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	newValue, _ := storage.NewSecretValue([]byte("private-new-credential"))
	credential, err := integrations.NewCloudAuthorizedCredential(profile, newValue)
	if err != nil {
		t.Fatal(err)
	}
	publicationErr := attempt.Publish(ctx, credential)
	nha096Kind(t, publicationErr, integrations.CloudCredentialCommitUnknown)
	if strings.Contains(fmt.Sprint(publicationErr), "private-") {
		t.Fatal("ambiguous result leaked credential")
	}
	nha096Kind(t, attempt.Publish(ctx, credential), integrations.CloudSessionInactive)
	stored, _, err := rig.repo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := stored.Bytes()
	if string(raw) != "private-old-credential" {
		t.Fatal("failed commit changed durable credential")
	}
}

func TestNHA177IntegrityFailureIsNotAbsent(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	ctx := context.Background()
	profile := nha177Profile(t, "synthetic-integrity")
	handle := nha096Handle(t, rig.instance, "nha177-corrupt-credential")
	owner, err := integrations.NewCloudAuthorizationOwner(rig.host, rig.client, rig.policy, handle, profile, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		t.Fatal(err)
	}
	value, _ := storage.NewSecretValue([]byte("private-existing-credential"))
	if err := rig.repo.RotateCredential(ctx, handle, value); err != nil {
		t.Fatal(err)
	}
	instance, name, err := handle.Components()
	if err != nil {
		t.Fatal(err)
	}
	instanceText, _ := instance.Value()
	nameText, _ := name.Value()
	if err := rig.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.integration_secrets SET ciphertext=set_byte(ciphertext, 13, get_byte(ciphertext, 13) # 1) WHERE instance_id=$1 AND secret_name=$2`, instanceText, nameText)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := owner.Begin(ctx)
	if attempt != nil {
		attempt.Close()
		t.Fatal("corrupt credential became an absent publication attempt")
	}
	nha096Kind(t, err, integrations.CloudCredentialFailed)
}

func TestNHA177RetirementAtPublicationCommitBarrier(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	ctx := context.Background()
	profile := nha177Profile(t, "synthetic-retired-commit")
	handle := nha096Handle(t, rig.instance, "nha177-retired-commit")
	owner, err := integrations.NewCloudAuthorizationOwner(rig.host, rig.client, rig.policy, handle, profile, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		t.Fatal(err)
	}
	old, _ := storage.NewSecretValue([]byte("private-old-credential"))
	if err := rig.repo.RotateCredential(ctx, handle, old); err != nil {
		t.Fatal(err)
	}
	attempt, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()
	if err := rig.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION public.nha177_secret_commit_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(481317, 177); RETURN NEW; END $$;
			CREATE CONSTRAINT TRIGGER nha177_secret_commit_barrier AFTER UPDATE ON public.integration_secrets DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha177_secret_commit_barrier()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	lockerDone := make(chan error, 1)
	go func() {
		lockerDone <- rig.store.InTransaction(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481317, 177)`); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	nha096Wait(t, held)
	newValue, _ := storage.NewSecretValue([]byte("private-new-credential"))
	credential, err := integrations.NewCloudAuthorizedCredential(profile, newValue)
	if err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	go func() { committed <- attempt.Publish(ctx, credential) }()
	waiting := false
	deadline := time.After(4 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !waiting {
		select {
		case <-tick.C:
			if err := rig.store.InTransaction(ctx, func(tx pgx.Tx) error {
				var count int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND pid<>pg_backend_pid()`).Scan(&count); err != nil {
					return err
				}
				waiting = count > 0
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		case err := <-committed:
			t.Fatalf("publication completed before commit barrier: %v", err)
		case <-deadline:
			t.Fatal("publication never reached PostgreSQL commit barrier")
		}
	}
	if err := rig.host.Revoke(rig.session); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := nha096Wait(t, lockerDone); err != nil {
		t.Fatal(err)
	}
	publicationErr := nha096Wait(t, committed)
	var cloudErr *integrations.CloudError
	if !errors.As(publicationErr, &cloudErr) || (cloudErr.Kind() != integrations.CloudCredentialCommitUnknown && cloudErr.Kind() != integrations.CloudCredentialCommittedRetired) {
		t.Fatalf("retired commit was visibly successful or lost ambiguity: %v", publicationErr)
	}
	nha096Kind(t, attempt.Publish(ctx, credential), integrations.CloudBudgetExhausted)
	stored, _, err := rig.repo.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := stored.Bytes()
	if string(raw) != "private-old-credential" && string(raw) != "private-new-credential" {
		t.Fatal("unexpected durable credential after ambiguous commit")
	}
}
