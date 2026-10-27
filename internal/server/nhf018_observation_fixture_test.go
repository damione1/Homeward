package server

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
)

type nhf018ReadFault uint8

const (
	nhf018ReadNormal nhf018ReadFault = iota
	nhf018ReadUnavailable
	nhf018ReadDelayed
	nhf018LoginUnavailable
	nhf018LoginLimited
)

func nhf018ReadFaultFromFile(path string) (nhf018ReadFault, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nhf018ReadNormal, nil
	}
	if err != nil {
		return 0, err
	}
	switch string(raw) {
	case "normal":
		return nhf018ReadNormal, nil
	case "unavailable":
		return nhf018ReadUnavailable, nil
	case "delayed":
		return nhf018ReadDelayed, nil
	case "login-unavailable":
		return nhf018LoginUnavailable, nil
	case "login-limited":
		return nhf018LoginLimited, nil
	default:
		return 0, errors.New("invalid synthetic read fault")
	}
}

func nhf018SeedLogs(t *testing.T, f *nha148TLSFixture, count int) {
	t.Helper()
	event, err := observability.NewEvent(obs.LogCode_LOG_CODE_PROCESS_READY, obs.LogLevel_LOG_LEVEL_INFO, obs.LogComponent_LOG_COMPONENT_PROCESS, time.Now().UTC(), observability.NewProcessAuthor())
	if err != nil {
		t.Fatal(err)
	}
	for range count {
		if err := f.runtime.journal.Emit(event); err != nil {
			t.Fatal(err)
		}
	}
}

// The default server stays unchanged. Only the extra opt-in loopback proxy
// introduces a read fault; every other request forwards to the real TLS Run.
func nhf018ObservationFixture(t *testing.T, f *nha148TLSFixture, out string) string {
	t.Helper()
	for range 110 {
		_, err := f.accounts.GetAccount(f.ctx, nha056SessionRequest(&auth.GetAccountRequest{AccountId: f.tokens.Session.AccountId}, f.token))
		if err != nil {
			t.Fatal("synthetic authentication audit", err)
		}
	}
	repository, err := f.runtime.store.BootstrapSecrets(f.ctx, f.data)
	if err != nil {
		t.Fatal(err)
	}
	requester, err := domain.ParseIntegrationInstanceID("nhf018-audit-reader")
	if err != nil {
		t.Fatal(err)
	}
	for range 110 {
		if err := repository.AuditDenied(f.ctx, requester, nha051ServerHandle(t, "nha148-mqtt", "password-ref"), storage.SecretDenialForeignHandle); err != nil {
			t.Fatal("synthetic denied-secret audit", err)
		}
	}
	nhf018SeedLogs(t, f, 230)
	target, err := url.Parse("https://" + f.run.address.String())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13}}
	proxy.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "synthetic upstream unavailable", http.StatusBadGateway)
	}
	identity, err := tls.LoadX509KeyPair(filepath.Join(f.data, nha181CertificateName), filepath.Join(f.data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == authv1connect.AuthServiceStartLoginProcedure {
			mode, err := nhf018ReadFaultFromFile(filepath.Join(out, "read-fault"))
			if err != nil {
				http.Error(w, "invalid synthetic fault", http.StatusInternalServerError)
				return
			}
			switch mode {
			case nhf018ReadNormal, nhf018ReadUnavailable, nhf018ReadDelayed:
			case nhf018LoginUnavailable:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":"unavailable","message":"synthetic login unavailable"}`))
				return
			case nhf018LoginLimited:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"code":"resource_exhausted","message":"synthetic login limited"}`))
				return
			}
		}
		if r.URL.Path == observabilityv1connect.ObservabilityServiceQueryRuntimeLogsProcedure {
			mode, err := nhf018ReadFaultFromFile(filepath.Join(out, "read-fault"))
			if err != nil {
				http.Error(w, "invalid synthetic fault", http.StatusInternalServerError)
				return
			}
			switch mode {
			case nhf018ReadNormal, nhf018LoginUnavailable, nhf018LoginLimited:
			case nhf018ReadDelayed:
				_ = os.WriteFile(filepath.Join(out, "read-entered"), nil, 0600)
				defer func() { _ = os.WriteFile(filepath.Join(out, "read-finished"), nil, 0600) }()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-deadline.C:
						http.Error(w, "synthetic delayed read timed out", http.StatusServiceUnavailable)
						return
					case <-ticker.C:
						if _, err := os.Stat(filepath.Join(out, "read-release")); err == nil {
							proxy.ServeHTTP(w, r)
							return
						}
					}
				}
			case nhf018ReadUnavailable:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":"unavailable","message":"synthetic observation unavailable"}`))
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{identity}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	if !strings.HasPrefix(server.URL, "https://127.0.0.1:") {
		t.Fatal("synthetic proxy must bind loopback TLS")
	}
	return server.URL
}
