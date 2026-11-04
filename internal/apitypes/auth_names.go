package apitypes

import "strings"

// Authentication resources have distinct admitted names. A name proves syntax,
// never ownership, a credential audience, or permission to access its parent.
type AccountName struct{ value string }
type InvitationName struct{ value string }
type SessionName struct{ value string }
type FactorCredentialName struct{ value string }
type McpKeyName struct{ value string }
type AuthorizationPolicyName struct{ account AccountName }
type SecondFactorPolicyName struct{ home HomeName }

func (n AccountName) String() string          { return n.value }
func (n InvitationName) String() string       { return n.value }
func (n SessionName) String() string          { return n.value }
func (n FactorCredentialName) String() string { return n.value }
func (n McpKeyName) String() string           { return n.value }
func (n AccountName) Valid() bool             { return n.value != "" }
func (n InvitationName) Valid() bool          { return n.value != "" }
func (n SessionName) Valid() bool             { return n.value != "" }
func (n FactorCredentialName) Valid() bool    { return n.value != "" }
func (n McpKeyName) Valid() bool              { return n.value != "" }
func (n AuthorizationPolicyName) Valid() bool { return n.account.Valid() }
func (n SecondFactorPolicyName) Valid() bool  { return n.home.Valid() }

func ParseAccountName(value string) (AccountName, error) {
	if err := resourceName(value, "homes", "accounts"); err != nil {
		return AccountName{}, err
	}
	return AccountName{value}, nil
}
func ParseInvitationName(value string) (InvitationName, error) {
	if err := resourceName(value, "homes", "invitations"); err != nil {
		return InvitationName{}, err
	}
	return InvitationName{value}, nil
}
func ParseSessionName(value string) (SessionName, error) {
	if err := resourceName(value, "homes", "accounts", "sessions"); err != nil {
		return SessionName{}, err
	}
	return SessionName{value}, nil
}
func ParseFactorCredentialName(value string) (FactorCredentialName, error) {
	if err := resourceName(value, "homes", "accounts", "factorCredentials"); err != nil {
		return FactorCredentialName{}, err
	}
	return FactorCredentialName{value}, nil
}
func ParseMcpKeyName(value string) (McpKeyName, error) {
	if err := resourceName(value, "homes", "accounts", "mcpKeys"); err != nil {
		return McpKeyName{}, err
	}
	return McpKeyName{value}, nil
}
func ParseAuthorizationPolicyName(value string) (AuthorizationPolicyName, error) {
	parent, ok := strings.CutSuffix(value, "/authorizationPolicy")
	if !ok {
		return AuthorizationPolicyName{}, ErrResourceName
	}
	account, err := ParseAccountName(parent)
	if err != nil {
		return AuthorizationPolicyName{}, err
	}
	return AuthorizationPolicyName{account}, nil
}
func ParseSecondFactorPolicyName(value string) (SecondFactorPolicyName, error) {
	parent, ok := strings.CutSuffix(value, "/secondFactorPolicy")
	if !ok {
		return SecondFactorPolicyName{}, ErrResourceName
	}
	home, err := ParseHomeName(parent)
	if err != nil {
		return SecondFactorPolicyName{}, err
	}
	return SecondFactorPolicyName{home}, nil
}

func authNameID(value string) string              { return value[strings.LastIndexByte(value, '/')+1:] }
func (n AccountName) ResourceID() string          { return authNameID(n.value) }
func (n InvitationName) ResourceID() string       { return authNameID(n.value) }
func (n SessionName) ResourceID() string          { return authNameID(n.value) }
func (n FactorCredentialName) ResourceID() string { return authNameID(n.value) }
func (n McpKeyName) ResourceID() string           { return authNameID(n.value) }

func (n AccountName) Home() HomeName {
	if !n.Valid() {
		return HomeName{}
	}
	return HomeName{n.value[:strings.LastIndex(n.value, "/accounts/")]}
}
func (n InvitationName) Home() HomeName {
	if !n.Valid() {
		return HomeName{}
	}
	return HomeName{n.value[:strings.LastIndex(n.value, "/invitations/")]}
}
func (n SessionName) Account() AccountName {
	if !n.Valid() {
		return AccountName{}
	}
	return AccountName{n.value[:strings.LastIndex(n.value, "/sessions/")]}
}
func (n FactorCredentialName) Account() AccountName {
	if !n.Valid() {
		return AccountName{}
	}
	return AccountName{n.value[:strings.LastIndex(n.value, "/factorCredentials/")]}
}
func (n McpKeyName) Account() AccountName {
	if !n.Valid() {
		return AccountName{}
	}
	return AccountName{n.value[:strings.LastIndex(n.value, "/mcpKeys/")]}
}
func (n AuthorizationPolicyName) Account() AccountName { return n.account }
func (n SecondFactorPolicyName) Home() HomeName        { return n.home }
func (n AuthorizationPolicyName) String() string {
	if !n.Valid() {
		return ""
	}
	return n.account.String() + "/authorizationPolicy"
}
func (n SecondFactorPolicyName) String() string {
	if !n.Valid() {
		return ""
	}
	return n.home.String() + "/secondFactorPolicy"
}
