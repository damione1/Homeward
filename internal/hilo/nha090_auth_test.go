package hilo

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha090B2CTransport struct {
	mu        sync.Mutex
	mode      string
	state     string
	challenge string
	calls     []string
}

func (f *nha090B2CTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if request.URL.Scheme != "https" || request.URL.Host != "connexion.hiloenergie.com" {
		return nil, fmt.Errorf("synthetic B2C rejected an unexpected destination")
	}
	path := request.URL.Path
	f.calls = append(f.calls, path)
	status := http.StatusOK
	header := make(http.Header)
	var body string
	switch {
	case strings.HasSuffix(path, "/authorize"):
		query := request.URL.Query()
		f.state, f.challenge = query.Get("state"), query.Get("code_challenge")
		if request.Method != http.MethodGet || query.Get("client_id") != clientID || query.Get("scope") != clientScope || query.Get("redirect_uri") != redirectURI || query.Get("code_challenge_method") != "S256" || f.state == "" || f.challenge == "" {
			return nil, fmt.Errorf("synthetic B2C authorize lost its pinned client, state, or PKCE")
		}
		status = http.StatusFound
		header.Set("Location", b2cOrigin+"/HiloDirectoryB2C.onmicrosoft.com/page?flow=synthetic")
	case strings.HasSuffix(path, "/page"):
		if request.Method != http.MethodGet {
			return nil, fmt.Errorf("synthetic B2C page method")
		}
		header.Set("Set-Cookie", "synthetic_session=one; Path=/; Secure; HttpOnly")
		body = `<html><script>var SETTINGS = {"csrf":"synthetic-csrf","transId":"synthetic-transaction","api":"CombinedSigninAndSignup","hosts":{"tenant":"/HiloDirectoryB2C.onmicrosoft.com/B2C_1A_SIGN_IN"}};</script></html>`
	case strings.HasSuffix(path, "/SelfAsserted"):
		if request.Method != http.MethodPost || request.Header.Get("Cookie") != "synthetic_session=one" || request.Header.Get("X-CSRF-TOKEN") != "synthetic-csrf" || request.Header.Get("Origin") != b2cOrigin || !strings.HasSuffix(request.Header.Get("Referer"), "/page?flow=synthetic") {
			return nil, fmt.Errorf("synthetic B2C credentials lost bounded cookie, CSRF or same-origin referer")
		}
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(payload))
		if err != nil || form.Get("signInName") != "synthetic@example.test" || form.Get("password") != "synthetic-password" || form.Get("request_type") != "RESPONSE" {
			return nil, fmt.Errorf("synthetic B2C credentials changed shape")
		}
		if f.mode == "mfa" {
			body = `{"status":"requiresMFA"}`
		} else {
			body = `{"status":"200"}`
		}
	case strings.HasSuffix(path, "/confirmed"):
		if request.Method != http.MethodGet || request.Header.Get("Cookie") != "synthetic_session=one" {
			return nil, fmt.Errorf("synthetic B2C confirmation lost its session cookie")
		}
		status = http.StatusFound
		state := f.state
		if f.mode == "wrong-state" {
			state = "unrelated-state"
		}
		callback := url.Values{"code": {"synthetic-code"}, "state": {state}}
		if f.mode == "oauth-error" {
			callback = url.Values{"error": {"access_denied"}, "state": {state}}
		}
		header.Set("Location", redirectURI+"?"+callback.Encode())
	case strings.HasSuffix(path, "/token"):
		if request.Method != http.MethodPost || f.mode != "success" {
			return nil, fmt.Errorf("synthetic B2C token exchange occurred outside success")
		}
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(payload))
		if err != nil || form.Get("grant_type") != "authorization_code" || form.Get("code") != "synthetic-code" || form.Get("client_id") != clientID || form.Get("redirect_uri") != redirectURI || form.Get("scope") != clientScope {
			return nil, fmt.Errorf("synthetic B2C code exchange changed shape")
		}
		challenge := sha256.Sum256([]byte(form.Get("code_verifier")))
		if form.Get("code_verifier") == "" || base64.RawURLEncoding.EncodeToString(challenge[:]) != f.challenge {
			return nil, fmt.Errorf("synthetic B2C PKCE verifier did not match the authorize challenge")
		}
		body = `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","id_token":"discard-this-identity","expires_in":3600,"token_type":"Bearer"}`
	default:
		return nil, fmt.Errorf("synthetic B2C rejected an unknown step")
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
}

func (f *nha090B2CTransport) configure(mode string) {
	f.mu.Lock()
	f.mode, f.calls = mode, nil
	f.mu.Unlock()
}

func (f *nha090B2CTransport) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestNHA090AuthorizationProviderUsesBoundedB2CExchanger(t *testing.T) {
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "synthetic-secrets"))
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachSecrets(secrets); err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha090-b2c-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	name, err := storage.ParseSecretName("hilo-credential")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := storage.NewSecretHandle(instance, name)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	account, err := integrations.ParseCloudAccountID("synthetic-account")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := integrations.NewCloudAccountBinding(instance, account)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), []integrations.CloudAccountBinding{binding}, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &nha090B2CTransport{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		owned := request.Clone(request.Context())
		owned.URL.Scheme, owned.URL.Host = "https", request.Host
		response, roundTripErr := fixture.RoundTrip(owned)
		if roundTripErr != nil {
			t.Errorf("synthetic B2C exchange: %v", roundTripErr)
			http.Error(w, "synthetic B2C rejected request", http.StatusBadRequest)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "127.0.0.1"},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "connexion.hiloenergie.com:443" {
				return nil, fmt.Errorf("synthetic B2C refused a nonfixture network destination")
			}
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	client, err := integrations.NewCloudClient(session, policy, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profile, err := integrations.ParseCloudAuthorizationProfileID("hilo-android-b2c-v1")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewAuthorizationProvider(profile)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := integrations.NewCloudAuthorizationOwner(host, client, policy, handle, profile, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		t.Fatal(err)
	}
	inputSecret, err := storage.NewSecretValue([]byte(`{"email":"synthetic@example.test","password":"synthetic-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	input, err := integrations.NewCloudAuthorizationInput(profile, inputSecret)
	if err != nil {
		t.Fatal(err)
	}
	assertAbsent := func() {
		t.Helper()
		_, _, readErr := secrets.ReadCredentialVersioned(ctx, handle)
		var storageErr *storage.Error
		if !errors.As(readErr, &storageErr) || storageErr.Kind() != storage.SecretNotFound {
			t.Fatalf("B2C provider wrote a credential before complete authorization: %v", readErr)
		}
	}
	for _, mode := range []string{"mfa", "wrong-state", "oauth-error"} {
		fixture.configure(mode)
		attempt, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, authorizeErr := provider.Authorize(ctx, attempt.Exchanger(), input)
		attempt.Close()
		calls := fixture.recorded()
		if !errors.Is(authorizeErr, ErrReauthorizationRequired) || len(calls) > 5 {
			t.Fatalf("unexpected B2C %s was accepted or unbounded: err=%v calls=%d", mode, authorizeErr, len(calls))
		}
		for _, step := range calls {
			if strings.HasSuffix(step, "/token") {
				t.Fatalf("B2C %s exchanged a code after rejection", mode)
			}
		}
		assertAbsent()
	}
	fixture.configure("success")
	attempt, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.Authorize(ctx, attempt.Exchanger(), input)
	if err != nil {
		attempt.Close()
		t.Fatal(err)
	}
	calls := fixture.recorded()
	if len(calls) != 5 || !strings.HasSuffix(calls[4], "/token") {
		attempt.Close()
		t.Fatalf("B2C success did not use five bounded exchanges: %v", calls)
	}
	assertAbsent()
	if err := attempt.Publish(ctx, credential); err != nil {
		attempt.Close()
		t.Fatal(err)
	}
	attempt.Close()
	stored, _, err := secrets.ReadCredentialVersioned(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := stored.Bytes()
	if err != nil || strings.Contains(string(raw), "discard-this-identity") {
		t.Fatal("the B2C id_token escaped into the runtime credential")
	}
	decoded, err := decodeCredential(stored)
	if err != nil || decoded.Access != "synthetic-access" || decoded.Refresh != "synthetic-refresh" || decoded.ExpiresAt.Before(time.Now().Add(59*time.Minute)) {
		t.Fatal("published opaque B2C credential was not admitted")
	}
}
