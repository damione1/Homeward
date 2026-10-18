// Package scope embeds the immutable v1 selection used by the server registry.
package scope

import _ "embed"

//go:embed manifest.json
var manifestV1 []byte

func ManifestV1() []byte { return append([]byte(nil), manifestV1...) }
