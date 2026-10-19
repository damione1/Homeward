package authmfa

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/net/idna"
)

const recordSchema = 1

type RelyingParty struct {
	instance contracts.InstanceID
	rpID     string
	origin   string
}

// NewRelyingParty accepts only the configured canonical HTTPS URL. It must
// never be called with Host/Forwarded or an origin from a credential proof.
func NewRelyingParty(instance contracts.InstanceID, canonicalURL string) (RelyingParty, error) {
	identity, err := instance.Value()
	if err != nil || !validID(identity) || len(canonicalURL) > 4096 {
		return RelyingParty{}, errorOf(ErrorInvalidInput)
	}
	parsed, err := url.Parse(canonicalURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return RelyingParty{}, errorOf(ErrorInvalidInput)
	}
	host, err := idna.Lookup.ToASCII(strings.ToLower(parsed.Hostname()))
	if err != nil || host == "" || strings.HasSuffix(host, ".") || protocol.ValidateRPID(host) != nil {
		return RelyingParty{}, errorOf(ErrorInvalidInput)
	}
	port := parsed.Port()
	if port != "" {
		number, parseErr := strconv.Atoi(port)
		if parseErr != nil || number < 1 || number > 65535 {
			return RelyingParty{}, errorOf(ErrorInvalidInput)
		}
		if number == 443 {
			port = ""
		} else {
			port = strconv.Itoa(number)
		}
	}
	originHost := host
	if strings.Contains(host, ":") {
		originHost = "[" + host + "]"
	}
	if port != "" {
		originHost = net.JoinHostPort(host, port)
	}
	return RelyingParty{instance: instance, rpID: host, origin: (&url.URL{Scheme: "https", Host: originHost}).String()}, nil
}

func (r RelyingParty) valid() bool {
	text, err := r.instance.Value()
	return err == nil && validID(text) && r.rpID != "" && r.origin != "" && protocol.ValidateRPID(r.rpID) == nil
}

func (r RelyingParty) Values() (contracts.InstanceID, string, string, error) {
	if !r.valid() {
		return contracts.InstanceID{}, "", "", errorOf(ErrorInvalidInput)
	}
	return r.instance, r.rpID, r.origin, nil
}

type CredentialRecord struct{ *credentialRecordValue }
type credentialRecordValue struct {
	account contracts.AccountID
	factor  contracts.FactorCredentialID
	party   RelyingParty
	version RecordVersion
	encoded []byte
}

type credentialDocument struct {
	Schema     uint32              `json:"schema"`
	Account    string              `json:"account"`
	Factor     string              `json:"factor"`
	Instance   string              `json:"instance"`
	RPID       string              `json:"rp_id"`
	Version    uint64              `json:"version"`
	Flags      byte                `json:"flags"`
	Credential webauthn.Credential `json:"credential"`
}

func validCredential(credential webauthn.Credential) bool {
	return len(credential.ID) > 0 && len(credential.ID) <= MaxCredentialIDBytes && len(credential.PublicKey) > 0 && len(credential.PublicKey) <= MaxPublicKeyBytes &&
		len(credential.Authenticator.AAGUID) == 16 && credential.Flags.UserPresent && credential.Flags.UserVerified &&
		(!credential.Flags.BackupState || credential.Flags.BackupEligible) &&
		len(credential.Attestation.ClientDataJSON) <= MaxClientDataJSONBytes && len(credential.Attestation.Object) <= MaxAttestationObjectBytes &&
		len(credential.Attestation.AuthenticatorData) <= MaxAuthenticatorDataBytes && len(credential.Attestation.ClientDataHash) == ClientDataHashBytes
}

func newCredentialRecord(account contracts.AccountID, factor contracts.FactorCredentialID, party RelyingParty, version RecordVersion, credential webauthn.Credential) (CredentialRecord, error) {
	a, ae := account.Value()
	f, fe := factor.Value()
	i, ie := party.instance.Value()
	v, ve := version.Number()
	if ae != nil || fe != nil || ie != nil || ve != nil || !validID(a) || !validID(f) || !party.valid() || !validCredential(credential) {
		return CredentialRecord{}, errorOf(ErrorIntegrity)
	}
	document := credentialDocument{Schema: recordSchema, Account: a, Factor: f, Instance: i, RPID: party.rpID, Version: v, Flags: byte(credential.Flags.ProtocolValue()), Credential: credential}
	raw, err := json.Marshal(document)
	if err != nil || len(raw) == 0 || len(raw) > MaxCredentialRecordBytes {
		return CredentialRecord{}, errorOf(ErrorInvalidInput)
	}
	return CredentialRecord{credentialRecordValue: &credentialRecordValue{account: account, factor: factor, party: party, version: version, encoded: raw}}, nil
}

// DecodeCredentialRecord admits a decrypted record against the independently
// authenticated SQL identity/version. Only the existing storage AEAD may supply
// this plaintext; shape admission alone cannot authorize an enrollment.
func DecodeCredentialRecord(account contracts.AccountID, factor contracts.FactorCredentialID, party RelyingParty, version RecordVersion, raw []byte) (CredentialRecord, error) {
	if len(raw) == 0 || len(raw) > MaxCredentialRecordBytes {
		return CredentialRecord{}, errorOf(ErrorIntegrity)
	}
	var document credentialDocument
	if err := decodeOneJSON(raw, &document); err != nil {
		return CredentialRecord{}, err
	}
	a, ae := account.Value()
	f, fe := factor.Value()
	i, ie := party.instance.Value()
	v, ve := version.Number()
	if ae != nil || fe != nil || ie != nil || ve != nil || !party.valid() || document.Schema != recordSchema || document.Account != a || document.Factor != f || document.Instance != i || document.RPID != party.rpID || document.Version != v {
		return CredentialRecord{}, errorOf(ErrorIntegrity)
	}
	// CredentialFlags.raw is private and JSON does not restore it upstream.
	// Persist its protocol octet and reject disagreement with the public flags.
	flags := webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(document.Flags))
	observed := document.Credential.Flags
	if flags.UserPresent != observed.UserPresent || flags.UserVerified != observed.UserVerified || flags.BackupEligible != observed.BackupEligible || flags.BackupState != observed.BackupState {
		return CredentialRecord{}, errorOf(ErrorIntegrity)
	}
	document.Credential.Flags = flags
	return newCredentialRecord(account, factor, party, version, document.Credential)
}

func (r CredentialRecord) valid() bool {
	if r.credentialRecordValue == nil {
		return false
	}
	a, ae := r.account.Value()
	f, fe := r.factor.Value()
	return ae == nil && fe == nil && validID(a) && validID(f) && r.party.valid() && r.version.revision > 0 && len(r.encoded) > 0 && len(r.encoded) <= MaxCredentialRecordBytes
}

func (r CredentialRecord) clone() CredentialRecord {
	if r.credentialRecordValue == nil {
		return CredentialRecord{}
	}
	copyValue := *r.credentialRecordValue
	copyValue.encoded = append([]byte(nil), r.encoded...)
	return CredentialRecord{credentialRecordValue: &copyValue}
}

func (r CredentialRecord) credential() (webauthn.Credential, error) {
	if !r.valid() {
		return webauthn.Credential{}, errorOf(ErrorIntegrity)
	}
	var document credentialDocument
	if err := decodeOneJSON(r.encoded, &document); err != nil {
		return webauthn.Credential{}, err
	}
	document.Credential.Flags = webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(document.Flags))
	return document.Credential, nil
}

func (r CredentialRecord) Bytes() ([]byte, error) {
	if !r.valid() {
		return nil, errorOf(ErrorInvalidInput)
	}
	return append([]byte(nil), r.encoded...), nil
}

func (r CredentialRecord) ID() (contracts.FactorCredentialID, error) {
	if !r.valid() {
		return contracts.FactorCredentialID{}, errorOf(ErrorInvalidInput)
	}
	return r.factor, nil
}

func (r CredentialRecord) Version() (RecordVersion, error) {
	if !r.valid() {
		return RecordVersion{}, errorOf(ErrorInvalidInput)
	}
	return r.version, nil
}

func (CredentialRecord) String() string                 { return "[REDACTED MFA CREDENTIAL]" }
func (r CredentialRecord) GoString() string             { return r.String() }
func (r CredentialRecord) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

type UserSnapshot struct{ *userSnapshotValue }
type userSnapshotValue struct {
	account contracts.AccountID
	handle  [UserHandleBytes]byte
	name    string
	display string
	records []CredentialRecord
}

func NewUserHandle() ([]byte, error) {
	var value [UserHandleBytes]byte
	if _, err := rand.Read(value[:]); err != nil || value == [UserHandleBytes]byte{} {
		return nil, errorOf(ErrorUnavailable)
	}
	return append([]byte(nil), value[:]...), nil
}

func NewUserSnapshot(account contracts.AccountID, handle []byte, name, display string, records []CredentialRecord) (UserSnapshot, error) {
	text, err := account.Value()
	if err != nil || !validID(text) || len(handle) != UserHandleBytes || name == "" || len(name) > 256 || len(display) > 1024 || !utf8.ValidString(name) || !utf8.ValidString(display) || len(records) > CredentialLimit {
		return UserSnapshot{}, errorOf(ErrorInvalidInput)
	}
	result := UserSnapshot{userSnapshotValue: &userSnapshotValue{account: account, name: name, display: display, records: make([]CredentialRecord, 0, len(records))}}
	copy(result.handle[:], handle)
	if result.handle == [UserHandleBytes]byte{} {
		return UserSnapshot{}, errorOf(ErrorInvalidInput)
	}
	seen := make(map[contracts.FactorCredentialID]struct{}, len(records))
	seenAuthenticator := make(map[string]struct{}, len(records))
	for _, record := range records {
		if !record.valid() || record.account != account {
			return UserSnapshot{}, errorOf(ErrorIntegrity)
		}
		if _, duplicate := seen[record.factor]; duplicate {
			return UserSnapshot{}, errorOf(ErrorIntegrity)
		}
		seen[record.factor] = struct{}{}
		credential, err := record.credential()
		if err != nil {
			return UserSnapshot{}, err
		}
		if _, duplicate := seenAuthenticator[string(credential.ID)]; duplicate {
			return UserSnapshot{}, errorOf(ErrorIntegrity)
		}
		seenAuthenticator[string(credential.ID)] = struct{}{}
		result.records = append(result.records, record.clone())
	}
	return result, nil
}

func (u UserSnapshot) validFor(binding Binding, party RelyingParty) bool {
	if u.userSnapshotValue == nil || !binding.valid() || !party.valid() || binding.account != u.account || binding.instance != party.instance || u.handle == [UserHandleBytes]byte{} || u.name == "" || u.records == nil || len(u.records) > CredentialLimit {
		return false
	}
	for _, record := range u.records {
		if !record.valid() || record.account != u.account || record.party != party {
			return false
		}
	}
	return true
}

func (u UserSnapshot) WebAuthnID() []byte {
	if u.userSnapshotValue == nil {
		return nil
	}
	return append([]byte(nil), u.handle[:]...)
}
func (u UserSnapshot) WebAuthnName() string {
	if u.userSnapshotValue == nil {
		return ""
	}
	return u.name
}
func (u UserSnapshot) WebAuthnDisplayName() string {
	if u.userSnapshotValue == nil {
		return ""
	}
	return u.display
}
func (u UserSnapshot) WebAuthnCredentials() []webauthn.Credential {
	if u.userSnapshotValue == nil {
		return nil
	}
	result := make([]webauthn.Credential, 0, len(u.records))
	for _, record := range u.records {
		credential, err := record.credential()
		if err != nil {
			return nil
		} // invalid private snapshot fails the library's credential checks
		result = append(result, credential)
	}
	return result
}
func (UserSnapshot) String() string                 { return "[REDACTED MFA USER SNAPSHOT]" }
func (u UserSnapshot) GoString() string             { return u.String() }
func (u UserSnapshot) MarshalJSON() ([]byte, error) { return json.Marshal(u.String()) }

type PreparedCeremony struct{ *preparedCeremonyValue }
type preparedCeremonyValue struct {
	kind    CeremonyKind
	binding Binding
	party   RelyingParty
	expires time.Time
	encoded []byte
}

type ceremonyDocument struct {
	Schema      uint32                `json:"schema"`
	Kind        CeremonyKind          `json:"kind"`
	Account     string                `json:"account"`
	ChallengeID string                `json:"challenge_id"`
	Instance    string                `json:"instance"`
	RPID        string                `json:"rp_id"`
	Origin      string                `json:"origin"`
	ExpiresAt   time.Time             `json:"expires_at"`
	Session     *webauthn.SessionData `json:"session"`
}

func validSessionData(kind CeremonyKind, party RelyingParty, session *webauthn.SessionData) bool {
	if !validCeremonyKind(kind) || !party.valid() || session == nil || session.RelyingPartyID != party.rpID || session.Origin != party.origin || len(session.UserID) != UserHandleBytes || session.UserVerification != protocol.VerificationRequired || session.Mediation != protocol.MediationDefault || session.AuthorizeUVInitialization || !session.Expires.IsZero() {
		return false
	}
	challenge, err := base64.RawURLEncoding.DecodeString(session.Challenge)
	if err != nil || len(challenge) != ChallengeBytes || base64.RawURLEncoding.EncodeToString(challenge) != session.Challenge {
		return false
	}
	var challengeArray [ChallengeBytes]byte
	var handleArray [UserHandleBytes]byte
	copy(challengeArray[:], challenge)
	copy(handleArray[:], session.UserID)
	if challengeArray == [ChallengeBytes]byte{} || handleArray == [UserHandleBytes]byte{} {
		return false
	}
	if kind == CeremonyEnrollment && (len(session.AllowedCredentialIDs) != 0 || len(session.CredParams) != 1 || session.CredParams[0].Type != protocol.PublicKeyCredentialType || session.CredParams[0].Algorithm != webauthncose.AlgES256) {
		return false
	}
	if kind == CeremonyLogin && (len(session.AllowedCredentialIDs) == 0 || len(session.AllowedCredentialIDs) > CredentialLimit || len(session.CredParams) != 0) {
		return false
	}
	seen := make(map[string]struct{}, len(session.AllowedCredentialIDs))
	for _, id := range session.AllowedCredentialIDs {
		if len(id) == 0 || len(id) > MaxCredentialIDBytes {
			return false
		}
		if _, duplicate := seen[string(id)]; duplicate {
			return false
		}
		seen[string(id)] = struct{}{}
	}
	return true
}

func newPreparedCeremony(kind CeremonyKind, binding Binding, party RelyingParty, expires time.Time, session *webauthn.SessionData) (PreparedCeremony, error) {
	// PostgreSQL persists microseconds. Normalize only our server deadline,
	// never any field in the upstream SessionData returned by Begin.
	expires = expires.Truncate(time.Microsecond)
	if !binding.valid() || binding.instance != party.instance || expires.IsZero() || !validSessionData(kind, party, session) {
		return PreparedCeremony{}, errorOf(ErrorIntegrity)
	}
	a, _ := binding.account.Value()
	c, _ := binding.challenge.Value()
	i, _ := binding.instance.Value()
	raw, err := json.Marshal(ceremonyDocument{Schema: recordSchema, Kind: kind, Account: a, ChallengeID: c, Instance: i, RPID: party.rpID, Origin: party.origin, ExpiresAt: expires, Session: session})
	if err != nil || len(raw) == 0 || len(raw) > MaxCeremonyRecordBytes {
		return PreparedCeremony{}, errorOf(ErrorInvalidInput)
	}
	return PreparedCeremony{preparedCeremonyValue: &preparedCeremonyValue{kind: kind, binding: binding, party: party, expires: expires, encoded: raw}}, nil
}

func DecodePreparedCeremony(binding Binding, party RelyingParty, raw []byte, now time.Time) (PreparedCeremony, error) {
	if !binding.valid() || !party.valid() || now.IsZero() || len(raw) == 0 || len(raw) > MaxCeremonyRecordBytes {
		return PreparedCeremony{}, errorOf(ErrorIntegrity)
	}
	var document ceremonyDocument
	if err := decodeOneJSON(raw, &document); err != nil {
		return PreparedCeremony{}, err
	}
	a, _ := binding.account.Value()
	c, _ := binding.challenge.Value()
	i, _ := binding.instance.Value()
	if !document.ExpiresAt.Equal(document.ExpiresAt.Truncate(time.Microsecond)) || document.Schema != recordSchema || document.Account != a || document.ChallengeID != c || document.Instance != i || document.RPID != party.rpID || document.Origin != party.origin || document.ExpiresAt.After(now.Add(CeremonyLifetime)) {
		return PreparedCeremony{}, errorOf(ErrorIntegrity)
	}
	if !now.Before(document.ExpiresAt) {
		return PreparedCeremony{}, errorOf(ErrorExpired)
	}
	return newPreparedCeremony(document.Kind, binding, party, document.ExpiresAt, document.Session)
}

func (p PreparedCeremony) session(now time.Time) (webauthn.SessionData, error) {
	if p.preparedCeremonyValue == nil || now.IsZero() || p.expires.IsZero() || len(p.encoded) == 0 || len(p.encoded) > MaxCeremonyRecordBytes || !p.binding.valid() || !p.party.valid() {
		return webauthn.SessionData{}, errorOf(ErrorIntegrity)
	}
	if !now.Before(p.expires) {
		return webauthn.SessionData{}, errorOf(ErrorExpired)
	}
	var document ceremonyDocument
	if err := decodeOneJSON(p.encoded, &document); err != nil {
		return webauthn.SessionData{}, err
	}
	if !validSessionData(p.kind, p.party, document.Session) {
		return webauthn.SessionData{}, errorOf(ErrorIntegrity)
	}
	return *document.Session, nil
}

func (p PreparedCeremony) Bytes() ([]byte, error) {
	if p.preparedCeremonyValue == nil || !p.binding.valid() || !p.party.valid() || p.expires.IsZero() || len(p.encoded) == 0 || len(p.encoded) > MaxCeremonyRecordBytes {
		return nil, errorOf(ErrorInvalidInput)
	}
	return append([]byte(nil), p.encoded...), nil
}

func (p PreparedCeremony) ExpiresAt() (time.Time, error) {
	if _, err := p.Bytes(); err != nil {
		return time.Time{}, err
	}
	return p.expires, nil
}
func (PreparedCeremony) String() string                 { return "[REDACTED MFA CEREMONY]" }
func (p PreparedCeremony) GoString() string             { return p.String() }
func (p PreparedCeremony) MarshalJSON() ([]byte, error) { return json.Marshal(p.String()) }

type Verifier struct {
	party  RelyingParty
	server *webauthn.WebAuthn
}

func NewVerifier(party RelyingParty) (*Verifier, error) {
	if !party.valid() {
		return nil, errorOf(ErrorInvalidInput)
	}
	// SessionData is preserved exactly as returned by Begin. The authoritative
	// five-minute expiration is in PreparedCeremony and rechecked in PG. Leave
	// the upstream optional wall-clock expiry disabled rather than rewriting it
	// or making an injected server clock silently depend on time.Now().
	server, err := webauthn.New(&webauthn.Config{RPID: party.rpID, RPDisplayName: "Homeward", RPOrigins: []string{party.origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired},
		Timeouts:               webauthn.TimeoutsConfig{Login: webauthn.TimeoutConfig{Enforce: false, Timeout: CeremonyLifetime, TimeoutUVD: CeremonyLifetime}, Registration: webauthn.TimeoutConfig{Enforce: false, Timeout: CeremonyLifetime, TimeoutUVD: CeremonyLifetime}}})
	if err != nil {
		return nil, errorOf(ErrorInvalidInput)
	}
	return &Verifier{party: party, server: server}, nil
}

func newChallenge() ([]byte, error) {
	var challenge [ChallengeBytes]byte
	if _, err := rand.Read(challenge[:]); err != nil || challenge == [ChallengeBytes]byte{} {
		return nil, errorOf(ErrorUnavailable)
	}
	return append([]byte(nil), challenge[:]...), nil
}

func (v *Verifier) BeginEnrollment(ctx context.Context, binding Binding, user UserSnapshot, now time.Time) (PreparedCeremony, error) {
	if !validContext(ctx) || v == nil || v.server == nil || now.IsZero() || !user.validFor(binding, v.party) || len(user.records) >= CredentialLimit {
		return PreparedCeremony{}, errorOf(ErrorInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return PreparedCeremony{}, err
	}
	challenge, err := newChallenge()
	if err != nil {
		return PreparedCeremony{}, err
	}
	exclusions := make([]protocol.CredentialDescriptor, 0, len(user.records))
	for _, credential := range user.WebAuthnCredentials() {
		exclusions = append(exclusions, credential.Descriptor())
	}
	_, session, err := v.server.BeginRegistration(user, webauthn.WithRegistrationOrigin(v.party.origin), webauthn.WithExclusions(exclusions),
		webauthn.WithCredentialParameters([]protocol.CredentialParameter{{Type: protocol.PublicKeyCredentialType, Algorithm: webauthncose.AlgES256}}),
		func(options *protocol.PublicKeyCredentialCreationOptions) error {
			options.Challenge = protocol.URLEncodedBase64(challenge)
			return nil
		})
	if err != nil {
		return PreparedCeremony{}, errorOf(ErrorUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return PreparedCeremony{}, err
	}
	return newPreparedCeremony(CeremonyEnrollment, binding, v.party, now.Add(CeremonyLifetime), session)
}

func (v *Verifier) BeginLogin(ctx context.Context, binding Binding, user UserSnapshot, now time.Time) (PreparedCeremony, error) {
	if !validContext(ctx) || v == nil || v.server == nil || now.IsZero() || !user.validFor(binding, v.party) || len(user.records) == 0 {
		return PreparedCeremony{}, errorOf(ErrorInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return PreparedCeremony{}, err
	}
	challenge, err := newChallenge()
	if err != nil {
		return PreparedCeremony{}, err
	}
	_, session, err := v.server.BeginLogin(user, webauthn.WithChallenge(challenge), webauthn.WithLoginOrigin(v.party.origin), webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return PreparedCeremony{}, errorOf(ErrorUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return PreparedCeremony{}, err
	}
	return newPreparedCeremony(CeremonyLogin, binding, v.party, now.Add(CeremonyLifetime), session)
}

func (p PreparedCeremony) Challenge(now time.Time) ([]byte, error) {
	session, err := p.session(now)
	if err != nil {
		return nil, err
	}
	return base64.RawURLEncoding.DecodeString(session.Challenge)
}

type credentialJSON[T any] struct {
	ID       string                  `json:"id"`
	RawID    string                  `json:"rawId"`
	Type     protocol.CredentialType `json:"type"`
	Response T                       `json:"response"`
}

type assertionResponseJSON struct {
	AuthenticatorData string  `json:"authenticatorData"`
	ClientDataJSON    string  `json:"clientDataJSON"`
	Signature         string  `json:"signature"`
	UserHandle        *string `json:"userHandle,omitempty"`
}

type attestationResponseJSON struct {
	ClientDataJSON    string `json:"clientDataJSON"`
	AttestationObject string `json:"attestationObject"`
}

func assertionJSON(input *authv1.Fido2Assertion) ([]byte, error) {
	if input == nil || len(input.ProtoReflect().GetUnknown()) != 0 || len(input.CredentialId) == 0 || len(input.CredentialId) > MaxCredentialIDBytes || len(input.AuthenticatorData) == 0 || len(input.AuthenticatorData) > MaxAuthenticatorDataBytes || len(input.ClientDataJson) == 0 || len(input.ClientDataJson) > MaxClientDataJSONBytes || len(input.Signature) == 0 || len(input.Signature) > MaxSignatureBytes || len(input.ClientDataHash) != ClientDataHashBytes || input.UserHandle != nil && len(input.UserHandle) != UserHandleBytes {
		return nil, errorOf(ErrorInvalidProof)
	}
	hash := sha256.Sum256(input.ClientDataJson)
	if subtle.ConstantTimeCompare(hash[:], input.ClientDataHash) != 1 {
		return nil, errorOf(ErrorInvalidProof)
	}
	response := assertionResponseJSON{AuthenticatorData: base64.RawURLEncoding.EncodeToString(input.AuthenticatorData), ClientDataJSON: base64.RawURLEncoding.EncodeToString(input.ClientDataJson), Signature: base64.RawURLEncoding.EncodeToString(input.Signature)}
	if input.UserHandle != nil {
		text := base64.RawURLEncoding.EncodeToString(input.UserHandle)
		response.UserHandle = &text
	}
	id := base64.RawURLEncoding.EncodeToString(input.CredentialId)
	raw, err := json.Marshal(credentialJSON[assertionResponseJSON]{ID: id, RawID: id, Type: protocol.PublicKeyCredentialType, Response: response})
	if err != nil || len(raw) > MaxRequestBytes {
		return nil, errorOf(ErrorInvalidProof)
	}
	return raw, nil
}

func attestationJSON(input *authv1.Fido2Attestation) ([]byte, error) {
	if input == nil || len(input.ProtoReflect().GetUnknown()) != 0 || len(input.CredentialId) == 0 || len(input.CredentialId) > MaxCredentialIDBytes || len(input.ClientDataJson) == 0 || len(input.ClientDataJson) > MaxClientDataJSONBytes || len(input.AttestationObject) == 0 || len(input.AttestationObject) > MaxAttestationObjectBytes || len(input.ClientDataHash) != ClientDataHashBytes {
		return nil, errorOf(ErrorInvalidProof)
	}
	hash := sha256.Sum256(input.ClientDataJson)
	if subtle.ConstantTimeCompare(hash[:], input.ClientDataHash) != 1 {
		return nil, errorOf(ErrorInvalidProof)
	}
	id := base64.RawURLEncoding.EncodeToString(input.CredentialId)
	raw, err := json.Marshal(credentialJSON[attestationResponseJSON]{ID: id, RawID: id, Type: protocol.PublicKeyCredentialType, Response: attestationResponseJSON{ClientDataJSON: base64.RawURLEncoding.EncodeToString(input.ClientDataJson), AttestationObject: base64.RawURLEncoding.EncodeToString(input.AttestationObject)}})
	if err != nil || len(raw) > MaxRequestBytes {
		return nil, errorOf(ErrorInvalidProof)
	}
	return raw, nil
}

type VerifiedEnrollment struct {
	binding        Binding
	ceremonyDigest [32]byte
	record         CredentialRecord
}

func (v VerifiedEnrollment) Binding() (Binding, error) {
	if !v.binding.valid() || v.ceremonyDigest == [32]byte{} || !v.record.valid() || v.record.account != v.binding.account || v.record.party.instance != v.binding.instance {
		return Binding{}, errorOf(ErrorInvalidProof)
	}
	return v.binding, nil
}
func (v VerifiedEnrollment) Record() (CredentialRecord, error) {
	if _, err := v.Binding(); err != nil {
		return CredentialRecord{}, err
	}
	return v.record.clone(), nil
}
func (VerifiedEnrollment) String() string                 { return "[REDACTED MFA ENROLLMENT PROOF]" }
func (v VerifiedEnrollment) GoString() string             { return v.String() }
func (v VerifiedEnrollment) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

func (v *Verifier) VerifyEnrollment(ctx context.Context, user UserSnapshot, ceremony PreparedCeremony, factor contracts.FactorCredentialID, input *authv1.Fido2Attestation, now time.Time) (VerifiedEnrollment, error) {
	if !validContext(ctx) || v == nil || v.server == nil || ceremony.preparedCeremonyValue == nil || ceremony.kind != CeremonyEnrollment || ceremony.party != v.party || !user.validFor(ceremony.binding, v.party) || len(user.records) >= CredentialLimit {
		return VerifiedEnrollment{}, errorOf(ErrorInvalidProof)
	}
	if err := ctx.Err(); err != nil {
		return VerifiedEnrollment{}, err
	}
	session, err := ceremony.session(now)
	if err != nil {
		return VerifiedEnrollment{}, err
	}
	if !bytes.Equal(session.UserID, user.handle[:]) {
		return VerifiedEnrollment{}, errorOf(ErrorInvalidProof)
	}
	raw, err := attestationJSON(input)
	if err != nil {
		return VerifiedEnrollment{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return VerifiedEnrollment{}, errorOf(ErrorInvalidProof)
	}
	credential, err := v.server.CreateCredential(user, session, parsed)
	if err != nil || credential == nil || !bytes.Equal(credential.ID, input.CredentialId) || credential.Authenticator.CloneWarning {
		return VerifiedEnrollment{}, errorOf(ErrorInvalidProof)
	}
	for _, existing := range user.WebAuthnCredentials() {
		if bytes.Equal(existing.ID, credential.ID) {
			return VerifiedEnrollment{}, errorOf(ErrorConflict)
		}
	}
	if err := ctx.Err(); err != nil {
		return VerifiedEnrollment{}, err
	}
	version, _ := NewRecordVersion(1)
	record, err := newCredentialRecord(user.account, factor, v.party, version, *credential)
	if err != nil {
		return VerifiedEnrollment{}, err
	}
	return VerifiedEnrollment{binding: ceremony.binding, ceremonyDigest: sha256.Sum256(ceremony.encoded), record: record}, nil
}

func (v *Verifier) VerifyLogin(ctx context.Context, user UserSnapshot, ceremony PreparedCeremony, input *authv1.Fido2Assertion, now time.Time) (VerifiedLogin, error) {
	if !validContext(ctx) || v == nil || v.server == nil || ceremony.preparedCeremonyValue == nil || ceremony.kind != CeremonyLogin || ceremony.party != v.party || !user.validFor(ceremony.binding, v.party) {
		return VerifiedLogin{}, errorOf(ErrorInvalidProof)
	}
	if err := ctx.Err(); err != nil {
		return VerifiedLogin{}, err
	}
	session, err := ceremony.session(now)
	if err != nil {
		return VerifiedLogin{}, err
	}
	if !bytes.Equal(session.UserID, user.handle[:]) {
		return VerifiedLogin{}, errorOf(ErrorInvalidProof)
	}
	raw, err := assertionJSON(input)
	if err != nil {
		return VerifiedLogin{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return VerifiedLogin{}, errorOf(ErrorInvalidProof)
	}
	credential, err := v.server.ValidateLogin(user, session, parsed)
	if err != nil || credential == nil || credential.Authenticator.CloneWarning {
		return VerifiedLogin{}, errorOf(ErrorInvalidProof)
	}
	if err := ctx.Err(); err != nil {
		return VerifiedLogin{}, err
	}
	for _, existing := range user.records {
		previous, err := existing.credential()
		if err != nil {
			return VerifiedLogin{}, err
		}
		if !bytes.Equal(previous.ID, credential.ID) {
			continue
		}
		next, err := existing.version.Next()
		if err != nil {
			return VerifiedLogin{}, err
		}
		updated, err := newCredentialRecord(user.account, existing.factor, v.party, next, *credential)
		if err != nil {
			return VerifiedLogin{}, err
		}
		return VerifiedLogin{variant: verifiedFIDO2, binding: ceremony.binding, ceremonyDigest: sha256.Sum256(ceremony.encoded), fido: &VerifiedFIDO2{expected: existing.version, updated: updated}}, nil
	}
	return VerifiedLogin{}, errorOf(ErrorInvalidProof)
}

func (r CredentialRecord) Account() (contracts.AccountID, error) {
	if !r.valid() {
		return contracts.AccountID{}, errorOf(ErrorIntegrity)
	}
	return r.account, nil
}
func (r CredentialRecord) RelyingParty() (RelyingParty, error) {
	if !r.valid() {
		return RelyingParty{}, errorOf(ErrorIntegrity)
	}
	return r.party, nil
}
func (p PreparedCeremony) Binding() (Binding, error) {
	if _, err := p.Bytes(); err != nil {
		return Binding{}, err
	}
	return p.binding, nil
}
func (p PreparedCeremony) Kind() (CeremonyKind, error) {
	if _, err := p.Bytes(); err != nil {
		return CeremonyUnspecified, err
	}
	return p.kind, nil
}
func (p PreparedCeremony) RelyingParty() (RelyingParty, error) {
	if _, err := p.Bytes(); err != nil {
		return RelyingParty{}, err
	}
	return p.party, nil
}
func (u UserSnapshot) Records() ([]CredentialRecord, error) {
	if u.userSnapshotValue == nil {
		return nil, errorOf(ErrorInvalidInput)
	}
	result := make([]CredentialRecord, 0, len(u.records))
	for _, record := range u.records {
		if !record.valid() {
			return nil, errorOf(ErrorIntegrity)
		}
		result = append(result, record.clone())
	}
	return result, nil
}
func (p PreparedCeremony) AssertionOptions(now time.Time) (*authv1.Fido2AssertionChallenge, error) {
	if p.preparedCeremonyValue == nil || p.kind != CeremonyLogin {
		return nil, errorOf(ErrorInvalidInput)
	}
	session, err := p.session(now)
	if err != nil {
		return nil, err
	}
	challenge, err := p.Challenge(now)
	if err != nil {
		return nil, err
	}
	instance, _ := p.party.instance.Value()
	result := &authv1.Fido2AssertionChallenge{Challenge: challenge, RpId: p.party.rpID, Origin: p.party.origin, InstanceId: &authv1.InstanceId{Value: instance}}
	for _, id := range session.AllowedCredentialIDs {
		result.AllowedCredentialIds = append(result.AllowedCredentialIds, append([]byte(nil), id...))
	}
	return result, nil
}
func (p PreparedCeremony) EnrollmentOptions(user UserSnapshot, now time.Time) (*authv1.Fido2EnrollmentChallenge, error) {
	if p.preparedCeremonyValue == nil || p.kind != CeremonyEnrollment || !user.validFor(p.binding, p.party) {
		return nil, errorOf(ErrorInvalidInput)
	}
	challenge, err := p.Challenge(now)
	if err != nil {
		return nil, err
	}
	instance, _ := p.party.instance.Value()
	return &authv1.Fido2EnrollmentChallenge{Challenge: challenge, RpId: p.party.rpID, Origin: p.party.origin, InstanceId: &authv1.InstanceId{Value: instance}, UserId: user.WebAuthnID(), UserName: user.WebAuthnName()}, nil
}

func (v VerifiedEnrollment) CeremonyDigest() ([32]byte, error) {
	if _, err := v.Binding(); err != nil {
		return [32]byte{}, err
	}
	return v.ceremonyDigest, nil
}
func (p PreparedCeremony) Digest() ([32]byte, error) {
	raw, err := p.Bytes()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
