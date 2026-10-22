package cli

import (
	"strings"
	"time"

	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"google.golang.org/protobuf/types/known/durationpb"
)

type mcpKeysCreate struct {
	Client   string   `arg:"" help:"Name of the client receiving this independent MCP credential."`
	Scope    []string `help:"Repeat FAMILY:OPERATION using protocol names without SCOPE_FAMILY_/SCOPE_OPERATION_ (for example STATE_REGISTRY:READ)."`
	Lifetime string   `help:"Positive Go duration up to 8760h; omitted means 720h (30 days)."`
}

func mcpKeyScope(text string) (*auth.Scope, error) {
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		return nil, fail("invalid_mcp_scope", ExitUsage)
	}
	family, ok := auth.ScopeFamily_value["SCOPE_FAMILY_"+parts[0]]
	if !ok {
		return nil, fail("invalid_mcp_scope", ExitUsage)
	}
	operation, ok := auth.ScopeOperation_value["SCOPE_OPERATION_"+parts[1]]
	if !ok {
		return nil, fail("invalid_mcp_scope", ExitUsage)
	}
	value := &auth.Scope{Family: auth.ScopeFamily(family), Operation: auth.ScopeOperation(operation)}
	if contracts.AdmitScope(value) != nil {
		return nil, fail("invalid_mcp_scope", ExitUsage)
	}
	return value, nil
}
func (c *mcpKeysCreate) Run(v *invocation) error {
	if _, err := mcpkeys.ParseClientName(c.Client); err != nil {
		return fail("invalid_mcp_client_name", ExitUsage)
	}
	request := &auth.CreateMcpKeyRequest{ClientName: c.Client}
	if c.Lifetime != "" {
		lifetime, err := time.ParseDuration(c.Lifetime)
		if err != nil {
			return fail("invalid_mcp_lifetime", ExitUsage)
		}
		request.Lifetime = durationpb.New(lifetime)
	}
	for _, text := range c.Scope {
		scope, err := mcpKeyScope(text)
		if err != nil {
			return err
		}
		request.Scopes = append(request.Scopes, scope)
	}
	if contracts.AdmitMcpKeyCreateRequest(request) != nil {
		return fail("invalid_mcp_key_request", ExitUsage)
	}
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := authv1connect.NewMcpKeyServiceClient(r.client, r.base).CreateMcpKey(v.ctx, bearer(request, r.credential.Access))
	if err != nil {
		return err
	}
	if response.Msg == nil || response.Msg.Key == nil {
		return fail("invalid_mcp_key_response", ExitLocal)
	}
	if _, err := contracts.AdmitMcpKey(response.Msg.Key, time.Now()); err != nil {
		return fail("invalid_mcp_key_response", ExitLocal)
	}
	if response.Msg.Key.AccountId.Value != r.credential.Account {
		return fail("invalid_mcp_key_response", ExitLocal)
	}
	if _, err := mcpkeys.ParseSecret(response.Msg.Secret); err != nil {
		return fail("invalid_mcp_key_response", ExitLocal)
	}
	// The explicit create command displays the secret once. No key is saved in
	// the interactive profile; subsequent list/revoke results have no secret.
	return v.emit(response.Msg)
}

type mcpKeysList struct {
	Account string `arg:"" optional:"" help:"Account ID; omitted means the signed-in account."`
}

func (c *mcpKeysList) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	request := &auth.ListMcpKeysRequest{}
	if c.Account != "" {
		request.AccountId = &auth.AccountId{Value: c.Account}
	}
	response, err := authv1connect.NewMcpKeyServiceClient(r.client, r.base).ListMcpKeys(v.ctx, bearer(request, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type mcpKeysRevoke struct {
	ID string `arg:"" help:"Stable key ID from keys list."`
}

func (c *mcpKeysRevoke) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := authv1connect.NewMcpKeyServiceClient(r.client, r.base).RevokeMcpKey(v.ctx, bearer(&auth.RevokeMcpKeyRequest{KeyId: &auth.McpKeyId{Value: c.ID}}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}
