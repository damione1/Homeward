// Package hilo ports the pinned Home Assistant/python-hilo observation rules
// and the public-client B2C protocol without embedding either runtime.
package hilo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

const (
	clientID      = "fd8e2de8-8ee3-4871-9812-397023e7638b"
	clientScope   = "openid offline_access https://HiloDirectoryB2C.onmicrosoft.com/hiloapis/user_impersonation"
	b2cPolicy     = "B2C_1A_SIGN_IN"
	redirectURI   = "https://salc.hiloenergie.com/login-callback"
	b2cOrigin     = "https://connexion.hiloenergie.com"
	authorizePath = "/HiloDirectoryB2C.onmicrosoft.com/oauth2/v2.0/authorize"
	tokenPath     = "/HiloDirectoryB2C.onmicrosoft.com/oauth2/v2.0/token"
)

var ErrReauthorizationRequired = errors.New("hilo: reauth_required")
var ErrAuthorizationProtocol = errors.New("hilo: authorization protocol rejected")
var settingsPattern = regexp.MustCompile(`(?s)var SETTINGS\s*=\s*(\{.*?\});`)

// AuthorizationProvider is registered by trusted Run composition. The input
// secret is a single-use JSON pair; it is never a runtime login fallback.
type AuthorizationProvider struct {
	profile integrations.CloudAuthorizationProfileID
	origin  string
}

func NewAuthorizationProvider(profile integrations.CloudAuthorizationProfileID) (AuthorizationProvider, error) {
	if !profile.Valid() {
		return AuthorizationProvider{}, ErrAuthorizationProtocol
	}
	return AuthorizationProvider{profile: profile, origin: b2cOrigin}, nil
}

type authorizationInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (authorizationInput) String() string                 { return "[REDACTED HILO AUTHORIZATION INPUT]" }
func (v authorizationInput) GoString() string             { return v.String() }
func (v authorizationInput) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

type b2cSettings struct {
	CSRF        string `json:"csrf"`
	Transaction string `json:"transId"`
	API         string `json:"api"`
	Hosts       struct {
		Tenant string `json:"tenant"`
	} `json:"hosts"`
}

func (p AuthorizationProvider) Authorize(ctx context.Context, exchange integrations.CloudAuthorizationExchanger, input integrations.CloudAuthorizationInput) (integrations.CloudAuthorizedCredential, error) {
	if nilInterface(ctx) || nilInterface(exchange) || !input.Profile().Equal(p.profile) || p.origin == "" {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	secret, err := input.Secret()
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	raw, err := secret.Bytes()
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	var credentials authorizationInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&credentials) != nil || credentials.Email == "" || credentials.Password == "" || len(credentials.Email) > 320 || len(credentials.Password) > 1024 || strings.ContainsAny(credentials.Email, "\r\n") {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	state, err := randomURLToken()
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	verifier, err := randomURLToken()
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	nonce, err := randomURLToken()
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "scope": {clientScope}, "response_type": {"code"}, "response_mode": {"query"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "state": {state}, "nonce": {nonce}, "p": {b2cPolicy}}
	authorizeURL := p.origin + authorizePath + "?" + q.Encode()
	page, err := authorizationExchange(ctx, exchange, "authorize", http.MethodGet, authorizeURL, nil, nil, integrations.CloudAuthorizationExpectedOKOrRedirect)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	// The bounded exchanger never follows redirects. Only a same-origin B2C
	// page may be followed, and each hop consumes a separate exchange slot.
	pageURL := authorizeURL
	for hop := 0; hop < 2 && page.StatusCode() == http.StatusFound; hop++ {
		location, ok := page.Location()
		if !ok || !p.allowedB2CRedirect(location) {
			return integrations.CloudAuthorizedCredential{}, ErrReauthorizationRequired
		}
		pageURL = location
		page, err = authorizationExchange(ctx, exchange, "page-"+string(rune('1'+hop)), http.MethodGet, pageURL, nil, nil, integrations.CloudAuthorizationExpectedOKOrRedirect)
		if err != nil {
			return integrations.CloudAuthorizedCredential{}, err
		}
	}
	if page.StatusCode() != http.StatusOK {
		return integrations.CloudAuthorizedCredential{}, ErrReauthorizationRequired
	}
	settings, err := parseSettings(page.Bytes())
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	base := p.origin + settings.Hosts.Tenant
	form := url.Values{"request_type": {"RESPONSE"}, "signInName": {credentials.Email}, "password": {credentials.Password}}
	headers := http.Header{"Content-Type": {"application/x-www-form-urlencoded; charset=UTF-8"}, "X-CSRF-TOKEN": {settings.CSRF}, "X-Requested-With": {"XMLHttpRequest"}, "Origin": {p.origin}, "Referer": {pageURL}}
	submit := base + "/SelfAsserted?" + url.Values{"tx": {settings.Transaction}, "p": {b2cPolicy}}.Encode()
	response, err := authorizationExchange(ctx, exchange, "credentials", http.MethodPost, submit, headers, []byte(form.Encode()), integrations.CloudAuthorizationExpectedOK)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(response.Bytes(), &result) != nil || result.Status != "200" {
		return integrations.CloudAuthorizedCredential{}, ErrReauthorizationRequired
	}
	confirm := base + "/api/" + url.PathEscape(settings.API) + "/confirmed?" + url.Values{"csrf_token": {settings.CSRF}, "tx": {settings.Transaction}, "p": {b2cPolicy}}.Encode()
	response, err = authorizationExchange(ctx, exchange, "confirm", http.MethodGet, confirm, http.Header{"Referer": {pageURL}}, nil, integrations.CloudAuthorizationExpectedRedirect)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	callback, ok := response.Location()
	if !ok {
		return integrations.CloudAuthorizedCredential{}, ErrReauthorizationRequired
	}
	code, err := checkedCallback(callback, state)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	tokenForm := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "redirect_uri": {redirectURI}, "code": {code}, "code_verifier": {verifier}, "scope": {clientScope}}
	response, err = authorizationExchange(ctx, exchange, "code", http.MethodPost, p.origin+tokenPath+"?p="+b2cPolicy, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(tokenForm.Encode()), integrations.CloudAuthorizationExpectedOK)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	credential, err := decodeTokenResponse(response.Bytes(), "", time.Now().UTC())
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	value, err := credential.secretValue()
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, ErrAuthorizationProtocol
	}
	return integrations.NewCloudAuthorizedCredential(p.profile, value)
}

func authorizationExchange(ctx context.Context, exchange integrations.CloudAuthorizationExchanger, id, method, endpoint string, headers http.Header, body []byte, expected integrations.CloudAuthorizationExpected) (integrations.CloudAuthorizationResponse, error) {
	exchangeID, err := integrations.ParseCloudAuthorizationExchangeID(id)
	if err != nil {
		return integrations.CloudAuthorizationResponse{}, ErrAuthorizationProtocol
	}
	request, err := integrations.NewCloudAuthorizationRequest(exchangeID, method, endpoint, headers, body, expected)
	if err != nil {
		return integrations.CloudAuthorizationResponse{}, ErrAuthorizationProtocol
	}
	response, err := exchange.Exchange(ctx, request)
	if err != nil {
		return integrations.CloudAuthorizationResponse{}, ErrReauthorizationRequired
	}
	return response, nil
}

func randomURLToken() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func (p AuthorizationProvider) allowedB2CRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.RawQuery == "" || !strings.HasPrefix(raw, p.origin+"/") {
		return false
	}
	base, err := url.Parse(p.origin)
	return err == nil && u.Host == base.Host && strings.HasPrefix(u.Path, "/HiloDirectoryB2C.onmicrosoft.com/")
}

func parseSettings(page []byte) (b2cSettings, error) {
	if len(page) > 1<<20 {
		return b2cSettings{}, ErrReauthorizationRequired
	}
	match := settingsPattern.FindSubmatch(page)
	if len(match) != 2 {
		return b2cSettings{}, ErrReauthorizationRequired
	}
	var result b2cSettings
	if json.Unmarshal(match[1], &result) != nil || result.CSRF == "" || result.Transaction == "" || result.API == "" || result.Hosts.Tenant == "" || len(result.CSRF) > 512 || len(result.Transaction) > 512 || len(result.API) > 128 {
		return b2cSettings{}, ErrReauthorizationRequired
	}
	if !strings.HasPrefix(result.Hosts.Tenant, "/HiloDirectoryB2C.onmicrosoft.com/") || strings.ContainsAny(result.Hosts.Tenant, "?#\\\r\n") || strings.Contains(result.Hosts.Tenant, "..") || strings.ContainsAny(result.API, "/?#\\\r\n") {
		return b2cSettings{}, ErrReauthorizationRequired
	}
	return result, nil
}

func checkedCallback(raw, expectedState string) (string, error) {
	u, err := url.Parse(raw)
	base, _ := url.Parse(redirectURI)
	if err != nil || !strings.HasPrefix(raw, redirectURI+"?") || u.Scheme != base.Scheme || u.Host != base.Host || u.Path != base.Path || u.User != nil || u.Fragment != "" || u.RawQuery == "" {
		return "", ErrReauthorizationRequired
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", ErrReauthorizationRequired
	}
	if q.Get("error") != "" || len(q["state"]) != 1 || q.Get("state") != expectedState || len(q["code"]) != 1 || q.Get("code") == "" || len(q) != 2 || len(q.Get("code")) > 4096 {
		return "", ErrReauthorizationRequired
	}
	return q.Get("code"), nil
}

// credentialJSON is an opaque-token envelope. The id_token is deliberately
// ignored even when the provider supplies it; this pilot has no OIDC identity.
type credentialJSON struct {
	Version   int       `json:"version"`
	Access    string    `json:"access"`
	Refresh   string    `json:"refresh"`
	ExpiresAt time.Time `json:"expires_at"`
	Scope     string    `json:"scope"`
	Profile   string    `json:"profile"`
}

func (c credentialJSON) String() string               { return "[REDACTED HILO CREDENTIAL]" }
func (c credentialJSON) GoString() string             { return c.String() }
func (c credentialJSON) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }
func (c credentialJSON) secretValue() (storage.SecretValue, error) {
	if c.Version != 1 || c.Access == "" || c.Refresh == "" || c.ExpiresAt.IsZero() || c.Scope != clientScope || c.Profile != "hilo-android-b2c-v1" {
		return storage.SecretValue{}, ErrAuthorizationProtocol
	}
	// Marshal the private wire envelope explicitly; the value itself redacts
	// generic JSON/string diagnostics.
	wire := struct {
		Version   int       `json:"version"`
		Access    string    `json:"access"`
		Refresh   string    `json:"refresh"`
		ExpiresAt time.Time `json:"expires_at"`
		Scope     string    `json:"scope"`
		Profile   string    `json:"profile"`
	}{c.Version, c.Access, c.Refresh, c.ExpiresAt, c.Scope, c.Profile}
	raw, err := json.Marshal(wire)
	if err != nil {
		return storage.SecretValue{}, ErrAuthorizationProtocol
	}
	return storage.NewSecretValue(raw)
}
func decodeTokenResponse(raw []byte, priorRefresh string, now time.Time) (credentialJSON, error) {
	var response struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
		Scope   string `json:"scope"`
		Type    string `json:"token_type"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Access == "" || response.Expires <= 30 || response.Expires > 24*3600 || !strings.EqualFold(response.Type, "Bearer") {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	if response.Refresh == "" {
		response.Refresh = priorRefresh
	}
	if response.Refresh == "" || response.Scope != "" && response.Scope != clientScope {
		return credentialJSON{}, ErrReauthorizationRequired
	}
	// Persist the provider's actual expiry. liveCredential applies the single
	// thirty-second renewal margin when deciding whether to use the token.
	return credentialJSON{Version: 1, Access: response.Access, Refresh: response.Refresh, ExpiresAt: now.Add(time.Duration(response.Expires) * time.Second), Scope: clientScope, Profile: "hilo-android-b2c-v1"}, nil
}
