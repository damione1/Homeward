package server

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func TestNHA203ConfigurationTLSAuthorizationPrecedesNameDiagnostics(t *testing.T) {
	fixture := nha149StartTLS(t)
	user, err := fixture.accounts.CreateAccount(fixture.ctx, nha056SessionRequest(&api.CreateAccountRequest{Parent: nha203SessionAccount(t, fixture.tokens.Session).Home().String(), Account: &api.Account{LoginName: "nha203-user", DisplayName: "Synthetic user", InitialPassword: "NHA203 isolated strong password", Role: api.AccountRole_ACCOUNT_ROLE_USER}}, fixture.token))
	if err != nil {
		t.Fatal(err)
	}
	tokens := nha162RunLogin(t, fixture.ctx, fixture.authentication, user.Msg.LoginName, "NHA203 isolated strong password")
	request := &api.GetConfigurationDocumentRequest{Name: "SYNTHETIC_INVALID_NAME_CANARY"}
	_, err = fixture.api.GetConfigurationDocument(fixture.ctx, nha056SessionRequest(request, tokens.AccessToken))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("unauthorized name diagnostics leaked", err)
	}
	var denied *connect.Error
	if !errors.As(err, &denied) {
		t.Fatal(err)
	}
	for _, detail := range denied.Details() {
		value, e := detail.Value()
		if e != nil {
			t.Fatal(e)
		}
		if _, ok := value.(*errdetails.BadRequest); ok {
			t.Fatal("unauthorized response included field diagnostics")
		}
	}
	_, err = fixture.api.GetConfigurationDocument(fixture.ctx, nha056SessionRequest(request, fixture.token))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("authorized invalid name not diagnosed", err)
	}
	var invalid *connect.Error
	if !errors.As(err, &invalid) {
		t.Fatal(err)
	}
	found := false
	for _, detail := range invalid.Details() {
		value, e := detail.Value()
		if e != nil {
			t.Fatal(e)
		}
		if bad, ok := value.(*errdetails.BadRequest); ok {
			if len(bad.FieldViolations) != 1 || bad.FieldViolations[0].Field != "name" || bad.FieldViolations[0].Reason != "INVALID_NAME" {
				t.Fatal("wrong admitted field path/rule")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("authorized invalid name lacked canonical BadRequest")
	}
}
