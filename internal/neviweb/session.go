package neviweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrSession = errors.New("neviweb: session unavailable")
var ErrSessionExpired = errors.New("neviweb: session expired")
var ErrCredentials = errors.New("neviweb: credentials rejected")
var ErrTooManySessions = errors.New("neviweb: provider session limit")
var ErrProviderQuota = errors.New("neviweb: provider request limit")
var ErrUnsupportedAttribute = errors.New("neviweb: device attribute unsupported")
var ErrProtocol = errors.New("neviweb: malformed provider response")

// sessionEnvelope is untrusted persisted JSON. It is never used as runtime
// state until every field and the end of the document have been admitted.
type sessionEnvelope struct {
	Version      int    `json:"version"`
	State        string `json:"state"`
	SessionID    string `json:"session_id,omitempty"`
	CookieHeader string `json:"cookie_header,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

type sessionPhase uint8

const (
	sessionUnspecified sessionPhase = iota
	sessionAbsent
	sessionActive
)

type sessionState struct {
	phase                sessionPhase
	id, cookies, account string
}

func (s sessionState) String() string               { return "[REDACTED NEVIWEB SESSION]" }
func (s sessionState) GoString() string             { return s.String() }
func (s sessionState) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }
func (s sessionState) valid() bool {
	switch s.phase {
	case sessionAbsent:
		return s.id == "" && s.cookies == "" && s.account == ""
	case sessionActive:
		return s.id != "" && len(s.id) <= 512 && !strings.ContainsAny(s.id, "\r\n") && s.cookies != "" && len(s.cookies) <= 8192 && !strings.ContainsAny(s.cookies, "\r\n") && s.account != ""
	default:
		return false
	}
}

// InitialSessionValue is an internal typed state, never an operator sentinel.
// Only trusted server composition may persist it if the scoped handle is absent.
func InitialSessionValue() (storage.SecretValue, error) {
	return storage.NewSecretValue([]byte(`{"version":1,"state":"absent"}`))
}

func decodeSession(value storage.SecretValue) (sessionState, error) {
	raw, err := value.Bytes()
	if err != nil {
		return sessionState{}, ErrSession
	}
	var dto sessionEnvelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&dto) != nil || dto.Version != 1 {
		return sessionState{}, ErrSession
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return sessionState{}, ErrSession
	}
	var s sessionState
	switch dto.State {
	case "absent":
		s = sessionState{phase: sessionAbsent, id: dto.SessionID, cookies: dto.CookieHeader, account: dto.AccountID}
	case "active":
		s = sessionState{phase: sessionActive, id: dto.SessionID, cookies: dto.CookieHeader, account: dto.AccountID}
	default:
		return sessionState{}, ErrSession
	}
	if !s.valid() {
		return sessionState{}, ErrSession
	}
	return s, nil
}

func activeSessionValue(id, cookie, account string) (storage.SecretValue, error) {
	s := sessionState{phase: sessionActive, id: id, cookies: cookie, account: account}
	if !s.valid() {
		return storage.SecretValue{}, ErrSession
	}
	raw, err := json.Marshal(sessionEnvelope{Version: 1, State: "active", SessionID: id, CookieHeader: cookie, AccountID: account})
	if err != nil {
		return storage.SecretValue{}, ErrSession
	}
	v, err := storage.NewSecretValue(raw)
	if err != nil {
		return storage.SecretValue{}, ErrSession
	}
	return v, nil
}

func providerFailure(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	var probe any
	if json.Unmarshal(body, &probe) != nil {
		return ErrProtocol
	}
	if _, ok := probe.([]any); ok {
		return nil
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		ErrorCode string `json:"errorCode"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return ErrProtocol
	}
	code := envelope.Error.Code
	if code == "" {
		code = envelope.ErrorCode
	}
	switch code {
	case "":
		return nil
	case "USRSESSEXP":
		return ErrSessionExpired
	case "USRBADLOGIN":
		return ErrCredentials
	case "ACCSESSEXC":
		return ErrTooManySessions
	case "ACCDAYREQMAX", "ACCRATELIMIT":
		return ErrProviderQuota
	case "DVCATTRNSPTD":
		return ErrUnsupportedAttribute
	default:
		return ErrProtocol
	}
}

func (p *Pilot) session(ctx context.Context, host integrations.SessionHost, refresh bool) (sessionState, error) {
	observed, err := p.client.ReadCredential(ctx, p.settings.session)
	if err != nil {
		return sessionState{}, err
	}
	value, err := observed.Value()
	if err != nil {
		return sessionState{}, err
	}
	current, err := decodeSession(value)
	if err != nil {
		return sessionState{}, err
	}
	if current.phase == sessionActive && !refresh {
		if current.account != p.settings.accountText {
			return sessionState{}, ErrSession
		}
		return current, nil
	}
	result, err := p.client.BeginRenewal(ctx, observed)
	if err != nil {
		return sessionState{}, err
	}
	if result.Kind() == integrations.CloudRenewalJoined {
		joined, e := result.Value()
		if e != nil {
			return sessionState{}, e
		}
		state, e := decodeSession(joined)
		if e != nil || state.phase != sessionActive || state.account != p.settings.accountText {
			return sessionState{}, ErrSession
		}
		return state, nil
	}
	lease, err := result.Lease()
	if err != nil {
		return sessionState{}, err
	}
	defer lease.Close()
	provider, err := host.Secrets()
	if err != nil {
		return sessionState{}, err
	}
	username, err := provider.Read(ctx, p.settings.username)
	if err != nil {
		return sessionState{}, err
	}
	password, err := provider.Read(ctx, p.settings.password)
	if err != nil {
		return sessionState{}, err
	}
	u, err := username.Bytes()
	if err != nil {
		return sessionState{}, err
	}
	pass, err := password.Bytes()
	if err != nil {
		return sessionState{}, err
	}
	body, err := json.Marshal(map[string]any{"username": string(u), "password": string(pass), "interface": "neviweb", "stayConnected": 1})
	if err != nil {
		return sessionState{}, ErrProtocol
	}
	loginHeaders := http.Header{"Content-Type": []string{"application/json"}}
	if current.cookies != "" {
		loginHeaders.Set("Cookie", current.cookies)
	}
	request, err := integrations.NewCloudRequest(http.MethodPost, p.settings.endpoint("/api/login"), loginHeaders, body)
	if err != nil {
		return sessionState{}, err
	}
	response, err := lease.Exchange(request)
	if err != nil {
		return sessionState{}, decodeCloudFailure(err, false)
	}
	if err := providerFailure(response.Bytes()); err != nil {
		return sessionState{}, err
	}
	var data struct {
		Session string `json:"session"`
		Account struct {
			ID json.Number `json:"id"`
		} `json:"account"`
	}
	dec := json.NewDecoder(bytes.NewReader(response.Bytes()))
	dec.UseNumber()
	if dec.Decode(&data) != nil {
		return sessionState{}, ErrProtocol
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return sessionState{}, ErrProtocol
	}
	if data.Session == "" || data.Account.ID.String() != p.settings.accountText {
		return sessionState{}, ErrSession
	}
	cookie, err := response.Cookies().MergeHeader(current.cookies)
	if err != nil || cookie == "" {
		return sessionState{}, ErrSession
	}
	encoded, err := activeSessionValue(data.Session, cookie, p.settings.accountText)
	if err != nil {
		return sessionState{}, err
	}
	if _, err := decodeSession(encoded); err != nil {
		return sessionState{}, err
	}
	if err := lease.Commit(encoded); err != nil {
		return sessionState{}, err
	}
	return decodeSession(encoded)
}

func sessionHeaders(s sessionState) http.Header {
	return http.Header{"Session-Id": []string{s.id}, "Cookie": []string{s.cookies}}
}

func decodeCloudFailure(err error, observation bool) error {
	var cloudErr *integrations.CloudError
	if errors.As(err, &cloudErr) {
		if body := cloudErr.ResponseBytes(); len(body) != 0 {
			if coded := providerFailure(body); coded != nil && coded != ErrProtocol {
				return coded
			}
		}
		if observation && cloudErr.Kind() == integrations.CloudCredentialFailed {
			return ErrSessionExpired
		}
	}
	return err
}
