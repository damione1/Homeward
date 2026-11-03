package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	listwire "github.com/damione1/Homeward/gen/home/lists/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/lists"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func nha200Info(t *testing.T, err error) *errdetails.ErrorInfo {
	t.Helper()
	var public *connect.Error
	if !errors.As(err, &public) {
		t.Fatal("missing native RPC error", err)
	}
	seen := make(map[string]bool)
	var info *errdetails.ErrorInfo
	for _, detail := range public.Details() {
		if seen[detail.Type()] {
			t.Fatal("duplicate error detail type", detail.Type())
		}
		seen[detail.Type()] = true
		value, err := detail.Value()
		if err != nil {
			t.Fatal(err)
		}
		if typed, ok := value.(*errdetails.ErrorInfo); ok {
			info = typed
		}
	}
	if info == nil || info.Domain != apierrors.Domain() {
		t.Fatal("canonical ErrorInfo absent or foreign")
	}
	return info
}

func TestNHA200AddItemAuthorizesObjectBeforePayloadAndCASOverTLS(t *testing.T) {
	r := nha162NewRPCRig(t)
	create := func(name string) *listwire.MutationReceipt {
		response, err := r.client.CreateList(r.ctx, nha162Bearer(&listwire.CreateListRequest{Name: name}, r.adminToken))
		if err != nil {
			t.Fatal(err)
		}
		return response.Msg.Receipt
	}
	allowed, forbidden := create("Synthetic writable tasks"), create("Synthetic private tasks")
	const password = "NHA200 synthetic password canary"
	account, err := r.accounts.CreateAccount(r.ctx, nha162Bearer(&auth.CreateAccountRequest{LoginName: "NHA200 scoped writer", DisplayName: "NHA200 scoped writer", InitialPassword: password, Role: auth.AccountRole_ACCOUNT_ROLE_USER}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	user, err := contracts.ParseAccountID(account.Msg.Account.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
	nha162SetPolicy(t, r, user, fmt.Sprintf("  - scope: {family: lists, operation: write}\n    objects: {lists: {explicit: [%q]}}\n", allowed.ListId.Value))
	token := r.login(t, "NHA200 scoped writer", password)
	for _, target := range []*listwire.ListId{forbidden.ListId, {Value: "list-synthetic-absent"}, nil} {
		_, err := r.client.AddItem(r.ctx, nha162Bearer(&listwire.AddItemRequest{ListId: target, Item: &listwire.ItemPayload{Summary: "SYNTHETIC_SECRET_CANARY\n"}}, token))
		if connect.CodeOf(err) != connect.CodePermissionDenied || nha200Info(t, err).Reason != "ACCESS_DENIED" {
			t.Fatalf("unauthorized payload diagnosed before target admission: %v", err)
		}
		public := err.(*connect.Error)
		if len(public.Details()) != 1 || strings.Contains(public.Message(), "CANARY") {
			t.Fatal("forbidden target leaked diagnostic information")
		}
		for _, detail := range public.Details() {
			if detail.Type() == "google.rpc.BadRequest" {
				t.Fatal("forbidden field diagnostics escaped")
			}
		}
	}
	_, err = r.client.AddItem(r.ctx, nha162Bearer(&listwire.AddItemRequest{
		ListId: allowed.ListId, ExpectedListRevision: &listwire.ListRevision{ListId: allowed.ListId},
		Item: &listwire.ItemPayload{Description: nha200String("SYNTHETIC_SECRET_CANARY\x00")},
	}, token))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || nha200Info(t, err).Reason != "VALIDATION_FAILED" {
		t.Fatalf("authorized bad request lost diagnostics: %v", err)
	}
	var bad *errdetails.BadRequest
	for _, detail := range err.(*connect.Error).Details() {
		if value, ok := nha200DetailValue(t, detail).(*errdetails.BadRequest); ok {
			bad = value
		}
	}
	if bad == nil || len(bad.FieldViolations) != 4 {
		t.Fatal("independent CAS, presence, text and description violations not collected")
	}
	want := []string{"expected_list_revision.number", "item.completed", "item.description", "item.summary"}
	for i, issue := range bad.FieldViolations {
		if issue.Field != want[i] || strings.Contains(issue.Description, "CANARY") {
			t.Fatal("unstable or unsafe field diagnostics")
		}
	}
	completed := false
	response, err := r.client.AddItem(r.ctx, nha162Bearer(&listwire.AddItemRequest{ListId: allowed.ListId, ExpectedListRevision: allowed.ListRevision, Item: &listwire.ItemPayload{Summary: "Explicit false remains admitted", Completed: &completed}}, token))
	if err != nil || response.Msg.Receipt == nil {
		t.Fatal("explicit false rejected or invalid attempts changed CAS", err)
	}
}

func nha200String(value string) *string { return &value }
func nha200DetailValue(t *testing.T, detail *connect.ErrorDetail) any {
	t.Helper()
	value, err := detail.Value()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestNHA200RevocationWinsOverCompletedRepresentationValidation(t *testing.T) {
	r := nha162NewRPCRig(t)
	created, err := r.client.CreateList(r.ctx, nha162Bearer(&listwire.CreateListRequest{Name: "Synthetic validation barrier"}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	ctx := nha162ImportContext(t, r)
	_, err = withListTarget(r.lists.handler, ctx, created.Msg.Receipt.ListId, auth.ScopeOperation_SCOPE_OPERATION_WRITE,
		func(domain.ListID) (int, error) {
			nha162SetPolicy(t, r, r.admin, "  []\n")
			return 0, listRPCCode(lists.Invalid)
		})
	if connect.CodeOf(err) != connect.CodePermissionDenied || nha200Info(t, err).Reason != "ACCESS_DENIED" {
		t.Fatal("revoked validation result escaped", err)
	}
}

type nha200FaultProbe struct {
	apiv2connect.UnimplementedHomeServiceHandler
	reason api.ApiErrorReason
}

func (p nha200FaultProbe) GetEntity(ctx context.Context, _ *connect.Request[api.GetEntityRequest]) (*connect.Response[api.Entity], error) {
	return nil, apiRPCError(ctx, apierrors.New(p.reason))
}

// This probe exercises the production error adapter and generated codecs over
// real TLS/h2. It is not a production entity handler; object admission is tested
// independently above through the real composed list/session/SQL route.
func TestNHA200CanonicalErrorsMatchConnectAndGRPCOverTLS(t *testing.T) {
	values := api.ApiErrorReason(0).Descriptor().Values()
	for i := 1; i < values.Len(); i++ {
		reason := api.ApiErrorReason(values.Get(i).Number())
		t.Run(reason.String(), func(t *testing.T) {
			_, handler := apiv2connect.NewHomeServiceHandler(nha200FaultProbe{reason: reason})
			server := httptest.NewUnstartedServer(handler)
			server.EnableHTTP2 = true
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
			server.StartTLS()
			defer server.Close()
			for _, options := range [][]connect.ClientOption{nil, {connect.WithProtoJSON()}, {connect.WithGRPC()}} {
				httpClient := server.Client()
				httpClient.Timeout = 5 * time.Second
				client := apiv2connect.NewHomeServiceClient(httpClient, server.URL, options...)
				_, err := client.GetEntity(context.Background(), connect.NewRequest(&api.GetEntityRequest{Name: "homes/demo/entities/probe"}))
				if int32(connect.CodeOf(err)) != int32(apierrors.New(reason).Code()) {
					t.Fatal("protocol changed canonical code", err)
				}
				info := nha200Info(t, err)
				if info.Reason != strings.TrimPrefix(reason.String(), "API_ERROR_REASON_") {
					t.Fatal("protocol changed canonical reason")
				}
			}
		})
	}
}

func TestNHA200UncertaintySurvivesCancellationAndUnknownCausesStayPrivate(t *testing.T) {
	for _, cause := range []error{integrations.ErrDurableCommitUnknown, integrations.ErrCommandOutcomeIndeterminate} {
		fault := apiFault(errors.Join(context.Canceled, cause))
		if fault.Code() != code.Code_UNKNOWN {
			t.Fatal("cancellation erased possible durable effect")
		}
		denied := apiFault(errors.Join(authorization.ErrDenied, cause))
		if denied.Reason() != api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED {
			t.Fatal("uncertainty leaked through authorization denial")
		}
	}
	public := coreRPCError(context.Background(), errors.New("SYNTHETIC_SECRET_CANARY"))
	if nha200Info(t, public).Reason != "INTERNAL_FAILURE" || strings.Contains(public.Error(), "CANARY") {
		t.Fatal("unknown cause leaked or became retryable")
	}
	var typedNil *lists.Error
	if apiFault(typedNil).Reason() != api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE {
		t.Fatal("typed nil error admitted")
	}
}

func TestNHA200ListCodeMappingCoversItsDeclaredSet(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../lists/model.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		first, ok := group.Specs[0].(*ast.ValueSpec)
		if !ok {
			continue
		}
		typ, ok := first.Type.(*ast.Ident)
		if !ok || typ.Name != "Code" {
			continue
		}
		for _, spec := range group.Specs {
			value := spec.(*ast.ValueSpec)
			for range value.Names {
				count++
				if listFault(lists.Code(count)).Reason() == api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE {
					t.Fatal("new list code lacks an explicit canonical mapping", strconv.Itoa(count))
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("domain Code declaration not found")
	}
}
