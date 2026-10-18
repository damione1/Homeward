package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

const nha181CertificateName = "public.crt"
const nha181PrivateKeyName = "public.key"

func nha181CertificateFixture(t *testing.T, dataDir string) *x509.CertPool {
	t.Helper()
	return nha181CertificateFixtureFor(t, dataDir, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
}

func nha181CertificateFixtureFor(t *testing.T, dataDir string, notBefore, notAfter time.Time) *x509.CertPool {
	t.Helper()
	return nha181CertificateFixtureWith(t, dataDir, notBefore, notAfter, nil)
}

func nha181CertificateFixtureWith(t *testing.T, dataDir string, notBefore, notAfter time.Time, configure func(*x509.Certificate)) *x509.CertPool {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if configure != nil {
		configure(template)
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
	defer clear(privateKeyPEM)
	if err := os.WriteFile(filepath.Join(dataDir, nha181CertificateName), certificatePEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, nha181PrivateKeyName), privateKeyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("synthetic certificate could not be trusted")
	}
	return roots
}

func nha181Client(roots *x509.CertPool) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}, ForceAttemptHTTP2: true,
	}, Timeout: 3 * time.Second}
}

func nha181Config(t *testing.T, postgresPort int, dataDir, listen, public string) config.Config {
	t.Helper()
	yaml := fmt.Sprintf("listen: %q\ndata_dir: %q\n%s"+
		"postgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n",
		listen, dataDir, public, postgresPort)
	value, err := config.ParseWithEnv([]byte(yaml), func(name string) (string, bool) {
		return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func nha181TLSYAML() string {
	return "public:\n  transport: tls\n  tls:\n    certificate_file: " + nha181CertificateName + "\n    private_key_file: " + nha181PrivateKeyName + "\n"
}

func TestNHA181RejectsUnsafeTransportBeforeDatabaseOrListener(t *testing.T) {
	dataDir := shortPrivateDataDir(t)
	var calls atomic.Int32
	listen := func(context.Context, string, string) (net.Listener, error) {
		calls.Add(1)
		return nil, errors.New("unexpected listener")
	}
	for _, fixture := range []struct {
		name, bind, public string
		want               error
	}{
		{"implicit wildcard h2c", "0.0.0.0:0", "", ErrPublicH2CNonLoopback},
		{"explicit wildcard h2c", "0.0.0.0:0", "public:\n  transport: h2c\n", ErrPublicH2CNonLoopback},
		{"missing TLS files", "127.0.0.1:0", nha181TLSYAML(), ErrPublicTLSUnsafe},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			settings := nha181Config(t, 1, dataDir, fixture.bind, fixture.public)
			err := Run(context.Background(), settings, listen, nil)
			if !errors.Is(err, fixture.want) || calls.Load() != 0 {
				t.Fatalf("pre-resource rejection: err=%v listeners=%d", err, calls.Load())
			}
		})
	}
}

func TestNHA181TLSFileAdmission(t *testing.T) {
	dataDir := shortPrivateDataDir(t)
	nha181CertificateFixture(t, dataDir)
	settings := nha181Config(t, 1, dataDir, "127.0.0.1:0", nha181TLSYAML())
	public, err := settings.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	if prepared, err := preparePublicServingTransport(settings, public); err != nil || prepared.mode != config.PublicTransportTLS || prepared.tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("valid TLS files not admitted: %v", err)
	}
	certPath := filepath.Join(dataDir, nha181CertificateName)
	keyPath := filepath.Join(dataDir, nha181PrivateKeyName)
	certificate, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	for _, fixture := range []struct {
		name string
		path string
		set  func(t *testing.T)
	}{
		{"key world-readable", keyPath, func(t *testing.T) {
			t.Helper()
			if err := os.Chmod(keyPath, 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"key unreadable", keyPath, func(t *testing.T) {
			t.Helper()
			if err := os.Chmod(keyPath, 0000); err != nil {
				t.Fatal(err)
			}
		}},
		{"certificate writable by group", certPath, func(t *testing.T) {
			t.Helper()
			if err := os.Chmod(certPath, 0620); err != nil {
				t.Fatal(err)
			}
		}},
		{"certificate unreadable", certPath, func(t *testing.T) {
			t.Helper()
			if err := os.Chmod(certPath, 0200); err != nil {
				t.Fatal(err)
			}
		}},
		{"certificate symlink", certPath, func(t *testing.T) {
			t.Helper()
			if err := os.Remove(certPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(keyPath, certPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"key symlink", keyPath, func(t *testing.T) {
			t.Helper()
			if err := os.Remove(keyPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(certPath, keyPath); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized certificate", certPath, func(t *testing.T) {
			t.Helper()
			if err := os.WriteFile(certPath, make([]byte, publicCertificateMaxBytes+1), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized key", keyPath, func(t *testing.T) {
			t.Helper()
			if err := os.WriteFile(keyPath, make([]byte, publicPrivateKeyMaxBytes+1), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"invalid pair", keyPath, func(t *testing.T) {
			t.Helper()
			if err := os.WriteFile(keyPath, []byte("not a private key"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := os.Remove(certPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(keyPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(certPath, certificate, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, key, 0600); err != nil {
				t.Fatal(err)
			}
			fixture.set(t)
			if _, err := preparePublicServingTransport(settings, public); !errors.Is(err, ErrPublicTLSUnsafe) {
				t.Fatalf("unsafe %s admitted: %v", fixture.path, err)
			}
		})
	}
	nha181CertificateFixtureFor(t, dataDir, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	if _, err := preparePublicServingTransport(settings, public); !errors.Is(err, ErrPublicTLSUnsafe) {
		t.Fatalf("expired certificate admitted: %v", err)
	}
}

func TestNHA181TLSLeafMatchesClientEKURules(t *testing.T) {
	dataDir := shortPrivateDataDir(t)
	settings := nha181Config(t, 1, dataDir, "127.0.0.1:0", nha181TLSYAML())
	public, err := settings.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	unknownEKU := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55555, 181, 1}
	unknownExtension := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55555, 181, 2}
	for _, fixture := range []struct {
		name     string
		change   func(*x509.Certificate)
		admitted bool
	}{
		{"unknown EKU only", func(c *x509.Certificate) {
			c.ExtKeyUsage = nil
			c.UnknownExtKeyUsage = []asn1.ObjectIdentifier{unknownEKU}
		}, false},
		{"unknown critical extension", func(c *x509.Certificate) {
			c.ExtraExtensions = []pkix.Extension{{Id: unknownExtension, Critical: true, Value: []byte{0x05, 0x00}}}
		}, false},
		{"server auth with unknown EKU", func(c *x509.Certificate) {
			c.UnknownExtKeyUsage = []asn1.ObjectIdentifier{unknownEKU}
		}, true},
		{"unknown noncritical extension", func(c *x509.Certificate) {
			c.ExtraExtensions = []pkix.Extension{{Id: unknownExtension, Value: []byte{0x05, 0x00}}}
		}, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			roots := nha181CertificateFixtureWith(t, dataDir, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), fixture.change)
			certificatePEM, err := os.ReadFile(filepath.Join(dataDir, nha181CertificateName))
			if err != nil {
				t.Fatal(err)
			}
			block, _ := pem.Decode(certificatePEM)
			if block == nil {
				t.Fatal("fixture PEM not decoded")
			}
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			_, clientErr := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1"})
			if (clientErr == nil) != fixture.admitted {
				t.Fatalf("client verification did not match fixture: %v", clientErr)
			}
			_, err = preparePublicServingTransport(settings, public)
			if fixture.admitted && err != nil || !fixture.admitted && !errors.Is(err, ErrPublicTLSUnsafe) {
				t.Fatalf("startup admission disagrees with client verifier: %v", err)
			}
		})
	}
}

// A Body that fails the test if read demonstrates that the common gate rejects
// an unencrypted credential route before the generated Connect decoder runs.
type nha181UnreadBody struct{ t *testing.T }

func (b nha181UnreadBody) Read([]byte) (int, error) {
	b.t.Fatal("credential body read before TLS gate")
	return 0, io.EOF
}
func (nha181UnreadBody) Close() error { return nil }

type nha181DeadlineRecorder struct{ *httptest.ResponseRecorder }

func (nha181DeadlineRecorder) SetReadDeadline(time.Time) error  { return nil }
func (nha181DeadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

func TestNHA181AccountGateRejectsPlaintextBeforeBody(t *testing.T) {
	settings := nha181Config(t, 1, shortPrivateDataDir(t), "127.0.0.1:0", "")
	public, err := settings.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPublicPlan(nil, nil, &nha139Auditor{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registerAccountRoutes(plan, nil); err != nil {
		t.Fatal(err)
	}
	compiled, err := compilePublicPlan(plan, public)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		authv1connect.AccountServiceAcceptInvitationProcedure,
		authv1connect.AccountServiceCreateAccountProcedure,
	} {
		r := httptest.NewRequest(http.MethodPost, path, nha181UnreadBody{t})
		r.Header.Set("Forwarded", "proto=https")
		r.Header.Set("X-Forwarded-Proto", "https")
		w := nha181DeadlineRecorder{httptest.NewRecorder()}
		compiled.handler(public, newPublicAdmissions()).ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "permission_denied") {
			t.Fatalf("plaintext %s: status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
}

func TestNHA181RunH2CRefusesInvitationWithoutBody(t *testing.T) {
	cluster := testpg.Start(t)
	settings := nha181Config(t, cluster.Port, shortPrivateDataDir(t), "127.0.0.1:0", "public:\n  transport: h2c\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listen net.ListenConfig
	go func() { done <- Run(ctx, settings, listen.Listen, func(addr net.Addr) { ready <- addr }) }()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("h2c Run before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("h2c Run did not become ready")
	}
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("h2c Run drain: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("h2c Run did not drain")
		}
	}()
	client := authv1connect.NewAccountServiceClient(&http.Client{Timeout: 3 * time.Second}, "http://"+address.String())
	request := connect.NewRequest(&authv1.AcceptInvitationRequest{})
	request.Header().Set("Forwarded", "proto=https")
	request.Header().Set("X-Forwarded-Proto", "https")
	if _, err := client.AcceptInvitation(ctx, request); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("plaintext invitation admitted or wrong gate: %v", err)
	}
}

func TestNHA181RunTLSConnectStreamAndDrain(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha181Config(t, cluster.Port, dataDir, "127.0.0.1:0", nha181TLSYAML())
	authenticator := &nha139Authenticator{}
	authorizer := &nha139Authorizer{}
	auditor := &nha139Auditor{}
	streamEnded := make(chan struct{}, 2)
	state := nha139StateFixture{streamEnded: streamEnded, streamFail: new(atomic.Bool), delay: new(atomic.Bool), openCalls: new(atomic.Int32)}
	plan, err := NewPublicPlan(authenticator, authorizer, auditor)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return statev1connect.NewStateServiceHandler(state, options...)
	},
		ConnectProcedure{Path: statev1connect.StateServiceOpenViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
		ConnectProcedure{Path: statev1connect.StateServiceWatchViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowServerStream, Transport: RouteTransportTLSRequired},
	); err != nil {
		t.Fatal(err)
	}
	if err := plan.Register(RouteKindMCP, RouteMethodGet, "/mcp/tls", RouteAccessAnonymous, RouteFlowUnary, RouteTransportTLSRequired, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || r.TLS.NegotiatedProtocol != "h2" || r.ProtoMajor != 2 {
			http.Error(w, "wrong TLS protocol", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})); err != nil {
		t.Fatal(err)
	}
	if err := plan.Register(RouteKindMedia, RouteMethodGet, "/media/http1", RouteAccessAnonymous, RouteFlowUnary, RouteTransportTLSRequired, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || r.ProtoMajor != 1 {
			http.Error(w, "wrong HTTP/1.1 protocol", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listen net.ListenConfig
	go func() {
		done <- RunWithFactory(ctx, settings, listen.Listen, func(addr net.Addr) { ready <- addr }, func(context.Context, *storage.Store) (RuntimePlan, error) {
			return RuntimePlan{Identity: unconfiguredIdentity{}, Public: plan}, nil
		})
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("Run before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Run did not become ready")
	}
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run drain: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Error("Run did not drain")
		}
	}()
	url := "https://" + address.String()
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	response, err := client.Get(url + "/mcp/tls")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.ProtoMajor != 2 {
		t.Fatalf("same listener did not serve h2: %d %s", response.StatusCode, response.Proto)
	}
	http1 := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}, Timeout: 3 * time.Second}
	defer http1.CloseIdleConnections()
	response, err = http1.Get(url + "/media/http1")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.ProtoMajor != 1 {
		t.Fatalf("same listener did not serve HTTP/1.1: %d %s", response.StatusCode, response.Proto)
	}
	for _, grpc := range []bool{false, true} {
		options := []connect.ClientOption{}
		if grpc {
			options = append(options, connect.WithGRPC())
		}
		api := statev1connect.NewStateServiceClient(client, url, options...)
		unary := connect.NewRequest(&statev1.OpenViewRequest{})
		unary.Header().Set("Authorization", "Bearer fixture")
		if _, err := api.OpenView(ctx, unary); err != nil {
			t.Fatalf("unary grpc=%t: %v", grpc, err)
		}
		streamCtx, streamCancel := context.WithCancel(ctx)
		request := connect.NewRequest(&statev1.WatchViewRequest{})
		request.Header().Set("Authorization", "Bearer fixture")
		stream, err := api.WatchView(streamCtx, request)
		if err != nil {
			streamCancel()
			t.Fatalf("stream grpc=%t: %v", grpc, err)
		}
		if !stream.Receive() || stream.Msg().GetStreamSequence() != 1 {
			streamCancel()
			t.Fatalf("stream grpc=%t: %v", grpc, stream.Err())
		}
		streamCancel()
		select {
		case <-streamEnded:
		case <-time.After(3 * time.Second):
			t.Fatal("stream handler was not canceled")
		}
	}
	wrongTrust := nha181Client(x509.NewCertPool())
	defer wrongTrust.CloseIdleConnections()
	if response, err := wrongTrust.Get(url + "/mcp/tls"); err == nil {
		response.Body.Close()
		t.Fatal("untrusted certificate accepted")
	}
	wrongName := nha181Client(roots)
	wrongName.Transport.(*http.Transport).TLSClientConfig.ServerName = "wrong.invalid"
	defer wrongName.CloseIdleConnections()
	if response, err := wrongName.Get(url + "/mcp/tls"); err == nil {
		response.Body.Close()
		t.Fatal("wrong certificate name accepted")
	}
	plain := &http.Client{Timeout: time.Second}
	if response, err := plain.Get("http://" + address.String() + "/mcp/tls"); err == nil {
		response.Body.Close()
		if response.StatusCode == http.StatusNoContent {
			t.Fatal("TLS listener accepted plaintext")
		}
	}
}
