package contracts

// These are the Homeward source identity and wire boundary shared by policy
// admission, storage and clients; they never describe the HA bundle schema.
const AuthorizationPolicySchema = "homeward.authorization/1"
const AuthorizationPolicySyntaxProfile = "homeward.authorization-yaml/1"
const MaxAuthorizationPolicySourceBytes = 1 << 20
