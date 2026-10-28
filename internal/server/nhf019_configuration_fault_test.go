package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	apply "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	"github.com/damione1/Homeward/gen/home/configurationapply/v1/configurationapplyv1connect"
)

type nhf019Fault uint8

const (
	nhf019Normal nhf019Fault = iota
	nhf019LoseCommitted
	nhf019Unknown
	nhf019Reconciling
	nhf019Restart
	nhf019Held
)

func nhf019ReadFault(path string) (nhf019Fault, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nhf019Normal, nil
	}
	if err != nil {
		return nhf019Normal, err
	}
	switch strings.TrimSpace(string(raw)) {
	case "normal":
		return nhf019Normal, nil
	case "lose-committed-response":
		return nhf019LoseCommitted, nil
	case "commit-unknown":
		return nhf019Unknown, nil
	case "committed-reconciling":
		return nhf019Reconciling, nil
	case "restart-required":
		return nhf019Restart, nil
	case "held":
		return nhf019Held, nil
	default:
		return nhf019Normal, errors.New("unknown synthetic configuration fault")
	}
}

type nhf019Counts struct {
	Received  uint64 `json:"received"`
	Forwarded uint64 `json:"forwarded"`
	Injected  uint64 `json:"injected"`
	Lost      uint64 `json:"lost"`
	Canceled  uint64 `json:"canceled"`
	Active    uint64 `json:"active"`
}
type nhf019FaultProxy struct {
	url    string
	mu     sync.Mutex
	counts nhf019Counts
	out    string
	t      *testing.T
}

func nhf019Replace(t *testing.T, directory, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(directory, name+".pending")
	if err := os.WriteFile(pending, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pending, filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
}
func (p *nhf019FaultProxy) update(change func(*nhf019Counts)) nhf019Counts {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(&p.counts)
	nhf019Replace(p.t, p.out, "counts.json", p.counts)
	return p.counts
}
func (p *nhf019FaultProxy) snapshot() nhf019Counts { p.mu.Lock(); defer p.mu.Unlock(); return p.counts }

type nhf019FaultKey struct{}

func nhf019Mutation(path string) bool {
	switch path {
	case configurationv1connect.ConfigurationServiceWriteDocumentProcedure, configurationv1connect.ConfigurationServiceRestoreDocumentProcedure, configurationapplyv1connect.ConfigurationApplyServiceApplyProcedure, configurationapplyv1connect.ConfigurationApplyServiceRollbackProcedure:
		return true
	default:
		return false
	}
}

// The proxy forwards production code except for explicitly injected outcomes.
// A lost response is cut only AFTER the real backend completed its mutation.
// Injected outcomes authenticate by a real ReadDocument and never forward Apply.
func nhf019StartFaultProxy(t *testing.T, out, data, origin string, roots *x509.CertPool, documents configurationv1connect.ConfigurationServiceClient) *nhf019FaultProxy {
	t.Helper()
	target, err := url.Parse(origin)
	if err != nil || target.Scheme != "https" || target.Hostname() != "127.0.0.1" {
		t.Fatal("isolated upstream required", err)
	}
	p := &nhf019FaultProxy{out: out, t: t}
	p.update(func(*nhf019Counts) {})
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	proxy.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "synthetic configuration upstream unavailable", http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		mode, _ := response.Request.Context().Value(nhf019FaultKey{}).(nhf019Fault)
		if mode == nhf019LoseCommitted && nhf019Mutation(response.Request.URL.Path) && response.StatusCode == http.StatusOK {
			_, err := io.Copy(io.Discard, io.LimitReader(response.Body, 2<<20))
			_ = response.Body.Close()
			if err != nil {
				return err
			}
			p.update(func(counts *nhf019Counts) { counts.Lost++ })
			panic(http.ErrAbortHandler)
		}
		return nil
	}
	injected := connect.NewUnaryHandler(configurationapplyv1connect.ConfigurationApplyServiceApplyProcedure, func(ctx context.Context, request *connect.Request[apply.ApplyRequest]) (*connect.Response[apply.ApplyResponse], error) {
		if contracts.AdmitConfigurationApplyRequest(request.Msg) != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("synthetic invalid candidate"))
		}
		read := connect.NewRequest(&cfg.ReadDocumentRequest{Id: &cfg.DocumentId{Value: "operational.main"}})
		read.Header().Set("Authorization", request.Header().Get("Authorization"))
		current, err := documents.ReadDocument(ctx, read)
		if err != nil {
			return nil, err
		}
		mode, ok := ctx.Value(nhf019FaultKey{}).(nhf019Fault)
		if !ok {
			return nil, connect.NewError(connect.CodeInternal, errors.New("unbound synthetic fault"))
		}
		target := current.Msg.ActiveGeneration.Revision + 1
		activation := &cfg.ActivationResult{}
		switch mode {
		case nhf019Unknown:
			activation.Outcome = cfg.ActivationOutcome_ACTIVATION_OUTCOME_COMMIT_UNKNOWN
			activation.Transaction = cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN
			activation.GenerationRevision = &target
		case nhf019Reconciling:
			activation.Outcome = cfg.ActivationOutcome_ACTIVATION_OUTCOME_COMMITTED_RECONCILING
			activation.Transaction = cfg.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED
			activation.GenerationRevision = &target
		case nhf019Restart:
			activation.Outcome = cfg.ActivationOutcome_ACTIVATION_OUTCOME_RESTART_REQUIRED
			activation.Transaction = cfg.TransactionOutcome_TRANSACTION_OUTCOME_NO_TRANSACTION
		case nhf019Normal, nhf019LoseCommitted, nhf019Held:
			return nil, connect.NewError(connect.CodeInternal, errors.New("wrong synthetic fault handler"))
		}
		response := &apply.ApplyResponse{Activation: activation}
		if contracts.AdmitConfigurationApplyResponse(response) != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("invalid synthetic outcome"))
		}
		p.update(func(counts *nhf019Counts) { counts.Injected++ })
		return connect.NewResponse(response), nil
	})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !nhf019Mutation(r.URL.Path) {
			proxy.ServeHTTP(w, r)
			return
		}
		mode, err := nhf019ReadFault(filepath.Join(out, "fault-mode"))
		if err != nil {
			http.Error(w, "invalid synthetic fault", http.StatusInternalServerError)
			return
		}
		counts := p.update(func(counts *nhf019Counts) { counts.Received++; counts.Active++ })
		defer p.update(func(counts *nhf019Counts) { counts.Active-- })
		r = r.WithContext(context.WithValue(r.Context(), nhf019FaultKey{}, mode))
		switch mode {
		case nhf019Normal, nhf019LoseCommitted:
			p.update(func(counts *nhf019Counts) { counts.Forwarded++ })
			proxy.ServeHTTP(w, r)
		case nhf019Unknown, nhf019Reconciling, nhf019Restart:
			if r.URL.Path != configurationapplyv1connect.ConfigurationApplyServiceApplyProcedure {
				http.Error(w, "injected outcome is Apply-only", http.StatusBadRequest)
				return
			}
			injected.ServeHTTP(w, r)
		case nhf019Held:
			// Consume the request so HTTP/1 can observe peer cancellation while
			// this handler deliberately waits without forwarding the mutation.
			if _, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 2<<20)); err != nil {
				http.Error(w, "invalid synthetic held body", http.StatusBadRequest)
				return
			}
			_ = r.Body.Close()
			nhf019Replace(t, out, "held.json", map[string]uint64{"sequence": counts.Received})
			select {
			case <-r.Context().Done():
				p.update(func(counts *nhf019Counts) { counts.Canceled++ })
			case <-time.After(8 * time.Second):
				http.Error(w, "synthetic held mutation deadline", http.StatusServiceUnavailable)
			}
		}
	}))
	identity, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{identity}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	if !strings.HasPrefix(server.URL, "https://127.0.0.1:") {
		t.Fatal("synthetic proxy is not loopback TLS")
	}
	p.url = server.URL
	return p
}
