package cli

import (
	"strings"
	"time"

	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
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
	if contracts.AdmitAPIScope(value) != nil {
		return nil, fail("invalid_mcp_scope", ExitUsage)
	}
	return value, nil
}
func (c *mcpKeysCreate) Run(v *invocation) error {
	if _, err := mcpkeys.ParseClientName(c.Client); err != nil {
		return fail("invalid_mcp_client_name", ExitUsage)
	}
	request := &auth.IssueMcpKeyRequest{ClientName: c.Client}
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
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	request.Parent = r.credential.Account
	if contracts.AdmitAPIMcpKeyIssue(request) != nil {
		return fail("invalid_mcp_key_request", ExitUsage)
	}
	response, err := apiv2connect.NewMcpKeyServiceClient(r.client, r.base).IssueMcpKey(v.ctx, bearer(request, r.credential.Access))
	if err != nil {
		return err
	}
	if response.Msg == nil || response.Msg.McpKey == nil {
		return fail("invalid_mcp_key_response", ExitLocal)
	}
	if err := contracts.AdmitAPIMcpKey(response.Msg.McpKey, time.Now()); err != nil {
		return fail("invalid_mcp_key_response", ExitLocal)
	}
	name, parseErr := apitypes.ParseMcpKeyName(response.Msg.McpKey.Name)
	if parseErr != nil || name.Account().String() != r.credential.Account {
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
	PageSize  int32  `help:"Requested page size; zero selects default."`
	PageToken string `help:"Opaque continuation from the same listing."`
	Account   string `arg:"" optional:"" help:"Canonical account name; omitted means the signed-in account."`
}

func (c *mcpKeysList) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	request := &auth.ListMcpKeysRequest{Parent: r.credential.Account, PageSize: c.PageSize, PageToken: c.PageToken}
	if c.Account != "" {
		request.Parent = c.Account
	}
	response, err := apiv2connect.NewMcpKeyServiceClient(r.client, r.base).ListMcpKeys(v.ctx, bearer(request, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}

type mcpKeysRevoke struct {
	ID string `arg:"" help:"Canonical key name from keys list."`
}

func (c *mcpKeysRevoke) Run(v *invocation) error {
	r, err := v.authenticated()
	if err != nil {
		return err
	}
	defer r.close()
	response, err := apiv2connect.NewMcpKeyServiceClient(r.client, r.base).RevokeMcpKey(v.ctx, bearer(&auth.RevokeMcpKeyRequest{Name: c.ID}, r.credential.Access))
	if err != nil {
		return err
	}
	return v.emit(response.Msg)
}
