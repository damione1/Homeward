package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// All fault controls live in this opt-in test fixture, outside production.
type nhf032Action string

const (
	nhf032Drop          nhf032Action = "drop-refresh"
	nhf032Outage        nhf032Action = "outage"
	nhf032Restore       nhf032Action = "restore"
	nhf032BarePreflight nhf032Action = "bare-preflight"
	nhf032BareRenewal   nhf032Action = "bare-renewal"
	nhf032Revoke        nhf032Action = "revoke"
	nhf032Expire        nhf032Action = "expire"
)

type nhf032Command struct {
	Sequence uint64       `json:"sequence"`
	Action   nhf032Action `json:"action"`
}
type nhf032Fault uint8

const (
	nhf032Normal nhf032Fault = iota
	nhf032Unavailable
	nhf032Unauthenticated
	nhf032RenewalUnauthenticated
)

type nhf032Clock struct{ offset atomic.Int64 }

func (c *nhf032Clock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *nhf032Clock) At(deadline time.Time, fire func()) sessionTimer {
	return time.AfterFunc(deadline.Sub(c.Now()), fire)
}

type nhf032Observations struct {
	mu                         sync.Mutex
	fault                      nhf032Fault
	firstKey                   [32]byte
	keysEqual                  bool
	keyedCalls                 uint64
	actionRequests             atomic.Uint64
	loginExpiry, refreshExpiry time.Time
	owner                      storage.SessionOwner
}

// This fixture composes the real PostgreSQL/auth runtime and the existing
// NHF003 TLS response-loss proxy. A second standard TLS reverse proxy observes
// only bounded synthetic metadata and injects typed protocol errors.
func TestNHF032SessionRecoveryServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF032_FIXTURE")
	if out == "" {
		t.Skip("opt-in native session recovery qualification")
	}
	if !filepath.IsAbs(out) {
		t.Fatal("fresh absolute fixture directory required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "short-access"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	clock := &nhf032Clock{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var store *storage.Store
	var listen net.ListenConfig
	go func() {
		done <- RunWithStartupFactory(ctx, settings.startup, listen.Listen, func(addr net.Addr) { ready <- addr }, func(_ context.Context, actual *storage.Store) (RuntimePlan, error) {
			store = actual
			plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: actual})
			if err == nil {
				plan.sessionClock = clock
			}
			return RuntimePlan{Identity: unconfiguredIdentity{}, Public: plan}, err
		})
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Error(err)
				}
			case <-time.After(15 * time.Second):
				t.Error("isolated server did not join")
			}
		}
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		joined = true
		t.Fatal("isolated runtime exited", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	const login, password = "NHF032 synthetic account", "NHF032 isolated synthetic password"
	account := nha162RunBootstrap(t, ctx, data, login, password)
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	inner := nhf003StartProxy(t, "https://"+address.String(), roots, pair, out, nhf003LoginExpectation{name: login, password: password})
	defer inner.close()
	target, err := url.Parse(inner.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	observed := &nhf032Observations{keysEqual: true}
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.Transport = transport
	reverse.ErrorLog = log.New(io.Discard, "", 0)
	reverse.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode != http.StatusOK {
			return nil
		}
		path := response.Request.URL.Path
		if path != authv1connect.AuthServiceStartLoginProcedure && path != authv1connect.AuthServiceRefreshSessionProcedure {
			return nil
		}
		payload, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		_ = response.Body.Close()
		if err != nil || len(payload) > 65536 {
			clear(payload)
			return errors.New("bounded synthetic response unreadable")
		}
		var tokens *authv1.SessionTokens
		if path == authv1connect.AuthServiceStartLoginProcedure {
			message := &authv1.StartLoginResponse{}
			if err := proto.Unmarshal(payload, message); err != nil {
				clear(payload)
				return err
			}
			tokens = message.Decision.GetAuthenticated()
		} else {
			message := &authv1.RefreshSessionResponse{}
			if err := proto.Unmarshal(payload, message); err != nil {
				clear(payload)
				return err
			}
			tokens = message.Tokens
		}
		if tokens == nil || tokens.Session == nil {
			clear(payload)
			return errors.New("synthetic tokens absent")
		}
		session, err := contracts.ParseSessionID(tokens.Session.Id.GetValue())
		if err != nil {
			clear(payload)
			return err
		}
		ownerAccount, err := contracts.ParseAccountID(tokens.Session.AccountId.GetValue())
		if err != nil {
			clear(payload)
			return err
		}
		observed.mu.Lock()
		if path == authv1connect.AuthServiceStartLoginProcedure {
			observed.loginExpiry = tokens.Session.ExpiresAt.AsTime()
			observed.owner = storage.SessionOwner{Account: ownerAccount, Session: session}
		} else {
			observed.refreshExpiry = tokens.Session.ExpiresAt.AsTime()
		}
		observed.mu.Unlock()
		response.Body = &nhf003ObservedBody{reader: bytes.NewReader(payload), data: payload}
		return nil
	}
	outer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/"+apiv2connect.CapabilityServiceName+"/") || strings.HasPrefix(r.URL.Path, "/"+apiv2connect.DeviceServiceName+"/") {
			observed.actionRequests.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		observed.mu.Lock()
		fault := observed.fault
		observed.mu.Unlock()
		switch fault {
		case nhf032Normal:
		case nhf032Unavailable:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"unavailable","message":"synthetic upstream unavailable"}`)
			return
		case nhf032RenewalUnauthenticated:
			if r.URL.Path == authv1connect.AuthServiceRefreshSessionProcedure {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"code":"unauthenticated","message":"synthetic bare renewal denial"}`)
				return
			}
		case nhf032Unauthenticated:
			if r.URL.Path == authv1connect.AuthServiceListSessionsProcedure {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"code":"unauthenticated","message":"synthetic bare access denial"}`)
				return
			}
		default:
			t.Error("invalid fixture fault state")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == authv1connect.AuthServiceRefreshSessionProcedure {
			payload, err := io.ReadAll(io.LimitReader(r.Body, 65537))
			_ = r.Body.Close()
			if err != nil || len(payload) > 65536 {
				clear(payload)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			message := &authv1.RefreshSessionRequest{}
			if err := proto.Unmarshal(payload, message); err != nil || len(message.RenewalKey) != 32 {
				clear(payload)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			digest := sha256.Sum256(message.RenewalKey)
			observed.mu.Lock()
			if observed.keyedCalls == 0 {
				observed.firstKey = digest
			} else if observed.firstKey != digest {
				observed.keysEqual = false
			}
			observed.keyedCalls++
			observed.mu.Unlock()
			message.Reset()
			body := &nhf003ObservedBody{reader: bytes.NewReader(payload), data: payload}
			r.Body = body
			defer body.Close()
		}
		reverse.ServeHTTP(w, r)
	}))
	outer.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, NextProtos: []string{"http/1.1"}}
	outer.StartTLS()
	defer outer.Close()
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": outer.URL, "ca_pem": string(certificate), "login_name": login, "password": password, "account_id": account.Value, "namespace_nonce": hex.EncodeToString(nonce)})
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var last uint64
	for {
		select {
		case <-ctx.Done():
			t.Fatal("synthetic control expired")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				observed.mu.Lock()
				finalOwner := observed.owner
				observed.mu.Unlock()
				var generation, retained int
				var sevenDayInactivity bool
				if sessionID, err := finalOwner.Session.Value(); err == nil {
					if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
						return tx.QueryRow(ctx, `SELECT current_generation,(expires_at-last_used_at)=interval '7 days',(SELECT count(*) FROM public.auth_refresh_generations g WHERE g.session_id=s.id) FROM public.auth_sessions s WHERE id=$1`, sessionID).Scan(&generation, &sevenDayInactivity, &retained)
					}); err != nil {
						t.Error("final synthetic session facts", err)
					}
				}
				// Stop the outer/inner proxies before the runtime and count joined work.
				outer.Close()
				inner.close()
				cancel()
				select {
				case err := <-done:
					joined = true
					if err != nil {
						t.Error(err)
					}
				case <-time.After(15 * time.Second):
					t.Error("runtime failed to join")
				}
				observed.mu.Lock()
				report := map[string]any{"server_joined": joined, "start_login_calls": inner.login.Load(), "refresh_calls": inner.refresh.Load(), "lost_refresh_responses": inner.lost.Load(), "keyed_refresh_calls": observed.keyedCalls, "same_renewal_key": observed.keysEqual, "sliding_deadline_advanced": observed.refreshExpiry.After(observed.loginExpiry), "action_requests": observed.actionRequests.Load(), "current_generation": generation, "retained_generations": retained, "seven_day_inactivity": sevenDayInactivity}
				observed.mu.Unlock()
				nhf003WritePrivate(t, out, "result.json", report)
				return
			}
			file := filepath.Join(out, "command.json")
			payload, err := os.ReadFile(file)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var command nhf032Command
			decoder := json.NewDecoder(bytes.NewReader(payload))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&command); err != nil {
				t.Fatal(err)
			}
			if command.Sequence == last {
				continue
			}
			if command.Sequence != last+1 || command.Sequence > 16 {
				t.Fatal("control sequence invalid")
			}
			switch command.Action {
			case nhf032Drop:
				if err := os.WriteFile(filepath.Join(out, "lose-refresh"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case nhf032Outage:
				observed.mu.Lock()
				observed.fault = nhf032Unavailable
				observed.mu.Unlock()
			case nhf032BarePreflight:
				observed.mu.Lock()
				observed.fault = nhf032Unauthenticated
				observed.mu.Unlock()
			case nhf032BareRenewal:
				observed.mu.Lock()
				observed.fault = nhf032RenewalUnauthenticated
				observed.mu.Unlock()
			case nhf032Restore:
				observed.mu.Lock()
				observed.fault = nhf032Normal
				observed.mu.Unlock()
			case nhf032Expire:
				clock.offset.Store(int64(8 * 24 * time.Hour))
			case nhf032Revoke:
				observed.mu.Lock()
				owner := observed.owner
				observed.mu.Unlock()
				if err := store.RevokeSession(ctx, owner.Account, owner, clock.Now()); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("unknown synthetic control action")
			}
			last = command.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", last), command)
		}
	}
}
