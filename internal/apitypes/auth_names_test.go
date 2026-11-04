package apitypes

import (
	"strings"
	"testing"
)

func TestAuthNamesKeepResourceAndParentTypesDistinct(t *testing.T) {
	account, err := ParseAccountName("homes/h/accounts/a")
	if err != nil || account.Home().String() != "homes/h" || account.ResourceID() != "a" {
		t.Fatal(account, err)
	}
	session, err := ParseSessionName(account.String() + "/sessions/s")
	if err != nil || session.Account() != account || session.ResourceID() != "s" {
		t.Fatal(session, err)
	}
	factor, err := ParseFactorCredentialName(account.String() + "/factorCredentials/f")
	if err != nil || factor.Account() != account {
		t.Fatal(factor, err)
	}
	key, err := ParseMcpKeyName(account.String() + "/mcpKeys/k")
	if err != nil || key.Account() != account {
		t.Fatal(key, err)
	}
	policy, err := ParseAuthorizationPolicyName(account.String() + "/authorizationPolicy")
	if err != nil || policy.Account() != account {
		t.Fatal(policy, err)
	}
	if _, err := ParseSessionName(factor.String()); err == nil {
		t.Fatal("factor accepted as session")
	}
	if _, err := ParseMcpKeyName(session.String()); err == nil {
		t.Fatal("session accepted as key")
	}
	if _, err := ParseAccountName(policy.String()); err == nil {
		t.Fatal("policy accepted as account")
	}
	if (SessionName{}).Account().Valid() || (AccountName{}).Home().Valid() || (AuthorizationPolicyName{}).String() != "" {
		t.Fatal("zero value gained identity")
	}
}

func TestAuthNamesRejectAliasesAndMalformedParents(t *testing.T) {
	for _, name := range []string{"", "accounts/a", "homes/h/accounts/", "homes/h/accounts/A", "homes/h/accounts/-a", "homes/h/accounts/a-", "homes/h/accounts/a%2fb", "homes/h/accounts/a/b", "homes/h/accounts/é", "homes/h/accounts/" + strings.Repeat("a", 64)} {
		if _, err := ParseAccountName(name); err == nil {
			t.Errorf("accepted malformed account %q", name)
		}
	}
	for _, name := range []string{"homes/h/accounts/a/authorizationPolicy/x", "homes/h/authorizationPolicy", "homes/h/accounts/a/authorizationpolicy"} {
		if _, err := ParseAuthorizationPolicyName(name); err == nil {
			t.Errorf("accepted malformed singleton %q", name)
		}
	}
	if _, err := ParseSecondFactorPolicyName("homes/h/secondFactorPolicy"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSecondFactorPolicyName("homes/h/accounts/a/secondFactorPolicy"); err == nil {
		t.Fatal("accepted wrong singleton parent")
	}
}
