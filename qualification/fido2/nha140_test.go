package fido2_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	ctapclient "github.com/telesma-app/ctap/client"
	"github.com/telesma-app/ctap/cose"
	"github.com/telesma-app/ctap/credential"
	ctapcrypto "github.com/telesma-app/ctap/crypto"
	"github.com/telesma-app/ctap/crypto/protocoltwo"
	"github.com/telesma-app/ctap/options"
	ctapprotocol "github.com/telesma-app/ctap/protocol"
	ctaptransport "github.com/telesma-app/ctap/transport"
	"github.com/telesma-app/ctap/transport/ctaphid"
)

const (
	rpID   = "homeward.test"
	origin = "https://homeward.test"
	pin    = "123456"
)

var channelID = ctaphid.ChannelID{1, 2, 3, 4}

type fakeCondition uint8

const (
	fakeReady fakeCondition = iota
	fakeNoTouch
	fakeRemoved
	fakePendingTouch
)

type fakeAuthenticator struct {
	mu           sync.Mutex
	condition    fakeCondition
	cancelled    bool
	request      []byte
	want         int
	command      ctaphid.Command
	reports      chan []byte
	closed       chan struct{}
	removed      chan struct{}
	closeOnce    sync.Once
	removeOnce   sync.Once
	pendingOnce  sync.Once
	readOnce     sync.Once
	releaseOnce  sync.Once
	credential   *ecdsa.PrivateKey
	agreement    *ecdh.PrivateKey
	credentialID []byte
	token        []byte
	counter      uint32
	pending      chan struct{}
	reading      chan struct{}
	readRelease  chan struct{}
}

func (f *fakeAuthenticator) releasePending() {
	f.removeOnce.Do(func() { close(f.removed) })
	f.releaseRead()
}

func (f *fakeAuthenticator) releaseRead() {
	f.releaseOnce.Do(func() { close(f.readRelease) })
}

func (f *fakeAuthenticator) setCondition(condition fakeCondition) {
	f.mu.Lock()
	f.condition = condition
	f.mu.Unlock()
	if condition == fakeRemoved {
		f.releasePending()
	}
}

func (f *fakeAuthenticator) wasCancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

func (f *fakeAuthenticator) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func newFake(t *testing.T) *fakeAuthenticator {
	t.Helper()
	credential, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agreement, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeAuthenticator{
		condition: fakeReady, credential: credential, agreement: agreement,
		credentialID: []byte("nha140-credential-001"), token: bytes.Repeat([]byte{0x72}, 32),
		reports: make(chan []byte, 128), closed: make(chan struct{}), removed: make(chan struct{}),
		pending: make(chan struct{}), reading: make(chan struct{}), readRelease: make(chan struct{}),
	}
}

func newClient(t *testing.T, device *fakeAuthenticator) (*ctapclient.Client, *ctaphid.Transport) {
	t.Helper()
	transport := ctaphid.NewTransport(device, channelID)
	client, err := ctapclient.NewClient(options.WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transport.Close(); err != nil {
			t.Errorf("close fake HID transport: %v", err)
		}
	})
	return client, transport
}

// Each Write is one CTAPHID report. The fake parses wire framing rather than
// returning client-layer structs, so real ctap.Client encoders and decoders run.
func (f *fakeAuthenticator) Write(ctx context.Context, report []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(report) < 6 || report[0] != 0 {
		return 0, fmt.Errorf("invalid HID report")
	}
	packet := report[1:]
	if packet[4]&0x80 != 0 {
		if len(packet) < 7 {
			return 0, io.ErrShortWrite
		}
		f.command = ctaphid.Command(packet[4] & 0x7f)
		f.want = int(binary.BigEndian.Uint16(packet[5:7]))
		f.request = append(f.request[:0], packet[7:]...)
	} else {
		f.request = append(f.request, packet[5:]...)
	}
	if len(f.request) < f.want {
		return len(report), nil
	}
	request := bytes.Clone(f.request[:f.want])
	f.request = nil
	if f.command == ctaphid.CTAPHID_CANCEL {
		f.cancelled = true
		if f.condition == fakePendingTouch {
			if err := f.respond(ctaptransport.CTAP2_ERR_KEEPALIVE_CANCEL, nil); err != nil {
				return 0, err
			}
			f.releaseRead()
		}
		return len(report), nil
	}
	if f.command != ctaphid.CTAPHID_CBOR {
		return 0, fmt.Errorf("unexpected HID command %d", f.command)
	}
	if len(request) == 0 {
		return 0, fmt.Errorf("empty CTAP command")
	}
	status, body, err := f.handle(ctapprotocol.Command(request[0]), request[1:])
	if err != nil {
		return 0, err
	}
	if f.condition == fakeRemoved {
		return len(report), nil
	}
	if f.condition == fakePendingTouch {
		f.pendingOnce.Do(func() { close(f.pending) })
		return len(report), nil
	}
	return len(report), f.respond(status, body)
}

func (f *fakeAuthenticator) respond(status ctaptransport.StatusCode, body []byte) error {
	msg, err := ctaphid.NewMessage(channelID, ctaphid.CTAPHID_CBOR, append([]byte{byte(status)}, body...))
	if err != nil {
		return err
	}
	for _, report := range msg.OutputReports() {
		select {
		case f.reports <- bytes.Clone(report[1:]):
		default:
			return fmt.Errorf("fake HID report queue full")
		}
	}
	return nil
}

func (f *fakeAuthenticator) Read(ctx context.Context, dst []byte) (int, error) {
	f.mu.Lock()
	pending := f.condition == fakePendingTouch
	f.mu.Unlock()
	if pending {
		f.readOnce.Do(func() { close(f.reading) })
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-f.closed:
			return 0, io.ErrClosedPipe
		case <-f.removed:
			return 0, io.EOF
		case <-f.readRelease:
		}
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-f.closed:
		return 0, io.ErrClosedPipe
	case <-f.removed:
		return 0, io.EOF
	case report := <-f.reports:
		return copy(dst, report), nil
	}
}

func (f *fakeAuthenticator) handle(command ctapprotocol.Command, input []byte) (ctaptransport.StatusCode, []byte, error) {
	if f.cancelled {
		return ctaptransport.CTAP2_ERR_KEEPALIVE_CANCEL, nil, nil
	}
	switch command {
	case ctapprotocol.AuthenticatorClientPIN:
		var req ctapprotocol.AuthenticatorClientPINRequest
		if err := cbor.Unmarshal(input, &req); err != nil {
			return 0, nil, err
		}
		return f.clientPIN(req)
	case ctapprotocol.AuthenticatorMakeCredential:
		var req ctapprotocol.AuthenticatorMakeCredentialRequest
		if err := cbor.Unmarshal(input, &req); err != nil {
			return 0, nil, err
		}
		if len(req.PinUvAuthParam) == 0 {
			return ctaptransport.CTAP2_ERR_PUAT_REQUIRED, nil, nil
		}
		if req.RP.ID != rpID || !bytes.Equal(req.PinUvAuthParam, ctapcrypto.Authenticate(ctapprotocol.PinUvAuthProtocolTwo, f.token, req.ClientDataHash)) {
			return ctaptransport.CTAP2_ERR_PIN_AUTH_INVALID, nil, nil
		}
		if f.condition == fakeNoTouch {
			return ctaptransport.CTAP2_ERR_USER_ACTION_TIMEOUT, nil, nil
		}
		data, err := f.authData(true)
		if err != nil {
			return 0, nil, err
		}
		body, err := cbor.Marshal(map[uint64]any{1: "none", 2: data, 3: map[string]any{}})
		return ctaptransport.CTAP2_OK, body, err
	case ctapprotocol.AuthenticatorGetAssertion:
		var req ctapprotocol.AuthenticatorGetAssertionRequest
		if err := cbor.Unmarshal(input, &req); err != nil {
			return 0, nil, err
		}
		if req.RPID != rpID || !bytes.Equal(req.PinUvAuthParam, ctapcrypto.Authenticate(ctapprotocol.PinUvAuthProtocolTwo, f.token, req.ClientDataHash)) {
			return ctaptransport.CTAP2_ERR_PIN_AUTH_INVALID, nil, nil
		}
		if len(req.AllowList) != 1 || !bytes.Equal(req.AllowList[0].ID, f.credentialID) {
			return ctaptransport.CTAP2_ERR_NO_CREDENTIALS, nil, nil
		}
		if f.condition == fakeNoTouch {
			return ctaptransport.CTAP2_ERR_USER_ACTION_TIMEOUT, nil, nil
		}
		data, err := f.authData(false)
		if err != nil {
			return 0, nil, err
		}
		digest := sha256.Sum256(append(bytes.Clone(data), req.ClientDataHash...))
		sig, err := ecdsa.SignASN1(rand.Reader, f.credential, digest[:])
		if err != nil {
			return 0, nil, err
		}
		body, err := cbor.Marshal(map[uint64]any{
			1: credential.PublicKeyCredentialDescriptor{Type: credential.PublicKeyCredentialTypePublicKey, ID: f.credentialID},
			2: data, 3: sig,
		})
		return ctaptransport.CTAP2_OK, body, err
	default:
		return ctaptransport.CTAP1_ERR_INVALID_COMMAND, nil, nil
	}
}

func (f *fakeAuthenticator) clientPIN(req ctapprotocol.AuthenticatorClientPINRequest) (ctaptransport.StatusCode, []byte, error) {
	switch req.SubCommand {
	case ctapprotocol.ClientPINSubCommandGetKeyAgreement:
		public, err := cose.KeyFromP256PublicKey(f.agreement.PublicKey())
		if err != nil {
			return 0, nil, err
		}
		body, err := cbor.Marshal(ctapprotocol.AuthenticatorClientPINResponse{KeyAgreement: public})
		return ctaptransport.CTAP2_OK, body, err
	case ctapprotocol.ClientPINSubCommandGetPinUvAuthTokenUsingPinWithPermissions:
		peer, err := req.KeyAgreement.P256PublicKey()
		if err != nil {
			return 0, nil, err
		}
		z, err := f.agreement.ECDH(peer)
		if err != nil {
			return 0, nil, err
		}
		shared, err := protocoltwo.KDF(z)
		if err != nil {
			return 0, nil, err
		}
		gotHash, err := protocoltwo.Decrypt(shared, req.PinHashEnc)
		if err != nil {
			return 0, nil, err
		}
		wantHash := sha256.Sum256([]byte(pin))
		if len(gotHash) < 16 || !bytes.Equal(gotHash[:16], wantHash[:16]) {
			return ctaptransport.CTAP2_ERR_PIN_INVALID, nil, nil
		}
		if req.RPID != rpID || req.Permissions == ctapprotocol.PermissionNone {
			return ctaptransport.CTAP2_ERR_UNAUTHORIZED_PERMISSION, nil, nil
		}
		encToken, err := protocoltwo.Encrypt(shared, f.token)
		if err != nil {
			return 0, nil, err
		}
		body, err := cbor.Marshal(ctapprotocol.AuthenticatorClientPINResponse{PinUvAuthToken: encToken})
		return ctaptransport.CTAP2_OK, body, err
	default:
		return ctaptransport.CTAP2_ERR_INVALID_SUBCOMMAND, nil, nil
	}
}

func (f *fakeAuthenticator) authData(attested bool) ([]byte, error) {
	hash := sha256.Sum256([]byte(rpID))
	flags := byte(0x05) // UP + UV
	if attested {
		flags |= 0x40
	}
	f.counter++
	data := append(bytes.Clone(hash[:]), flags, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(data[33:37], f.counter)
	if !attested {
		return data, nil
	}
	data = append(data, make([]byte, 16)...)
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(f.credentialID)))
	data = append(data, length...)
	data = append(data, f.credentialID...)
	public := f.credential.PublicKey
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: public.X.FillBytes(make([]byte, 32)), -3: public.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		return nil, err
	}
	return append(data, cose...), nil
}

type testUser struct{ credential []webauthn.Credential }

func (u *testUser) WebAuthnID() []byte                         { return []byte("nha140-user-id") }
func (u *testUser) WebAuthnName() string                       { return "tester" }
func (u *testUser) WebAuthnDisplayName() string                { return "NHA-140 Tester" }
func (u *testUser) WebAuthnCredentials() []webauthn.Credential { return u.credential }

type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

func newServer(t *testing.T) *webauthn.WebAuthn {
	t.Helper()
	server, err := webauthn.New(&webauthn.Config{
		RPID: rpID, RPDisplayName: "Homeward FIDO2 qualification", RPOrigins: []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func encodeClientData(t *testing.T, kind, challenge string) []byte {
	t.Helper()
	data, err := json.Marshal(clientData{Type: kind, Challenge: challenge, Origin: origin})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func pinToken(t *testing.T, client *ctapclient.Client, suppliedPIN string, permission ctapprotocol.Permission) ([]byte, error) {
	t.Helper()
	agreement, err := client.GetKeyAgreement(context.Background(), ctapprotocol.PinUvAuthProtocolTwo)
	if err != nil {
		return nil, err
	}
	return client.GetPinUvAuthTokenUsingPinWithPermissions(context.Background(), ctapprotocol.PinUvAuthProtocolTwo, agreement, suppliedPIN, permission, rpID)
}

func registrationResponse(t *testing.T, clientDataJSON []byte, id, authData []byte) []byte {
	t.Helper()
	attestation, err := cbor.Marshal(map[string]any{"fmt": "none", "authData": authData, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{
		"id": base64.RawURLEncoding.EncodeToString(id), "rawId": base64.RawURLEncoding.EncodeToString(id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": base64.RawURLEncoding.EncodeToString(clientDataJSON), "attestationObject": base64.RawURLEncoding.EncodeToString(attestation)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type assertionWire struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response struct {
		ClientDataJSON    string `json:"clientDataJSON"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
	} `json:"response"`
}

func assertionResponse(t *testing.T, clientDataJSON []byte, id, authData, sig []byte) assertionWire {
	t.Helper()
	encodedID := base64.RawURLEncoding.EncodeToString(id)
	wire := assertionWire{ID: encodedID, RawID: encodedID, Type: "public-key"}
	wire.Response.ClientDataJSON = base64.RawURLEncoding.EncodeToString(clientDataJSON)
	wire.Response.AuthenticatorData = base64.RawURLEncoding.EncodeToString(authData)
	wire.Response.Signature = base64.RawURLEncoding.EncodeToString(sig)
	return wire
}

func parseAssertion(t *testing.T, wire assertionWire) *protocol.ParsedCredentialAssertionData {
	t.Helper()
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func decodeWireBytes(t *testing.T, encoded string) []byte {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Re-sign changed data so a verifier cannot reject it solely because the
// original signature no longer matches. Each negative case isolates one rule.
func resignAssertion(t *testing.T, wire *assertionWire, private *ecdsa.PrivateKey) {
	t.Helper()
	authData := decodeWireBytes(t, wire.Response.AuthenticatorData)
	clientDataJSON := decodeWireBytes(t, wire.Response.ClientDataJSON)
	clientHash := sha256.Sum256(clientDataJSON)
	digest := sha256.Sum256(append(bytes.Clone(authData), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	wire.Response.Signature = base64.RawURLEncoding.EncodeToString(signature)
}

func expectWebAuthnFailure(t *testing.T, err error, wantType, wantDetail string) {
	t.Helper()
	var typed *protocol.Error
	if !errors.As(err, &typed) {
		t.Fatalf("wanted WebAuthn %s, got %v", wantType, err)
	}
	if typed.Type != wantType || !strings.Contains(typed.Details+typed.DevInfo, wantDetail) {
		t.Fatalf("wanted WebAuthn %s mentioning %q, got type=%q details=%q info=%q", wantType, wantDetail, typed.Type, typed.Details, typed.DevInfo)
	}
}

func firstAssertion(ctx context.Context, client *ctapclient.Client, token, clientDataJSON, credentialID []byte) (*ctapprotocol.AuthenticatorGetAssertionResponse, error) {
	hash := sha256.Sum256(clientDataJSON)
	for assertion, err := range client.GetAssertion(ctx, ctapprotocol.PinUvAuthProtocolTwo, token, rpID, hash[:],
		[]credential.PublicKeyCredentialDescriptor{{Type: credential.PublicKeyCredentialTypePublicKey, ID: credentialID}}, nil,
		map[ctapprotocol.Option]bool{ctapprotocol.OptionUserPresence: true, ctapprotocol.OptionUserVerification: true}) {
		return &assertion, err
	}
	return nil, fmt.Errorf("no assertion returned")
}

func TestNHA172CryptoRoundTripAndTampering(t *testing.T) {
	server, user, device := newServer(t), &testUser{}, newFake(t)
	client, _ := newClient(t, device)
	_, registrationSession, err := server.BeginRegistration(user)
	if err != nil {
		t.Fatal(err)
	}
	registrationClientData := encodeClientData(t, "webauthn.create", registrationSession.Challenge)
	token, err := pinToken(t, client, pin, ctapprotocol.PermissionMakeCredential)
	if err != nil {
		t.Fatalf("correct PIN token: %v", err)
	}
	registrationHash := sha256.Sum256(registrationClientData)
	created, err := client.MakeCredential(context.Background(), ctapprotocol.PinUvAuthProtocolTwo, token, registrationHash[:],
		credential.PublicKeyCredentialRpEntity{ID: rpID, Name: "Homeward"},
		credential.PublicKeyCredentialUserEntity{ID: user.WebAuthnID(), Name: user.WebAuthnName(), DisplayName: user.WebAuthnDisplayName()},
		[]credential.PublicKeyCredentialParameters{{Type: credential.PublicKeyCredentialTypePublicKey, Algorithm: cose.AlgorithmES256}},
		nil, nil, map[ctapprotocol.Option]bool{ctapprotocol.OptionUserPresence: true, ctapprotocol.OptionUserVerification: true}, 0, nil)
	if err != nil {
		t.Fatalf("CTAP MakeCredential: %v", err)
	}
	parsedRegistration, err := protocol.ParseCredentialCreationResponseBytes(registrationResponse(t, registrationClientData, device.credentialID, created.AuthDataRaw))
	if err != nil {
		t.Fatalf("parse registration: %v", err)
	}
	credential, err := server.CreateCredential(user, *registrationSession, parsedRegistration)
	if err != nil {
		t.Fatalf("verify registration: %v", err)
	}
	user.credential = []webauthn.Credential{*credential}
	_, loginSession, err := server.BeginLogin(user)
	if err != nil {
		t.Fatal(err)
	}
	loginClientData := encodeClientData(t, "webauthn.get", loginSession.Challenge)
	loginToken, err := pinToken(t, client, pin, ctapprotocol.PermissionGetAssertion)
	if err != nil {
		t.Fatalf("correct PIN login token: %v", err)
	}
	assertion, err := firstAssertion(context.Background(), client, loginToken, loginClientData, credential.ID)
	if err != nil {
		t.Fatalf("CTAP GetAssertion: %v", err)
	}
	wire := assertionResponse(t, loginClientData, credential.ID, assertion.AuthDataRaw, assertion.Signature)
	if _, err := server.ValidateLogin(user, *loginSession, parseAssertion(t, wire)); err != nil {
		t.Fatalf("verify signed assertion: %v", err)
	}

	for _, tc := range []struct {
		name       string
		alter      func(*assertionWire)
		resign     bool
		wantType   string
		wantDetail string
	}{
		{"challenge", func(w *assertionWire) {
			w.Response.ClientDataJSON = base64.RawURLEncoding.EncodeToString(encodeClientData(t, "webauthn.get", base64.RawURLEncoding.EncodeToString([]byte("another-challenge-long-enough"))))
		}, true, protocol.ErrVerification.Type, "Error validating challenge"},
		{"rp_id_hash", func(w *assertionWire) {
			b := decodeWireBytes(t, w.Response.AuthenticatorData)
			otherRP := sha256.Sum256([]byte("other.homeward.test"))
			copy(b[:32], otherRP[:])
			w.Response.AuthenticatorData = base64.RawURLEncoding.EncodeToString(b)
		}, true, protocol.ErrVerification.Type, "RP Hash mismatch"},
		{"uv_bit", func(w *assertionWire) {
			b := decodeWireBytes(t, w.Response.AuthenticatorData)
			b[32] &^= 4
			w.Response.AuthenticatorData = base64.RawURLEncoding.EncodeToString(b)
		}, true, protocol.ErrVerification.Type, "User verification required"},
		{"signature", func(w *assertionWire) {
			b := decodeWireBytes(t, w.Response.Signature)
			b[len(b)-1] ^= 1
			w.Response.Signature = base64.RawURLEncoding.EncodeToString(b)
		}, false, protocol.ErrAssertionSignature.Type, "Error validating the assertion signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			altered := wire
			tc.alter(&altered)
			if tc.resign {
				resignAssertion(t, &altered, device.credential)
			}
			if _, err := server.ValidateLogin(user, *loginSession, parseAssertion(t, altered)); err == nil {
				t.Fatal("altered assertion accepted")
			} else {
				expectWebAuthnFailure(t, err, tc.wantType, tc.wantDetail)
			}
		})
	}
}

func expectCTAPStatus(t *testing.T, err error, status ctaptransport.StatusCode) {
	t.Helper()
	var typed *ctaptransport.CTAPError
	if !errors.As(err, &typed) || typed.StatusCode != status {
		t.Fatalf("wanted CTAP status %v, got %v", status, err)
	}
}

func TestNHA172NativeFailureBranches(t *testing.T) {
	clientDataJSON := []byte(`{"type":"webauthn.get","challenge":"opaque-challenge","origin":"https://homeward.test"}`)
	clientHash := sha256.Sum256(clientDataJSON)
	t.Run("pin_required", func(t *testing.T) {
		device := newFake(t)
		client, _ := newClient(t, device)
		_, err := client.MakeCredential(context.Background(), ctapprotocol.PinUvAuthProtocolTwo, nil, clientHash[:],
			credential.PublicKeyCredentialRpEntity{ID: rpID}, credential.PublicKeyCredentialUserEntity{ID: []byte("user")}, nil, nil, nil, nil, 0, nil)
		expectCTAPStatus(t, err, ctaptransport.CTAP2_ERR_PUAT_REQUIRED)
	})
	t.Run("pin_incorrect", func(t *testing.T) {
		device := newFake(t)
		client, _ := newClient(t, device)
		_, err := pinToken(t, client, "wrong-pin", ctapprotocol.PermissionGetAssertion)
		expectCTAPStatus(t, err, ctaptransport.CTAP2_ERR_PIN_INVALID)
	})
	for _, tc := range []struct {
		name      string
		condition fakeCondition
		cancelled bool
		want      ctaptransport.StatusCode
	}{
		{"touch_absent", fakeNoTouch, false, ctaptransport.CTAP2_ERR_USER_ACTION_TIMEOUT},
		{"cancelled_before_command", fakeReady, true, ctaptransport.CTAP2_ERR_KEEPALIVE_CANCEL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := newFake(t)
			client, transport := newClient(t, device)
			device.setCondition(tc.condition)
			if tc.cancelled {
				if err := transport.Cancel(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			_, err := firstAssertion(context.Background(), client, device.token, clientDataJSON, device.credentialID)
			expectCTAPStatus(t, err, tc.want)
		})
	}
	t.Run("transport_removed", func(t *testing.T) {
		device := newFake(t)
		client, _ := newClient(t, device)
		device.setCondition(fakeRemoved)
		_, err := firstAssertion(context.Background(), client, device.token, clientDataJSON, device.credentialID)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected disconnected transport EOF, got %v", err)
		}
	})
	t.Run("cancel_in_flight", func(t *testing.T) {
		device := newFake(t)
		device.setCondition(fakePendingTouch)
		client, _ := newClient(t, device)
		select {
		case <-device.reading:
		case <-time.After(5 * time.Second):
			t.Fatal("fake HID did not block in Read")
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		done := make(chan struct{})
		t.Cleanup(func() {
			cancel()
			device.releasePending()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("CTAP client goroutine did not finish during cleanup")
			}
		})
		go func() {
			defer close(done)
			_, err := firstAssertion(ctx, client, device.token, clientDataJSON, device.credentialID)
			result <- err
		}()
		select {
		case <-device.pending:
		case <-time.After(5 * time.Second):
			t.Fatal("CTAP client did not enter the pending HID exchange")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want context cancellation after CTAPHID_CANCEL, got %v", err)
			}
			if !device.wasCancelled() {
				t.Fatal("client did not send CTAPHID_CANCEL")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("CTAP client did not finish after cancellation")
		}
	})
	t.Run("remove_in_flight", func(t *testing.T) {
		device := newFake(t)
		device.setCondition(fakePendingTouch)
		client, _ := newClient(t, device)
		select {
		case <-device.reading:
		case <-time.After(5 * time.Second):
			t.Fatal("fake HID did not block in Read")
		}
		result := make(chan error, 1)
		done := make(chan struct{})
		t.Cleanup(func() {
			device.releasePending()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("CTAP client goroutine did not finish during cleanup")
			}
		})
		go func() {
			defer close(done)
			_, err := firstAssertion(context.Background(), client, device.token, clientDataJSON, device.credentialID)
			result <- err
		}()
		select {
		case <-device.pending:
		case <-time.After(5 * time.Second):
			t.Fatal("CTAP client did not enter the pending HID exchange")
		}
		device.setCondition(fakeRemoved)
		select {
		case err := <-result:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("want in-flight removal EOF, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("CTAP client did not finish after removal")
		}
	})
}
