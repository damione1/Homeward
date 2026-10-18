package fido2_test

import (
	"bytes"
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
	ctapcrypto "github.com/go-ctap/ctaphid/pkg/crypto"
	"github.com/go-ctap/ctaphid/pkg/crypto/protocoltwo"
	ctapclient "github.com/go-ctap/ctaphid/pkg/ctap"
	"github.com/go-ctap/ctaphid/pkg/ctaphid"
	"github.com/go-ctap/ctaphid/pkg/ctaptypes"
	"github.com/go-ctap/ctaphid/pkg/webauthntypes"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/ldclabs/cose/iana"
	coseecdh "github.com/ldclabs/cose/key/ecdh"
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
	condition    fakeCondition
	cancelled    bool
	request      []byte
	want         int
	command      ctaphid.Command
	response     bytes.Buffer
	credential   *ecdsa.PrivateKey
	agreement    *ecdh.PrivateKey
	credentialID []byte
	token        []byte
	counter      uint32
	reading      chan struct{}
	cancelSignal chan struct{}
	cancelOnce   sync.Once
	readStarted  bool
}

func (f *fakeAuthenticator) releasePending() {
	if f.cancelSignal != nil {
		f.cancelOnce.Do(func() { close(f.cancelSignal) })
	}
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
	}
}

// Each Write is one CTAPHID report. The fake parses wire framing rather than
// returning client-layer structs, so real ctap.Client encoders and decoders run.
func (f *fakeAuthenticator) Write(report []byte) (int, error) {
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
			if f.cancelSignal == nil {
				return 0, fmt.Errorf("pending transport has no cancellation signal")
			}
			if err := f.respond(ctaphid.CTAP2_ERR_KEEPALIVE_CANCEL, nil); err != nil {
				return 0, err
			}
			f.releasePending()
		}
		return len(report), nil
	}
	if f.command != ctaphid.CTAPHID_CBOR {
		return 0, fmt.Errorf("unexpected HID command %d", f.command)
	}
	if len(request) == 0 {
		return 0, fmt.Errorf("empty CTAP command")
	}
	status, body, err := f.handle(ctaptypes.Command(request[0]), request[1:])
	if err != nil {
		return 0, err
	}
	if f.condition == fakeRemoved {
		return len(report), nil
	}
	if f.condition == fakePendingTouch {
		return len(report), nil
	}
	return len(report), f.respond(status, body)
}

func (f *fakeAuthenticator) respond(status ctaphid.StatusCode, body []byte) error {
	msg, err := ctaphid.NewMessage(channelID, ctaphid.CTAPHID_CBOR, append([]byte{byte(status)}, body...))
	if err != nil {
		return err
	}
	_, err = msg.WriteTo(reportWriter{&f.response})
	return err
}

func (f *fakeAuthenticator) Read(dst []byte) (int, error) {
	if f.condition == fakeRemoved {
		return 0, io.EOF
	}
	if f.condition == fakePendingTouch {
		if f.reading == nil || f.cancelSignal == nil {
			return 0, fmt.Errorf("pending transport missing lifecycle channels")
		}
		if !f.readStarted {
			f.readStarted = true
			close(f.reading)
		}
		<-f.cancelSignal
	}
	return f.response.Read(dst)
}

type reportWriter struct{ buffer *bytes.Buffer }

func (w reportWriter) Write(report []byte) (int, error) {
	if len(report) == 0 || report[0] != 0 {
		return 0, fmt.Errorf("invalid generated report")
	}
	_, err := w.buffer.Write(report[1:])
	return len(report), err
}

func (f *fakeAuthenticator) handle(command ctaptypes.Command, input []byte) (ctaphid.StatusCode, []byte, error) {
	if f.cancelled {
		return ctaphid.CTAP2_ERR_KEEPALIVE_CANCEL, nil, nil
	}
	switch command {
	case ctaptypes.AuthenticatorClientPIN:
		var req ctaptypes.AuthenticatorClientPINRequest
		if err := cbor.Unmarshal(input, &req); err != nil {
			return 0, nil, err
		}
		return f.clientPIN(req)
	case ctaptypes.AuthenticatorMakeCredential:
		var req ctaptypes.AuthenticatorMakeCredentialRequest
		if err := cbor.Unmarshal(input, &req); err != nil {
			return 0, nil, err
		}
		if len(req.PinUvAuthParam) == 0 {
			return ctaphid.CTAP2_ERR_PUAT_REQUIRED, nil, nil
		}
		if req.RP.ID != rpID || !bytes.Equal(req.PinUvAuthParam, ctapcrypto.Authenticate(ctaptypes.PinUvAuthProtocolTwo, f.token, req.ClientDataHash)) {
			return ctaphid.CTAP2_ERR_PIN_AUTH_INVALID, nil, nil
		}
		if f.condition == fakeNoTouch {
			return ctaphid.CTAP2_ERR_USER_ACTION_TIMEOUT, nil, nil
		}
		data, err := f.authData(true)
		if err != nil {
			return 0, nil, err
		}
		body, err := cbor.Marshal(map[uint64]any{1: "none", 2: data, 3: map[string]any{}})
		return ctaphid.CTAP2_OK, body, err
	case ctaptypes.AuthenticatorGetAssertion:
		var req ctaptypes.AuthenticatorGetAssertionRequest
		if err := cbor.Unmarshal(input, &req); err != nil {
			return 0, nil, err
		}
		if req.RPID != rpID || !bytes.Equal(req.PinUvAuthParam, ctapcrypto.Authenticate(ctaptypes.PinUvAuthProtocolTwo, f.token, req.ClientDataHash)) {
			return ctaphid.CTAP2_ERR_PIN_AUTH_INVALID, nil, nil
		}
		if len(req.AllowList) != 1 || !bytes.Equal(req.AllowList[0].ID, f.credentialID) {
			return ctaphid.CTAP2_ERR_NO_CREDENTIALS, nil, nil
		}
		if f.condition == fakeNoTouch {
			return ctaphid.CTAP2_ERR_USER_ACTION_TIMEOUT, nil, nil
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
			1: webauthntypes.PublicKeyCredentialDescriptor{Type: webauthntypes.PublicKeyCredentialTypePublicKey, ID: f.credentialID},
			2: data, 3: sig,
		})
		return ctaphid.CTAP2_OK, body, err
	default:
		return ctaphid.CTAP1_ERR_INVALID_COMMAND, nil, nil
	}
}

func (f *fakeAuthenticator) clientPIN(req ctaptypes.AuthenticatorClientPINRequest) (ctaphid.StatusCode, []byte, error) {
	switch req.SubCommand {
	case ctaptypes.ClientPINSubCommandGetKeyAgreement:
		public, err := coseecdh.KeyFromPublic(f.agreement.PublicKey())
		if err != nil {
			return 0, nil, err
		}
		delete(public, iana.KeyParameterKid)
		public[iana.KeyParameterAlg] = -25
		body, err := cbor.Marshal(ctaptypes.AuthenticatorClientPINResponse{KeyAgreement: public})
		return ctaphid.CTAP2_OK, body, err
	case ctaptypes.ClientPINSubCommandGetPinUvAuthTokenUsingPinWithPermissions:
		peer, err := coseecdh.KeyToPublic(req.KeyAgreement)
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
			return ctaphid.CTAP2_ERR_PIN_INVALID, nil, nil
		}
		if req.RPID != rpID || req.Permissions == ctaptypes.PermissionNone {
			return ctaphid.CTAP2_ERR_UNAUTHORIZED_PERMISSION, nil, nil
		}
		encToken, err := protocoltwo.Encrypt(shared, f.token)
		if err != nil {
			return 0, nil, err
		}
		body, err := cbor.Marshal(ctaptypes.AuthenticatorClientPINResponse{PinUvAuthToken: encToken})
		return ctaphid.CTAP2_OK, body, err
	default:
		return ctaphid.CTAP2_ERR_INVALID_SUBCOMMAND, nil, nil
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

func pinToken(t *testing.T, client *ctapclient.Client, device *fakeAuthenticator, suppliedPIN string, permission ctaptypes.Permission) ([]byte, error) {
	t.Helper()
	agreement, err := client.GetKeyAgreement(device, channelID, ctaptypes.PinUvAuthProtocolTwo)
	if err != nil {
		return nil, err
	}
	return client.GetPinUvAuthTokenUsingPinWithPermissions(device, channelID, ctaptypes.PinUvAuthProtocolTwo, agreement, suppliedPIN, permission, rpID)
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

func firstAssertion(client *ctapclient.Client, device *fakeAuthenticator, token, clientDataJSON, credentialID []byte) (*ctaptypes.AuthenticatorGetAssertionResponse, error) {
	for assertion, err := range client.GetAssertion(device, channelID, ctaptypes.PinUvAuthProtocolTwo, token, rpID, clientDataJSON,
		[]webauthntypes.PublicKeyCredentialDescriptor{{Type: webauthntypes.PublicKeyCredentialTypePublicKey, ID: credentialID}}, nil,
		map[ctaptypes.Option]bool{ctaptypes.OptionUserPresence: true, ctaptypes.OptionUserVerification: true}) {
		return assertion, err
	}
	return nil, fmt.Errorf("no assertion returned")
}

func TestNHA140CryptoRoundTripAndTampering(t *testing.T) {
	server, user, client, device := newServer(t), &testUser{}, ctapclient.NewClient(), newFake(t)
	_, registrationSession, err := server.BeginRegistration(user)
	if err != nil {
		t.Fatal(err)
	}
	registrationClientData := encodeClientData(t, "webauthn.create", registrationSession.Challenge)
	token, err := pinToken(t, client, device, pin, ctaptypes.PermissionMakeCredential)
	if err != nil {
		t.Fatalf("correct PIN token: %v", err)
	}
	created, err := client.MakeCredential(device, channelID, ctaptypes.PinUvAuthProtocolTwo, token, registrationClientData,
		webauthntypes.PublicKeyCredentialRpEntity{ID: rpID, Name: "Homeward"},
		webauthntypes.PublicKeyCredentialUserEntity{ID: user.WebAuthnID(), Name: user.WebAuthnName(), DisplayName: user.WebAuthnDisplayName()},
		[]webauthntypes.PublicKeyCredentialParameters{{Type: webauthntypes.PublicKeyCredentialTypePublicKey, Algorithm: -7}},
		nil, nil, map[ctaptypes.Option]bool{ctaptypes.OptionUserPresence: true, ctaptypes.OptionUserVerification: true}, 0, nil)
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
	loginToken, err := pinToken(t, client, device, pin, ctaptypes.PermissionGetAssertion)
	if err != nil {
		t.Fatalf("correct PIN login token: %v", err)
	}
	assertion, err := firstAssertion(client, device, loginToken, loginClientData, credential.ID)
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

func expectCTAPStatus(t *testing.T, err error, status ctaphid.StatusCode) {
	t.Helper()
	var typed *ctaphid.CTAPError
	if !errors.As(err, &typed) || typed.StatusCode != status {
		t.Fatalf("wanted CTAP status %v, got %v", status, err)
	}
}

func TestNHA140NativeFailureBranches(t *testing.T) {
	clientDataJSON := []byte(`{"type":"webauthn.get","challenge":"opaque-challenge","origin":"https://homeward.test"}`)
	t.Run("pin_required", func(t *testing.T) {
		client, device := ctapclient.NewClient(), newFake(t)
		_, err := client.MakeCredential(device, channelID, ctaptypes.PinUvAuthProtocolTwo, nil, clientDataJSON,
			webauthntypes.PublicKeyCredentialRpEntity{ID: rpID}, webauthntypes.PublicKeyCredentialUserEntity{ID: []byte("user")}, nil, nil, nil, nil, 0, nil)
		expectCTAPStatus(t, err, ctaphid.CTAP2_ERR_PUAT_REQUIRED)
	})
	t.Run("pin_incorrect", func(t *testing.T) {
		client, device := ctapclient.NewClient(), newFake(t)
		_, err := pinToken(t, client, device, "wrong-pin", ctaptypes.PermissionGetAssertion)
		expectCTAPStatus(t, err, ctaphid.CTAP2_ERR_PIN_INVALID)
	})
	for _, tc := range []struct {
		name      string
		condition fakeCondition
		cancelled bool
		want      ctaphid.StatusCode
	}{
		{"touch_absent", fakeNoTouch, false, ctaphid.CTAP2_ERR_USER_ACTION_TIMEOUT},
		{"cancelled", fakeReady, true, ctaphid.CTAP2_ERR_KEEPALIVE_CANCEL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, device := ctapclient.NewClient(), newFake(t)
			device.condition = tc.condition
			if tc.cancelled {
				if err := ctaphid.Cancel(device, channelID); err != nil {
					t.Fatal(err)
				}
			}
			_, err := firstAssertion(client, device, device.token, clientDataJSON, device.credentialID)
			expectCTAPStatus(t, err, tc.want)
		})
	}
	t.Run("transport_removed", func(t *testing.T) {
		client, device := ctapclient.NewClient(), newFake(t)
		device.condition = fakeRemoved
		_, err := firstAssertion(client, device, device.token, clientDataJSON, device.credentialID)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected disconnected transport EOF, got %v", err)
		}
	})
	t.Run("cancel_in_flight", func(t *testing.T) {
		client, device := ctapclient.NewClient(), newFake(t)
		device.condition = fakePendingTouch
		device.reading = make(chan struct{})
		device.cancelSignal = make(chan struct{})
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
			_, err := firstAssertion(client, device, device.token, clientDataJSON, device.credentialID)
			result <- err
		}()
		select {
		case <-device.reading:
		case <-time.After(5 * time.Second):
			t.Fatal("CTAP client did not enter the transport read")
		}
		if err := ctaphid.Cancel(device, channelID); err != nil {
			t.Fatalf("send CTAPHID_CANCEL: %v", err)
		}
		select {
		case err := <-result:
			expectCTAPStatus(t, err, ctaphid.CTAP2_ERR_KEEPALIVE_CANCEL)
		case <-time.After(5 * time.Second):
			t.Fatal("CTAP client did not finish after cancellation")
		}
	})
}
