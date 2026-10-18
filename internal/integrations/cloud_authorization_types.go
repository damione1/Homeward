package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/storage"
)

// CloudAuthorizationProfileID is an open integration profile identifier.
type CloudAuthorizationProfileID struct{ value string }

func ParseCloudAuthorizationProfileID(raw string) (CloudAuthorizationProfileID, error) {
	if raw == "" || len(raw) > 128 || !utf8.ValidString(raw) {
		return CloudAuthorizationProfileID{}, &CloudError{kind: CloudInvalid}
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return CloudAuthorizationProfileID{}, &CloudError{kind: CloudInvalid}
		}
	}
	return CloudAuthorizationProfileID{value: raw}, nil
}

func (id CloudAuthorizationProfileID) Valid() bool { return id.value != "" }
func (id CloudAuthorizationProfileID) Equal(other CloudAuthorizationProfileID) bool {
	return id.value != "" && id.value == other.value
}
func (id CloudAuthorizationProfileID) String() string {
	return "[REDACTED CLOUD AUTHORIZATION PROFILE]"
}
func (id CloudAuthorizationProfileID) GoString() string { return id.String() }
func (id CloudAuthorizationProfileID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.String())
}

// Input and output are separate capabilities. Their secret bytes are owned,
// bounded by storage.SecretValue, and never enter diagnostics.
type CloudAuthorizationInput struct {
	profile CloudAuthorizationProfileID
	secret  storage.SecretValue
}

func NewCloudAuthorizationInput(profile CloudAuthorizationProfileID, secret storage.SecretValue) (CloudAuthorizationInput, error) {
	if !profile.Valid() {
		return CloudAuthorizationInput{}, &CloudError{kind: CloudInvalid}
	}
	raw, err := secret.Bytes()
	if err != nil {
		return CloudAuthorizationInput{}, &CloudError{kind: CloudInvalid}
	}
	owned, err := storage.NewSecretValue(raw)
	if err != nil {
		return CloudAuthorizationInput{}, &CloudError{kind: CloudInvalid}
	}
	return CloudAuthorizationInput{profile: profile, secret: owned}, nil
}
func (v CloudAuthorizationInput) Profile() CloudAuthorizationProfileID { return v.profile }
func (v CloudAuthorizationInput) Secret() (storage.SecretValue, error) {
	if !v.profile.Valid() {
		return storage.SecretValue{}, &CloudError{kind: CloudInvalid}
	}
	raw, err := v.secret.Bytes()
	if err != nil {
		return storage.SecretValue{}, &CloudError{kind: CloudInvalid}
	}
	return storage.NewSecretValue(raw)
}
func (v CloudAuthorizationInput) String() string   { return "[REDACTED CLOUD AUTHORIZATION INPUT]" }
func (v CloudAuthorizationInput) GoString() string { return v.String() }
func (v CloudAuthorizationInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

type CloudAuthorizedCredential struct {
	profile CloudAuthorizationProfileID
	secret  storage.SecretValue
}

func NewCloudAuthorizedCredential(profile CloudAuthorizationProfileID, secret storage.SecretValue) (CloudAuthorizedCredential, error) {
	if !profile.Valid() {
		return CloudAuthorizedCredential{}, &CloudError{kind: CloudInvalid}
	}
	raw, err := secret.Bytes()
	if err != nil {
		return CloudAuthorizedCredential{}, &CloudError{kind: CloudInvalid}
	}
	owned, err := storage.NewSecretValue(raw)
	if err != nil {
		return CloudAuthorizedCredential{}, &CloudError{kind: CloudInvalid}
	}
	return CloudAuthorizedCredential{profile: profile, secret: owned}, nil
}
func (v CloudAuthorizedCredential) Profile() CloudAuthorizationProfileID { return v.profile }
func (v CloudAuthorizedCredential) Secret() (storage.SecretValue, error) {
	if !v.profile.Valid() {
		return storage.SecretValue{}, &CloudError{kind: CloudInvalid}
	}
	raw, err := v.secret.Bytes()
	if err != nil {
		return storage.SecretValue{}, &CloudError{kind: CloudInvalid}
	}
	return storage.NewSecretValue(raw)
}
func (v CloudAuthorizedCredential) String() string   { return "[REDACTED CLOUD AUTHORIZED CREDENTIAL]" }
func (v CloudAuthorizedCredential) GoString() string { return v.String() }
func (v CloudAuthorizedCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

// Limits are Homeward safety bounds, not provider quotas. A caller may admit
// stricter values, but never widen the published ceiling.
type CloudAuthorizationLimitsInput struct {
	TotalTimeout time.Duration
	MaxExchanges int
	MaxActive    int
	MaxPerHandle int
}

type CloudAuthorizationLimits struct{ value CloudAuthorizationLimitsInput }

func AdmitCloudAuthorizationLimits(v CloudAuthorizationLimitsInput) (CloudAuthorizationLimits, error) {
	if v.TotalTimeout < time.Millisecond || v.TotalTimeout > 120*time.Second ||
		v.MaxExchanges < 1 || v.MaxExchanges > 6 ||
		v.MaxActive < 1 || v.MaxActive > 16 || v.MaxPerHandle != 1 {
		return CloudAuthorizationLimits{}, &CloudError{kind: CloudInvalid}
	}
	return CloudAuthorizationLimits{value: v}, nil
}
func DefaultCloudAuthorizationLimits() CloudAuthorizationLimits {
	v, _ := AdmitCloudAuthorizationLimits(CloudAuthorizationLimitsInput{
		TotalTimeout: 120 * time.Second, MaxExchanges: 6, MaxActive: 16, MaxPerHandle: 1,
	})
	return v
}
func (v CloudAuthorizationLimits) Valid() bool {
	_, err := AdmitCloudAuthorizationLimits(v.value)
	return err == nil
}
func (v CloudAuthorizationLimits) Input() CloudAuthorizationLimitsInput { return v.value }

// ExchangeID is unique only within one attempt. It is consumed before the
// shared policy admits a transport entry.
type CloudAuthorizationExchangeID struct{ value string }

func ParseCloudAuthorizationExchangeID(raw string) (CloudAuthorizationExchangeID, error) {
	if raw == "" || len(raw) > 128 || !utf8.ValidString(raw) {
		return CloudAuthorizationExchangeID{}, &CloudError{kind: CloudInvalid}
	}
	for _, r := range raw {
		if r < 0x21 || r > 0x7e {
			return CloudAuthorizationExchangeID{}, &CloudError{kind: CloudInvalid}
		}
	}
	return CloudAuthorizationExchangeID{value: raw}, nil
}
func (id CloudAuthorizationExchangeID) String() string {
	return "[REDACTED CLOUD AUTHORIZATION EXCHANGE ID]"
}
func (id CloudAuthorizationExchangeID) GoString() string { return id.String() }
func (id CloudAuthorizationExchangeID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.String())
}

// CloudAuthorizationExpected is a closed status expectation.
type CloudAuthorizationExpected uint8

const (
	CloudAuthorizationExpectedUnspecified CloudAuthorizationExpected = iota
	CloudAuthorizationExpectedOK
	CloudAuthorizationExpectedRedirect
	CloudAuthorizationExpectedOKOrRedirect
)

func (e CloudAuthorizationExpected) valid() bool {
	return e == CloudAuthorizationExpectedOK || e == CloudAuthorizationExpectedRedirect || e == CloudAuthorizationExpectedOKOrRedirect
}
func (e CloudAuthorizationExpected) accepts(status int) bool {
	switch e {
	case CloudAuthorizationExpectedOK:
		return status == http.StatusOK
	case CloudAuthorizationExpectedRedirect:
		return status == http.StatusFound
	case CloudAuthorizationExpectedOKOrRedirect:
		return status == http.StatusOK || status == http.StatusFound
	default:
		return false
	}
}

type CloudAuthorizationRequest struct {
	id       CloudAuthorizationExchangeID
	request  CloudRequest
	expected CloudAuthorizationExpected
}

func NewCloudAuthorizationRequest(id CloudAuthorizationExchangeID, method, rawURL string, headers http.Header, body []byte, expected CloudAuthorizationExpected) (CloudAuthorizationRequest, error) {
	if id.value == "" || !expected.valid() || (method != http.MethodGet && method != http.MethodPost) {
		return CloudAuthorizationRequest{}, &CloudError{kind: CloudInvalid}
	}
	for name := range headers {
		if strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Host") {
			return CloudAuthorizationRequest{}, &CloudError{kind: CloudInvalid}
		}
	}
	request, err := NewCloudRequest(method, rawURL, headers, body)
	if err != nil || !cloudAuthorizationHTTPSURL(&request.url, rawURL) {
		return CloudAuthorizationRequest{}, &CloudError{kind: CloudInvalid}
	}
	return CloudAuthorizationRequest{id: id, request: request, expected: expected}, nil
}
func (v CloudAuthorizationRequest) String() string   { return "[REDACTED CLOUD AUTHORIZATION REQUEST]" }
func (v CloudAuthorizationRequest) GoString() string { return v.String() }
func (v CloudAuthorizationRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

// The provider receives this wrapper, never an owner or an attempt pointer.
type CloudAuthorizationExchanger interface {
	Exchange(context.Context, CloudAuthorizationRequest) (CloudAuthorizationResponse, error)
}

type CloudAuthorizationCookie struct {
	name, value, domain, path, sourceHost string
	secure, hostOnly, deleted             bool
	expires                               time.Time
	maxAge                                int
}

func (c CloudAuthorizationCookie) Name() string       { return c.name }
func (c CloudAuthorizationCookie) Value() string      { return c.value }
func (c CloudAuthorizationCookie) Domain() string     { return c.domain }
func (c CloudAuthorizationCookie) Path() string       { return c.path }
func (c CloudAuthorizationCookie) SourceHost() string { return c.sourceHost }
func (c CloudAuthorizationCookie) Secure() bool       { return c.secure }
func (c CloudAuthorizationCookie) HostOnly() bool     { return c.hostOnly }
func (c CloudAuthorizationCookie) Deleted() bool      { return c.deleted }
func (c CloudAuthorizationCookie) Expires() time.Time { return c.expires }
func (c CloudAuthorizationCookie) MaxAge() int        { return c.maxAge }
func (c CloudAuthorizationCookie) String() string     { return "[REDACTED CLOUD AUTHORIZATION COOKIE]" }
func (c CloudAuthorizationCookie) GoString() string   { return c.String() }
func (c CloudAuthorizationCookie) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.String())
}

type CloudAuthorizationResponse struct {
	status   int
	body     []byte
	location string
	cookies  []CloudAuthorizationCookie
}

func (r CloudAuthorizationResponse) StatusCode() int { return r.status }
func (r CloudAuthorizationResponse) Bytes() []byte   { return append([]byte(nil), r.body...) }
func (r CloudAuthorizationResponse) Location() (string, bool) {
	return r.location, r.location != ""
}
func (r CloudAuthorizationResponse) Cookies() []CloudAuthorizationCookie {
	return append([]CloudAuthorizationCookie(nil), r.cookies...)
}
func (r CloudAuthorizationResponse) String() string   { return "[REDACTED CLOUD AUTHORIZATION RESPONSE]" }
func (r CloudAuthorizationResponse) GoString() string { return r.String() }
func (r CloudAuthorizationResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}
