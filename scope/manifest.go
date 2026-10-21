// Package scope embeds immutable documentary selections used by the server registry.
package scope

import _ "embed"

//go:embed manifest.json
var manifestV1 []byte

func ManifestV1() []byte { return append([]byte(nil), manifestV1...) }

//go:embed ecobee_mvp_v1.json
var ecobeeMVPV1 []byte

// EcobeeMVPV1Bytes is the separate NHA-137 amendment, never a replacement of v1.
func EcobeeMVPV1Bytes() []byte { return append([]byte(nil), ecobeeMVPV1...) }
