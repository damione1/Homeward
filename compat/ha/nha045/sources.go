package nha045

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const (
	GoVersion = "go1.27.2"
	HAVersion = "2026.10.1"
	HAPython  = "3.14.6"
	HAJinja   = "3.1.6"
	Image     = "ghcr.io/home-assistant/home-assistant@sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7"
)

// Hashes are from the already pinned image/042 source extractions, not from a
// moving upstream branch. The loaded modules must match before any scenario.
var haSources = []sourceWire{
	{"homeassistant.helpers.script", "75376abc1686bcfde2f5d235863728b630b91f703ad03eb1a4b6a22eb8387ee3"},
	{"homeassistant.components.script", "8afceb2f5a092830ca98f5ddd6349f131f5d8d555dda92c31782e71efc8df69e"},
	{"homeassistant.helpers.script_variables", "c70c73cdd5979f05fd16997762c4cddb9a0dd1bfb4b7f23d7a11c4ec325bf9ea"},
	{"homeassistant.components.automation", "d0bd002c7ee1f7b053d076815a5fb76f81d73808a1ee286ebc1d788241f52da9"},
	{"homeassistant.helpers.event", "caafbdd2437c4912e21dceca588577f24270a743e80a217e308175818de9e269"},
	{"homeassistant.helpers.trace", "3735ccdf467c2e5242055948803e08b6fd9e83a8d404d81562edebd1f924a6f1"},
	{"homeassistant.components.homeassistant.triggers.state", "dc72a3a3f606b486165bf72d304cf7643cf7d4592971675f1ccab51aed9fffd8"},
	{"homeassistant.components.homeassistant.triggers.event", "c99566aebd9a26857fc9b64f5d0af7bbb46d46e1464291f4aadab49acc89aff2"},
	{"homeassistant.components.automation.trace", "f8da595e040a7ab80f2f9c963c18209bf554ef83d6ab0f0f0f422bc5df4124cd"},
}

func validateSources(runtime runtimeWire) error {
	if len(runtime.SourceSHA256) == 0 || len(runtime.SourceSHA256) > 32 {
		return fail(InvalidObservation, "source provenance absent or oversized")
	}
	seen := make(map[string]bool)
	for _, source := range runtime.SourceSHA256 {
		if source.Path == "" || len(source.Path) > 128 || !admittedSHA(source.SHA256) || seen[source.Path] {
			return fail(InvalidObservation, "invalid/duplicate source provenance")
		}
		seen[source.Path] = true
		if runtime.Name == "go" {
			if source.Path != "probe" {
				return fail(InvalidObservation, "unknown Go source evidence")
			}
		} else {
			found := false
			for _, expected := range haSources {
				if source.Path == expected.Path {
					found = true
					if source.SHA256 != expected.SHA256 {
						return fail(VersionMismatch, "HA module source drift: "+source.Path)
					}
				}
			}
			if !found {
				return fail(InvalidObservation, "unrecognized HA module evidence")
			}
		}
	}
	if runtime.Name == "ha" {
		if len(seen) != len(haSources) {
			return fail(InvalidObservation, "HA module evidence incomplete")
		}
		for _, source := range haSources {
			if !seen[source.Path] {
				return fail(InvalidObservation, "HA module evidence missing: "+source.Path)
			}
		}
	}
	return nil
}
func executableSource() (sourceWire, error) {
	path, e := os.Executable()
	if e != nil {
		return sourceWire{}, e
	}
	file, e := os.Open(path)
	if e != nil {
		return sourceWire{}, e
	}
	defer file.Close()
	hash := sha256.New()
	n, e := io.Copy(hash, io.LimitReader(file, 128<<20+1))
	if e != nil {
		return sourceWire{}, e
	}
	if n > 128<<20 {
		return sourceWire{}, fmt.Errorf("probe binary exceeds provenance bound")
	}
	return sourceWire{"probe", hex.EncodeToString(hash.Sum(nil))}, nil
}
