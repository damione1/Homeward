package heatingownership

import (
	"reflect"

	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

// ParseForManifest preserves the historical decision and Parse contract. The
// only non-baseline admission is the complete published Ecobee amendment; an
// arbitrary parsed set, unknown revision or zero Manifest grants no policy.
func ParseForManifest(data []byte, manifest registry.Manifest) (Decision, error) {
	revision, present := manifest.Revision()
	if !present {
		return Decision{}, ErrInvalidDecision
	}
	switch revision {
	case scope.BaselineV1():
		// Revision is present here only for the canonical frozen v1 bytes.
		return Parse(data, manifest)
	case scope.EcobeeMVPV1():
		published, err := registry.LoadManifest(scope.EcobeeMVPV1())
		if err != nil || !reflect.DeepEqual(manifest, published) {
			return Decision{}, ErrInvalidDecision
		}
		baseline, err := registry.LoadManifest(scope.BaselineV1())
		if err != nil {
			return Decision{}, ErrInvalidDecision
		}
		anchor, admitted := manifest.BaselineDigest()
		baselineDigest, valid := baseline.Digest()
		if !admitted || !valid || anchor != baselineDigest {
			return Decision{}, ErrInvalidDecision
		}
		decision, err := Parse(data, baseline)
		if err != nil {
			return Decision{}, err
		}
		for _, pair := range decision.pairs {
			if !hasSource(manifest, pair.hilo, "hilo") || !hasSource(manifest, pair.neviweb, "neviweb130") {
				return Decision{}, ErrInvalidDecision
			}
		}
		decision.manifest = manifest
		return decision, nil
	default:
		return Decision{}, ErrInvalidDecision
	}
}
