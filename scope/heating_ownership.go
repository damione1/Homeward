package scope

import _ "embed"

//go:embed heating_ownership_v1.json
var heatingOwnershipV1 []byte

// HeatingOwnershipV1 returns the NHA-011 decision corpus independently of the
// immutable scope v1 selection. Callers cannot mutate the embedded bytes.
func HeatingOwnershipV1() []byte { return append([]byte(nil), heatingOwnershipV1...) }
