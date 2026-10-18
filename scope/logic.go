package scope

import (
	"embed"
	"io/fs"
)

//go:embed logic.json
var logicV1 []byte

//go:embed logic_mvp_v1.json
var logicMVPV1 []byte

//go:embed logic_mvp_sources/*.yaml
var logicMVPSourcesV1 embed.FS

// LogicV1 is the frozen NHA-002 set of 89 inactive, pending objects.
func LogicV1() []byte { return append([]byte(nil), logicV1...) }

// LogicMVPV1 is the separate NHA-158 selection amendment. It grants no
// execution support or activation by itself.
func LogicMVPV1() []byte { return append([]byte(nil), logicMVPV1...) }

// LogicMVPSourcesV1 contains documentary YAML snapshots for provenance checks.
// No runtime parser or action dispatcher is attached to this filesystem.
func LogicMVPSourcesV1() fs.FS { return logicMVPSourcesV1 }
