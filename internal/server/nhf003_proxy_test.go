package server

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var nhf003LostResponse = errors.New("synthetic response loss after native refresh")

type nhf003Proxy struct {
	server                                            *httptest.Server
	transport                                         *http.Transport
	login, complete, refresh, revoke, lost, shortened atomic.Uint64
	observationMu                                     sync.Mutex
	lastLogin                                         *nhf003LoginObservation
	stateReads, stateReadOK, revokeOK                 atomic.Uint64
}

type nhf003LoginExpectation struct{ name, password string }
type nhf003LoginObservation struct {
	Decoded                 bool  `json:"decoded"`
	NameMatches             bool  `json:"name_matches"`
	PasswordMatches         bool  `json:"password_matches"`
	PasswordCaseFoldMatches bool  `json:"password_case_fold_matches"`
	PasswordMismatches      []int `json:"password_mismatch_positions"`
	NameLength              int   `json:"name_length"`
	PasswordLength          int   `json:"password_length"`
}

func (p *nhf003Proxy) loginObservation() *nhf003LoginObservation {
	p.observationMu.Lock()
	defer p.observationMu.Unlock()
	if p.lastLogin == nil {
		return nil
	}
	copy := *p.lastLogin
	return &copy
}

// The synthetic diagnostic retains no submitted values. This body owns its
// serialization lock so Close can wipe it without racing a transport Read.
type nhf003ObservedBody struct {
	mu     sync.Mutex
	reader *bytes.Reader
	data   []byte
}

func (b *nhf003ObservedBody) Read(out []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reader.Read(out)
}
func (b *nhf003ObservedBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.data)
	b.data = nil
	b.reader.Reset(nil)
	return nil
}

// Standard reverse proxy, not a Connect codec/framer. Native SQL/auth produces
// every decision. Two opt-in faults shorten only the advertised access lifetime
// (conservative client expiry), or close HTTP/1 TLS after native refresh commits
// but before the client receives its response. Server expiry stays unchanged.
func nhf003StartProxy(t *testing.T, upstream string, roots *x509.CertPool, pair tls.Certificate, control string, expected nhf003LoginExpectation) *nhf003Proxy {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	p := &nhf003Proxy{transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(request *http.Request) {
		director(request)
		request.Header.Set("Accept-Encoding", "identity")
		switch request.URL.Path {
		case statev1connect.StateServiceOpenViewProcedure:
			p.stateReads.Add(1)
		case authv1connect.AuthServiceStartLoginProcedure:
			p.login.Add(1)
		case authv1connect.AuthServiceCompleteLoginProcedure:
			p.complete.Add(1)
		case authv1connect.AuthServiceRefreshSessionProcedure:
			p.refresh.Add(1)
		case authv1connect.AuthServiceRevokeSessionProcedure:
			p.revoke.Add(1)
		}
	}
	proxy.Transport = p.transport
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode != http.StatusOK {
			return nil
		}
		path := response.Request.URL.Path
		if path == statev1connect.StateServiceOpenViewProcedure {
			p.stateReadOK.Add(1)
		}
		if path == authv1connect.AuthServiceRevokeSessionProcedure {
			p.revokeOK.Add(1)
		}
		if path == authv1connect.AuthServiceRefreshSessionProcedure {
			if _, err := os.Stat(filepath.Join(control, "lose-refresh")); err == nil && p.lost.CompareAndSwap(0, 1) {
				// Consume/erase only synthetic data. At this point the native
				// handler has already committed the refresh rotation.
				data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
				_ = response.Body.Close()
				clear(data)
				if err != nil {
					return err
				}
				return nhf003LostResponse
			}
		}
		if path != authv1connect.AuthServiceStartLoginProcedure && path != authv1connect.AuthServiceCompleteLoginProcedure {
			return nil
		}
		if _, err := os.Stat(filepath.Join(control, "short-access")); err != nil {
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		_ = response.Body.Close()
		if err != nil {
			return err
		}
		if len(data) > 65536 {
			clear(data)
			return errors.New("synthetic login response exceeds bound")
		}
		defer clear(data)
		var message proto.Message
		var decision *authv1.LoginDecision
		if path == authv1connect.AuthServiceStartLoginProcedure {
			value := &authv1.StartLoginResponse{}
			if err := proto.Unmarshal(data, value); err != nil {
				return err
			}
			message, decision = value, value.Decision
		} else {
			value := &authv1.CompleteLoginResponse{}
			if err := proto.Unmarshal(data, value); err != nil {
				return err
			}
			message, decision = value, value.Decision
		}
		if tokens := decision.GetAuthenticated(); tokens != nil {
			// The client expires conservatively with 30s clock uncertainty;
			// advertise three usable seconds before that safety boundary.
			tokens.AccessExpiresAt = timestamppb.New(time.Now().Add(33 * time.Second))
			p.shortened.Add(1)
		}
		encoded, err := proto.Marshal(message)
		if err != nil {
			return err
		}
		response.Body = io.NopCloser(bytes.NewReader(encoded))
		response.ContentLength = int64(len(encoded))
		response.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, nhf003LostResponse) {
			if hijacker, ok := w.(http.Hijacker); ok {
				connection, _, hijackErr := hijacker.Hijack()
				if hijackErr == nil {
					_ = connection.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "synthetic proxy unavailable", http.StatusBadGateway)
	}
	p.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == authv1connect.AuthServiceStartLoginProcedure && r.Body != nil {
			data, err := io.ReadAll(io.LimitReader(r.Body, 65537))
			_ = r.Body.Close()
			if err != nil || len(data) > 65536 {
				clear(data)
				http.Error(w, "synthetic login exceeds bound", http.StatusRequestEntityTooLarge)
				return
			}
			observation := &nhf003LoginObservation{}
			if r.Header.Get("Content-Encoding") == "" {
				message := &authv1.StartLoginRequest{}
				if proto.Unmarshal(data, message) == nil {
					observation.Decoded = true
					observation.NameMatches = message.LoginName == expected.name
					observation.PasswordMatches = message.Password == expected.password
					observation.PasswordCaseFoldMatches = strings.EqualFold(message.Password, expected.password)
					if len(message.Password) == len(expected.password) {
						for index := range len(message.Password) {
							if message.Password[index] != expected.password[index] {
								observation.PasswordMismatches = append(observation.PasswordMismatches, index)
							}
						}
					}
					observation.NameLength = len(message.LoginName)
					observation.PasswordLength = len(message.Password)
				}
				message.Reset()
			}
			p.observationMu.Lock()
			p.lastLogin = observation
			p.observationMu.Unlock()
			body := &nhf003ObservedBody{reader: bytes.NewReader(data), data: data}
			r.Body = body
			defer body.Close()
		}
		proxy.ServeHTTP(w, r)
	}))
	p.server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, NextProtos: []string{"http/1.1"}}
	p.server.StartTLS()
	return p
}

func (p *nhf003Proxy) close() { p.server.Close(); p.transport.CloseIdleConnections() }
