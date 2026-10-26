package server

// Explicit qualification only: a fresh local PostgreSQL cluster, the real
// ListService/auth/policy composition and loopback TLS. No device Host exists.
import (
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	lists "github.com/damione1/Homeward/gen/home/lists/v1"
	listconnect "github.com/damione1/Homeward/gen/home/lists/v1/listsv1connect"
	"github.com/jackc/pgx/v5"
)

type nhf011ControlAction string

const (
	nhf011Outage   nhf011ControlAction = "outage"
	nhf011Restore  nhf011ControlAction = "restore-network"
	nhf011Revoke   nhf011ControlAction = "revoke-reader"
	nhf011Allow    nhf011ControlAction = "allow-reader"
	nhf011Snapshot nhf011ControlAction = "snapshot"
)

func (a *nhf011ControlAction) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch nhf011ControlAction(value) {
	case nhf011Outage, nhf011Restore, nhf011Revoke, nhf011Allow, nhf011Snapshot:
		*a = nhf011ControlAction(value)
		return nil
	default:
		return errors.New("unknown synthetic list control")
	}
}

type nhf011Control struct {
	Sequence uint64              `json:"sequence"`
	Action   nhf011ControlAction `json:"action"`
}

func nhf011ReadControl(root string, after uint64) (*nhf011Control, error) {
	file, err := os.Open(filepath.Join(root, "command.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, errors.New("oversized or non-regular list control")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4096))
	decoder.DisallowUnknownFields()
	var command nhf011Control
	if err = decoder.Decode(&command); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing list control")
	}
	if command.Sequence == 0 || command.Action == "" {
		return nil, errors.New("incomplete list control")
	}
	if command.Sequence <= after {
		return nil, nil
	}
	if command.Sequence != after+1 {
		return nil, errors.New("out-of-order list control")
	}
	return &command, nil
}

func TestNHF011ListsFixtureControls(t *testing.T) {
	root := t.TempDir()
	if command, err := nhf011ReadControl(root, 0); err != nil || command != nil {
		t.Fatal("absent command", command, err)
	}
	for _, value := range []string{
		`{}`, `{"sequence":0,"action":"snapshot"}`, `{"sequence":2,"action":"snapshot"}`,
		`{"sequence":1,"action":"unknown"}`, `{"sequence":1,"action":"snapshot","extra":true}`,
		`{"sequence":1,"action":"snapshot"} {}`, strings.Repeat(" ", 4097) + `{}`,
	} {
		if err := os.WriteFile(filepath.Join(root, "command.json"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := nhf011ReadControl(root, 0); err == nil {
			t.Fatal("invalid fixture control admitted")
		}
	}
	for _, action := range []nhf011ControlAction{nhf011Outage, nhf011Restore, nhf011Revoke, nhf011Allow, nhf011Snapshot} {
		value, err := json.Marshal(nhf011Control{Sequence: 1, Action: action})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "command.json"), value, 0600); err != nil {
			t.Fatal(err)
		}
		command, err := nhf011ReadControl(root, 0)
		if err != nil || command == nil || command.Action != action || command.Sequence != 1 {
			t.Fatal("valid fixture control refused", err)
		}
		if command, err = nhf011ReadControl(root, 1); err != nil || command != nil {
			t.Fatal("control was replayed", err)
		}
	}
}

func TestNHF011ListsServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF011_FIXTURE")
	if out == "" {
		t.Skip("explicit isolated lists fixture only")
	}
	if os.Getenv("HOMEWARD_NHF011_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("fresh absolute synthetic fixture directory required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("fixture directory must be fresh", err)
	}
	r := nha162NewRPCRigWithLifetime(t, 8*time.Minute)
	defer r.server.CloseClientConnections()
	seed, err := r.client.CreateList(r.ctx, nha162Bearer(&lists.CreateListRequest{Name: "Liste synthétique longue"}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	seedID := seed.Msg.Receipt.ListId.Value
	// Bulk setup is deliberately distinct from the RPC mutation qualification.
	// Only this newly allocated fixture database is written directly.
	err = r.store.InTransaction(r.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.ctx, `INSERT INTO public.homeward_list_items(list_id,item_uid,origin,summary,completed,position,lifecycle,revision)
		 SELECT l.list_id,'synthetic-'||lpad(n::text,5,'0'),l.origin,'Tâche synthétique '||lpad(n::text,5,'0'),false,n-1,1,1
		 FROM public.homeward_lists l CROSS JOIN generate_series(1,10000) n WHERE l.list_id=$1`, seedID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(r.ctx, `UPDATE public.homeward_lists SET revision=10001 WHERE list_id=$1`, seedID)
		return err
	})
	if err != nil {
		t.Fatal("private bulk setup", err)
	}
	const readerPassword = "NHF011 synthetic reader password canary"
	reader, err := r.accounts.CreateAccount(r.ctx, nha162Bearer(&auth.CreateAccountRequest{LoginName: "NHF011 Reader", DisplayName: "Synthetic reader", InitialPassword: readerPassword, Role: auth.AccountRole_ACCOUNT_ROLE_USER}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	readerID, err := contracts.ParseAccountID(reader.Msg.Account.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
	readPolicy := fmt.Sprintf("  - scope: {family: lists, operation: read}\n    objects: {lists: {explicit: [%q]}}\n", seedID)
	nha162SetPolicy(t, r, readerID, readPolicy)
	upstream, err := url.Parse(r.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(upstream)
	forward.Transport = r.server.Client().Transport
	errorWriter := connect.NewErrorWriter()
	unavailable := func(w http.ResponseWriter, request *http.Request) {
		_ = errorWriter.Write(w, request, connect.NewError(connect.CodeUnavailable, errors.New("synthetic link unavailable")))
	}
	forward.ErrorHandler = func(w http.ResponseWriter, request *http.Request, _ error) { unavailable(w, request) }
	var offline atomic.Bool
	var writes, streams, actionCalls atomic.Uint64
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/home.actions.v1.") {
			actionCalls.Add(1)
		}
		switch request.URL.Path {
		case listconnect.ListServiceCreateListProcedure, listconnect.ListServiceRenameListProcedure, listconnect.ListServiceDeleteListProcedure,
			listconnect.ListServiceAddItemProcedure, listconnect.ListServiceEditItemProcedure, listconnect.ListServiceDeleteItemProcedure,
			listconnect.ListServiceSetCompletedProcedure, listconnect.ListServiceMoveItemProcedure:
			writes.Add(1)
		case listconnect.ListServiceWatchListsProcedure:
			streams.Add(1)
			defer streams.Add(^uint64(0))
		}
		if offline.Load() {
			unavailable(w, request)
			return
		}
		forward.ServeHTTP(w, request)
	}))
	proxy.EnableHTTP2 = true
	certificateDir := shortPrivateDataDir(t)
	nha181CertificateFixture(t, certificateDir)
	pair, err := tls.LoadX509KeyPair(filepath.Join(certificateDir, nha181CertificateName), filepath.Join(certificateDir, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	proxy.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	proxy.StartTLS()
	defer proxy.Close()
	defer proxy.CloseClientConnections()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw})
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": proxy.URL, "ca_pem": string(certificate), "seed_list_id": seedID, "seed_item_count": 10000,
		"admin": map[string]string{"login_name": "NHA162 Admin", "password": "NHA162 synthetic admin password canary"}, "reader": map[string]string{"login_name": "NHF011 Reader", "password": readerPassword}})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-stop:
			return
		case <-r.ctx.Done():
			t.Fatal("isolated list fixture expired")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			command, err := nhf011ReadControl(out, sequence)
			if err != nil {
				t.Fatal(err)
			}
			if command == nil {
				continue
			}
			switch command.Action {
			case nhf011Outage:
				offline.Store(true)
				proxy.CloseClientConnections()
			case nhf011Restore:
				offline.Store(false)
			case nhf011Revoke:
				nha162SetPolicy(t, r, readerID, "  []\n")
			case nhf011Allow:
				nha162SetPolicy(t, r, readerID, readPolicy)
			case nhf011Snapshot:
				nhf003WritePrivate(t, out, fmt.Sprintf("stats-%02d.json", command.Sequence), map[string]any{"mutation_requests": writes.Load(), "active_streams": streams.Load(), "action_rpc_requests": actionCalls.Load(), "device_drivers": 0, "seeded_items": 10000})
			default:
				t.Fatal("unadmitted list control")
			}
			sequence = command.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", sequence), command)
		}
	}
}
