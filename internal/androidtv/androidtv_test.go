package androidtv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

const testMAC = "02:11:22:33:44:55"

func testPlayer() PlayerInput {
	return PlayerInput{Address: "192.168.50.20", MAC: testMAC, Alias: "media_player.demo", Label: "Demo", CredentialSecret: "paired-remote"}
}
func TestSettingsBoundedLocators(t *testing.T) {
	p := testPlayer()
	s, e := ParseSettings(Input{Instance: "demo-tv", Players: []PlayerInput{p}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Instance(); e != nil {
		t.Fatal(e)
	}
	s.Players()[0].MAC = "bad"
	if s.Players()[0].MAC != testMAC {
		t.Fatal("mutable settings")
	}
	for _, address := range []string{"8.8.8.8", "localhost", "127.0.0.1", "169.254.169.254", "192.168.50.20:6466", "http://192.168.50.20", "::1"} {
		p := testPlayer()
		p.Address = address
		if _, e := ParseSettings(Input{Instance: "demo-tv", Players: []PlayerInput{p}}); e == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	for _, mutate := range []func(*PlayerInput){func(p *PlayerInput) { p.MAC = "ff:ff:ff:ff:ff:ff" }, func(p *PlayerInput) { p.MAC = "00:00:00:00:00:00" }, func(p *PlayerInput) { p.Alias = "switch.demo" }, func(p *PlayerInput) { p.CredentialSecret = "" }, func(p *PlayerInput) { p.Label = "bad\x00label" }} {
		p := testPlayer()
		mutate(&p)
		if _, e := ParseSettings(Input{Instance: "demo-tv", Players: []PlayerInput{p}}); e == nil {
			t.Fatal("accepted invalid settings")
		}
	}
	if _, e := ParseSettings(Input{Instance: "demo-tv", Players: []PlayerInput{p, p}}); e == nil {
		t.Fatal("duplicate player")
	}
}
func testCredential(t *testing.T, cn string, from, to time.Time, usage []x509.ExtKeyUsage) (storage.SecretValue, *x509.Certificate) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: from, NotAfter: to, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	cert, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	k, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(der)
	raw, e := json.Marshal(credentialJSON{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k})), hex.EncodeToString(digest[:])})
	if e != nil {
		t.Fatal(e)
	}
	v, e := storage.NewSecretValue(raw)
	if e != nil {
		t.Fatal(e)
	}
	return v, cert
}
func TestPinnedCertificateAndMACAreMandatory(t *testing.T) {
	now := time.Now()
	value, cert := testCredential(t, "atvremote/test/model/Demo/"+testMAC, now.Add(-time.Hour), now.Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth})
	config, e := credentialTLS(value, testMAC)
	if e != nil {
		t.Fatal(e)
	}
	if config.VerifyConnection == nil || len(config.Certificates) != 1 || config.MinVersion < tls.VersionTLS12 {
		t.Fatal("incomplete TLS policy")
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if e = config.VerifyConnection(state); e != nil {
		t.Fatal(e)
	}
	wrongMAC, e := credentialTLS(value, "02:22:33:44:55:66")
	if e != nil {
		t.Fatal(e)
	}
	if wrongMAC.VerifyConnection(state) == nil {
		t.Fatal("wrong MAC accepted")
	}
	_, other := testCredential(t, "atvremote/test/"+testMAC, now.Add(-time.Hour), now.Add(time.Hour), nil)
	if config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{other}}) == nil {
		t.Fatal("different certificate accepted despite matching MAC")
	}
	if config.VerifyConnection(tls.ConnectionState{}) == nil {
		t.Fatal("missing certificate accepted")
	}
	for _, scenario := range []struct {
		cn       string
		from, to time.Time
		usage    []x509.ExtKeyUsage
	}{{"bad/" + testMAC, now.Add(-time.Hour), now.Add(time.Hour), nil}, {"atvremote/test/" + testMAC, now.Add(-2 * time.Hour), now.Add(-time.Hour), nil}, {"atvremote/test/" + testMAC, now.Add(time.Hour), now.Add(2 * time.Hour), nil}, {"atvremote/test/" + testMAC, now.Add(-time.Hour), now.Add(time.Hour), []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}} {
		v, c := testCredential(t, scenario.cn, scenario.from, scenario.to, scenario.usage)
		cfg, e := credentialTLS(v, testMAC)
		if e != nil {
			t.Fatal(e)
		}
		if cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{c}}) == nil {
			t.Fatal("invalid pinned identity accepted")
		}
	}
	for _, raw := range []string{`{}`, `{"client_key_pem":"canary-private-key"}`, `{"unknown":"value"}`, `{} {}`} {
		v, _ := storage.NewSecretValue([]byte(raw))
		if _, e := credentialTLS(v, testMAC); e != ErrCredential {
			t.Fatalf("invalid credential error %v", e)
		}
	}
}

func TestMTLSHandshakeRejectsUnpinnedPeer(t *testing.T) {
	now := time.Now()
	value, _ := testCredential(t, "atvremote/test/"+testMAC, now.Add(-time.Hour), now.Add(time.Hour), nil)
	config, e := credentialTLS(value, testMAC)
	if e != nil {
		t.Fatal(e)
	}
	otherValue, _ := testCredential(t, "atvremote/test/"+testMAC, now.Add(-time.Hour), now.Add(time.Hour), nil)
	other, e := credentialTLS(otherValue, testMAC)
	if e != nil {
		t.Fatal(e)
	}
	for _, scenario := range []struct {
		name        string
		certificate tls.Certificate
		accepted    bool
	}{
		{"paired", config.Certificates[0], true}, {"different key with same MAC", other.Certificates[0], false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				socket, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer socket.Close()
				server := tls.Server(socket, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{scenario.certificate}, ClientAuth: tls.RequireAnyClientCert, SessionTicketsDisabled: true})
				done <- server.HandshakeContext(ctx)
			}()
			dialer := tls.Dialer{Config: config.Clone()}
			client, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
			if client != nil {
				defer client.Close()
			}
			if (err == nil) != scenario.accepted {
				t.Fatalf("TLS accepted=%v expected=%v: %v", err == nil, scenario.accepted, err)
			}
			if !scenario.accepted && !errors.Is(err, ErrCredential) {
				t.Fatalf("wrong peer was not rejected by its pin: %v", err)
			}
			select {
			case serverErr := <-done:
				if scenario.accepted && serverErr != nil {
					t.Fatal(serverErr)
				}
			case <-ctx.Done():
				t.Fatal("TLS handshake did not terminate")
			}
		})
	}
}

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(1, len(p))]) }
func TestFramingHandlesFragmentedVarintAndRejectsUnboundedFrames(t *testing.T) {
	body := message(100, bytes.Repeat([]byte("x"), 300))
	var wire bytes.Buffer
	if e := writeFrame(&wire, body); e != nil {
		t.Fatal(e)
	}
	got, e := readFrame(bufio.NewReader(oneByteReader{&wire}))
	if e != nil || !bytes.Equal(got, body) {
		t.Fatal("fragmented frame", e)
	}
	for _, b := range [][]byte{{0}, binary.AppendUvarint(nil, maxFrame+1), bytes.Repeat([]byte{0xff}, 11), {4, 1}} {
		if _, e := readFrame(bufio.NewReader(bytes.NewReader(b))); e == nil {
			t.Fatal("invalid framing accepted")
		}
	}
}
func configure(p *protocol) ([]byte, error) {
	b, _, e := p.handle(message(1, varint(1, 1023)))
	return b, e
}
func TestRemoteObservationNeverInventsPlaybackOrCommands(t *testing.T) {
	p := newProtocol(false)
	if p.observation.Power != nil {
		t.Fatal("invented initial off")
	}
	if _, _, e := p.handle(message(40, nil)); e == nil {
		t.Fatal("start before configure")
	}
	reply, e := configure(p)
	if e != nil {
		t.Fatal(e)
	}
	fs, _ := fields(reply)
	cfg, _ := payload(fs, 1)
	inner, _ := fields(cfg)
	negotiated, _ := number(inner, 1)
	if negotiated != featurePing|featurePower|featureVolume {
		t.Fatalf("negotiated mutation feature %d", negotiated)
	}
	if _, changed, e := p.handle(message(40, nil)); e != nil || !changed || p.observation.Power == nil || *p.observation.Power {
		t.Fatal("off not observed", e)
	}
	vol := append(varint(6, 20), varint(7, 7)...)
	if _, _, e := p.handle(message(50, vol)); e != nil {
		t.Fatal(e)
	}
	if p.observation.VolumeBasisPoints == nil || *p.observation.VolumeBasisPoints != 3500 {
		t.Fatal("volume conversion")
	}
	if _, _, e := p.handle(message(50, append(varint(6, 20), varint(7, 21)...))); e != nil {
		t.Fatal(e)
	}
	if p.observation.VolumeBasisPoints != nil {
		t.Fatal("out of bounds volume")
	}
	if _, _, e := p.handle(message(20, message(1, message(12, []byte("com.example.app"))))); e != nil {
		t.Fatal(e)
	}
	if p.observation.Source != nil {
		t.Fatal("IME not enabled")
	}
	reply, _, e = p.handle(message(8, varint(1, 731)))
	if e != nil {
		t.Fatal(e)
	}
	fs, _ = fields(reply)
	ping, _ := payload(fs, 9)
	inner, _ = fields(ping)
	v, _ := number(inner, 1)
	if v != 731 {
		t.Fatal("ping not echoed")
	}
	if _, e = configure(p); e == nil {
		t.Fatal("duplicate configuration accepted")
	}
	p = newProtocol(true)
	_, _ = configure(p)
	_, _, _ = p.handle(message(20, message(1, message(12, []byte("com.example.app")))))
	if p.observation.Source == nil || *p.observation.Source != "com.example.app" {
		t.Fatal("source missing")
	}
}

func TestMalformedStateCannotBecomeObservedOff(t *testing.T) {
	for _, raw := range [][]byte{message(40, message(1, []byte("wrong-wire-type"))), message(40, append(varint(1, 1), varint(1, 0)...)), message(40, varint(1, 2))} {
		p := newProtocol(false)
		_, _ = configure(p)
		if _, _, e := p.handle(raw); e == nil || p.observation.Power != nil {
			t.Fatal("malformed power became a real observation")
		}
	}
}
func TestCacheRetainsOffButExpiresAndCancels(t *testing.T) {
	r, e := NewRuntime()
	if e != nil {
		t.Fatal(e)
	}
	id, _ := domain.ParseEntityID("demo-tv-player")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &pilot{runtime: r, ctx: ctx}
	catalog, e := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if e != nil {
		t.Fatal(e)
	}
	description := r.description("Demo")
	if e = catalog.RegisterDriverCapability(id, description); e != nil {
		t.Fatal(e)
	}
	if description.GetExtension().ActionMessage != nil {
		t.Fatal("action codec advertised")
	}
	off := false
	p.cache(id, "Demo", Observation{Power: &off}, true)
	s, ok := r.Snapshot(id)
	if !ok || s.PoweredOn == nil || *s.PoweredOn || s.Playback != media.PlaybackState_PLAYBACK_STATE_UNSPECIFIED || s.Title != nil || s.PositionMillis != nil || s.DurationMillis != nil || s.SourceId != nil {
		t.Fatal("invented media state")
	}
	if len(s.Capabilities.Operations) > 0 || s.ActionAccess.String() != "ACTION_ACCESS_DENIED" || s.Quality.Availability.String() != "AVAILABILITY_AVAILABLE" {
		t.Fatal("off treated unavailable or actions advertised")
	}
	s.Title = proto.String("changed")
	again, _ := r.Snapshot(id)
	if again.Title != nil {
		t.Fatal("mutable snapshot")
	}
	r.mu.Lock()
	c := r.records[id]
	c.heartbeat = time.Now().Add(-idleTimeout - time.Second)
	r.records[id] = c
	r.mu.Unlock()
	if _, ok = r.Snapshot(id); ok {
		t.Fatal("expired observation fresh")
	}
	p.cache(id, "Demo", Observation{Power: &off}, true)
	cancel()
	if _, ok = r.Snapshot(id); ok {
		t.Fatal("retired observation available")
	}
}

type testHost struct{ value storage.SecretValue }

func (h *testHost) Discover(context.Context, []integrations.Candidate) ([]integrations.Binding, error) {
	return nil, ErrUnavailable
}
func (h *testHost) Publish(context.Context, integrations.Binding, integrations.StateObservation) error {
	return nil
}
func (h *testHost) Emit(context.Context, integrations.Binding, integrations.DeviceEvent) error {
	return nil
}
func (h *testHost) ReportHealth(context.Context, integrations.HealthReport) error { return nil }
func (h *testHost) Secrets() (integrations.SecretProvider, error)                 { return h, nil }
func (h *testHost) Read(context.Context, storage.SecretHandle) (storage.SecretValue, error) {
	return h.value, nil
}
func TestReconnectClearsOldStateAndCannotReplayKeys(t *testing.T) {
	r, _ := NewRuntime()
	settings, _ := ParseSettings(Input{Instance: "demo-tv", Players: []PlayerInput{testPlayer()}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &pilot{runtime: r, ctx: ctx, settings: settings}
	id, _ := domain.ParseEntityID("demo-tv-player")
	v, _ := testCredential(t, "atvremote/test/"+testMAC, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), nil)
	host := &testHost{v}
	firstReady := make(chan struct{})
	dropFirst := make(chan struct{})
	secondReady := make(chan struct{})
	startSecond := make(chan struct{})
	errs := make(chan error, 2)
	var dials atomic.Int32
	r.dial = func(_ context.Context, address string, config *tls.Config) (net.Conn, error) {
		if address != "192.168.50.20:6466" || config == nil || config.VerifyConnection == nil || len(config.Certificates) != 1 {
			return nil, ErrCredential
		}
		client, server := net.Pipe()
		attempt := dials.Add(1)
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			if e := writeFrame(server, message(1, varint(1, 1023))); e != nil {
				errs <- e
				return
			}
			reply, e := readFrame(reader)
			if e != nil {
				errs <- e
				return
			}
			fs, e := fields(reply)
			if e != nil || len(fs) != 1 || fs[0].number != 1 {
				errs <- ErrProtocol
				return
			}
			inner, _ := fields(fs[0].data)
			features, _ := number(inner, 1)
			if features&^(uint64(featurePing|featurePower|featureVolume)) != 0 {
				errs <- ErrProtocol
				return
			}
			if attempt == 1 {
				_ = writeFrame(server, message(40, varint(1, 1)))
				close(firstReady)
				<-dropFirst
				return
			}
			close(secondReady)
			<-startSecond
			_ = writeFrame(server, message(40, nil))
			for {
				reply, e = readFrame(reader)
				if e != nil {
					return
				}
				fs, e = fields(reply)
				if e != nil {
					errs <- e
					return
				}
				for _, f := range fs {
					if f.number == 10 || f.number == 90 {
						errs <- ErrProtocol
						return
					}
				}
			}
		}()
		return client, nil
	}
	done := make(chan error, 1)
	go func() { done <- p.run(ctx, host, testPlayer(), id) }()
	select {
	case <-firstReady:
	case <-time.After(2 * time.Second):
		t.Fatal("first connection missing")
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := r.Snapshot(id); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first observation missing")
		}
		time.Sleep(time.Millisecond)
	}
	close(dropFirst)
	select {
	case <-secondReady:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect missing")
	}
	if _, ok := r.Snapshot(id); ok {
		t.Fatal("old connection state replayed before remote_start")
	}
	close(startSecond)
	deadline = time.Now().Add(time.Second)
	for {
		if _, ok := r.Snapshot(id); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new observation missing")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("connection not cancelled")
	}
	select {
	case e := <-errs:
		t.Fatal(e)
	default:
	}
	if dials.Load() != 2 {
		t.Fatal("unexpected dial count")
	}
}
func TestCredentialDiagnosticsAreRedacted(t *testing.T) {
	c := credentialJSON{ClientKeyPEM: "canary-private-key"}
	if strings.Contains(c.String(), "canary") || strings.Contains(c.GoString(), "canary") {
		t.Fatal("credential diagnostic leaked")
	}
}
