package integrations

import (
	"crypto/sha256"
	"math"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
)

// LightEncoding is a private Host/adapter contract, not a capability or wire
// promise. A discovery fingerprint fences a changed converter profile even
// when the public LightConstraints bytes are unchanged.
type LightEncoding struct {
	scale       uint32
	fingerprint [sha256.Size]byte
	ikea30W     bool
	valid       bool
}

func NewMQTTLightEncoding(scale uint32, fingerprint [sha256.Size]byte, ikea30W bool) (LightEncoding, error) {
	if scale == 0 || scale > 65535 || fingerprint == ([sha256.Size]byte{}) || (ikea30W && scale != 254) {
		return LightEncoding{}, ErrInvalidInput
	}
	return LightEncoding{scale: scale, fingerprint: fingerprint, ikea30W: ikea30W, valid: true}, nil
}

// WithLightEncoding attaches a validated, value-owned profile to one light
// candidate. The Host revalidates it on admission, including private fields.
func (c Candidate) WithLightEncoding(encoding LightEncoding) (Candidate, error) {
	if !validLightEncoding(encoding) || len(c.descriptions) != 1 || c.descriptions[0] == nil || c.descriptions[0].SchemaId != contracts.LightSchema {
		return Candidate{}, ErrInvalidInput
	}
	c.lightEncoding = encoding
	return c, nil
}

func validLightEncoding(e LightEncoding) bool {
	return e.valid && e.scale >= 1 && e.scale <= 65535 && e.fingerprint != ([sha256.Size]byte{}) && (!e.ikea30W || e.scale == 254)
}

type lightEncodingSnapshot struct {
	encoding LightEncoding
	endpoint string
}

// lightTarget is captured once under the route fence. The same encoded value
// drives the MQTT payload and the tracker predicate; the original request is
// retained unchanged for command identity and deduplication.
type lightTarget struct {
	profile        *lightEncodingSnapshot
	brightness     uint32
	hasBrightness  bool
	effective      float64
	proofAvailable bool
	inferredOn     bool
	hasInferredOn  bool
}

func targetForLight(request *actions.CommandRequest, snapshot *lightEncodingSnapshot) lightTarget {
	target := lightTarget{profile: snapshot, proofAvailable: true}
	if snapshot == nil || request == nil || request.CapabilitySchemaId != contracts.LightSchema {
		return target
	}
	arg := request.GetArguments().GetSetLight()
	if arg == nil || arg.BrightnessPercent == nil {
		return target
	}
	e := snapshot.encoding
	q := uint32(math.Round(*arg.BrightnessPercent * float64(e.scale) / 100))
	if e.ikea30W {
		switch {
		case arg.On != nil && !*arg.On:
			if q > 0 {
				// The converter's OFF branch ignores positive brightness.
				target.proofAvailable = false
			}
		case arg.On != nil && *arg.On:
			if q < 2 {
				q = 2
			}
		case *arg.BrightnessPercent > 0:
			if q < 2 {
				q = 2
			}
			target.inferredOn, target.hasInferredOn = true, true
		default:
			target.inferredOn, target.hasInferredOn = false, true
		}
	} else if (q == 0 && ((arg.On != nil && *arg.On) || *arg.BrightnessPercent > 0)) || (arg.On != nil && !*arg.On && q > 0) {
		// Unknown converters can infer OFF at zero or discard brightness
		// from an explicit OFF. Do not claim a numeric target for either.
		target.proofAvailable = false
	}
	target.brightness = q
	target.hasBrightness = true
	target.effective = float64(q) * 100 / float64(e.scale)
	return target
}
