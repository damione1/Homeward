package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Opt-in test-only conservative expiry, as in NHF003. Native SQL/auth still
// issues and rotates every credential; no identity or privilege is injected.
func nhf023ReceptionProxy(proxy *httputil.ReverseProxy, out string) {
	if os.Getenv("HOMEWARD_NHF023_BACKGROUND_REFRESH") != "1" {
		return
	}
	director := proxy.Director
	proxy.Director = func(request *http.Request) { director(request); request.Header.Set("Accept-Encoding", "identity") }
	var mu sync.Mutex
	var refreshes uint64
	store := func(name string, value any) error {
		file, err := os.CreateTemp(out, ".reception-observation-")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if err := json.NewEncoder(file).Encode(value); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Rename(file.Name(), filepath.Join(out, name))
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode != http.StatusOK {
			return nil
		}
		path := response.Request.URL.Path
		if path != authv1connect.AuthServiceStartLoginProcedure && path != authv1connect.AuthServiceRefreshSessionProcedure {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if path == authv1connect.AuthServiceRefreshSessionProcedure {
			refreshes++
			return store("background-refresh-observed.json", map[string]uint64{"native_refresh_http_200": refreshes})
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		_ = response.Body.Close()
		if err != nil {
			clear(data)
			return err
		}
		defer clear(data)
		if len(data) > 65536 {
			return errors.New("synthetic auth response exceeds bound")
		}
		value := &auth.StartLoginResponse{}
		if err := proto.Unmarshal(data, value); err != nil {
			return err
		}
		if tokens := value.GetDecision().GetAuthenticated(); tokens != nil {
			// Client safety subtracts30s. Refresh is due about240s after login,
			// after explicit service start/Activity suspension in this scenario.
			tokens.AccessExpiresAt = timestamppb.New(time.Now().Add(270 * time.Second))
			if err := store("background-short-access.json", map[string]int64{"advertised_expiry_unix": tokens.AccessExpiresAt.Seconds}); err != nil {
				return err
			}
		}
		encoded, err := proto.Marshal(value)
		if err != nil {
			return err
		}
		response.Body = &nhf003ObservedBody{reader: bytes.NewReader(encoded), data: encoded}
		response.ContentLength = int64(len(encoded))
		response.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
		return nil
	}
}
