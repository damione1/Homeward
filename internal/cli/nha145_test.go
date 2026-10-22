package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha145API struct {
	mu sync.Mutex
	authv1connect.UnimplementedMcpKeyServiceHandler
	auth    *nha143API
	key     *auth.McpKey
	secret  string
	revoked bool
}

func (f *nha145API) CreateMcpKey(_ context.Context, r *connect.Request[auth.CreateMcpKeyRequest]) (*connect.Response[auth.CreateMcpKeyResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.auth.record("key-create", r.Header()); err != nil {
		return nil, err
	}
	now := time.Now()
	f.key = &auth.McpKey{Id: &auth.McpKeyId{Value: "key-A"}, AccountId: &auth.AccountId{Value: "account-1"}, ClientName: r.Msg.ClientName, Audience: auth.CredentialAudience_CREDENTIAL_AUDIENCE_MCP, Scopes: r.Msg.Scopes, CreatedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(30 * 24 * time.Hour))}
	return connect.NewResponse(&auth.CreateMcpKeyResponse{Key: f.key, Secret: f.secret}), nil
}
func (f *nha145API) ListMcpKeys(_ context.Context, r *connect.Request[auth.ListMcpKeysRequest]) (*connect.Response[auth.ListMcpKeysResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.auth.record("key-list", r.Header()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&auth.ListMcpKeysResponse{Keys: []*auth.McpKey{f.key}}), nil
}
func (f *nha145API) RevokeMcpKey(_ context.Context, r *connect.Request[auth.RevokeMcpKeyRequest]) (*connect.Response[auth.RevokeMcpKeyResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.auth.record("key-revoke", r.Header()); err != nil {
		return nil, err
	}
	if r.Msg.KeyId.Value != f.key.Id.Value {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	f.revoked = true
	return connect.NewResponse(&auth.RevokeMcpKeyResponse{}), nil
}
func TestNHA145KongCreateListRevokeAndOneTimeSecret(t *testing.T) {
	server, baseAPI, ca := nha143TLS(t, nha143Access)
	secret, err := mcpkeys.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	text, _ := secret.Text()
	api := &nha145API{auth: baseAPI, secret: text}
	server.Config.Handler.(*http.ServeMux).Handle(authv1connect.NewMcpKeyServiceHandler(api))
	dir := nha143Private(t)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "mcp", "--url", server.URL, "--ca", ca)
	nha143Login(t, dir, "mcp")
	for _, command := range [][]string{{"keys", "--help"}, {"keys", "create", "--help"}, {"keys", "list", "--help"}, {"keys", "revoke", "--help"}} {
		nha143Run(t, dir, terminal, ExitOK, command...)
	}
	for _, arguments := range [][]string{{"keys", "create", "A", "--lifetime", "0s"}, {"keys", "create", "A", "--lifetime", "-1s"}, {"keys", "create", "A", "--lifetime", "8761h"}, {"keys", "create", "A", "--scope", "ACCOUNTS:NOPE"}, {"keys", "create", "A", "--scope", "ACCOUNTS:READ", "--scope", "ACCOUNTS:READ"}} {
		nha143Run(t, dir, terminal, ExitUsage, append([]string{"--profile", "mcp"}, arguments...)...)
	}
	created := nha143Run(t, dir, terminal, ExitOK, "--profile", "mcp", "keys", "create", "Desktop assistant", "--scope", "ACCOUNTS:READ")
	if !strings.Contains(created, text) {
		t.Fatal("one-time secret absent from explicit creation")
	}
	listed := nha143Run(t, dir, terminal, ExitOK, "--profile", "mcp", "keys", "list")
	if strings.Contains(listed, text) || strings.Contains(listed, "secret") {
		t.Fatal("list leaked secret")
	}
	nha143Run(t, dir, terminal, ExitOK, "--profile", "mcp", "keys", "revoke", "key-A")
	api.mu.Lock()
	revoked := api.revoked
	api.mu.Unlock()
	if !revoked {
		t.Fatal("CLI failed to revoke key")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mcp.credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), text) {
		t.Fatal("MCP secret replaced interactive profile")
	}
	var stored credentials
	if json.Unmarshal(raw, &stored) != nil || stored.Access != nha143Access {
		t.Fatal("interactive profile changed")
	}
}
