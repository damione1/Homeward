package integrations

import (
	"crypto/sha256"
	"math"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
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
	if !validLightEncoding(encoding) || c.casetaAlias != (domain.HAAlias{}) || c.casetaFanAlias != (domain.HAAlias{}) || len(c.descriptions) != 1 || c.descriptions[0] == nil || c.descriptions[0].SchemaId != contracts.LightSchema {
		return Candidate{}, ErrInvalidInput
	}
	c.lightEncoding = encoding
	return c, nil
}

// WithCasetaHA100V1 marks a candidate only after an operator-configured
// SourceKey/alias and the actually negotiated Host 1.3 have been joined by the
// worker. Host checks the resolved identity and exact capability before commit.
func (c Candidate) WithCasetaHA100V1(alias domain.HAAlias) (Candidate, error) {
	if _, err := alias.Value(); err != nil || c.lightEncoding != (LightEncoding{}) || c.casetaAlias != (domain.HAAlias{}) || c.casetaFanAlias != (domain.HAAlias{}) || !validCasetaHA100V1Description(c.descriptions) {
		return Candidate{}, ErrInvalidInput
	}
	c.casetaAlias = alias
	return c, nil
}

func validCasetaHA100V1Description(descriptions []*registry.CapabilityDescription) bool {
	if len(descriptions) != 1 || descriptions[0] == nil {
		return false
	}
	d := descriptions[0]
	if d.SchemaId != contracts.LightSchema || d.Major != 1 || d.Endpoint != "level" {
		return false
	}
	light := d.GetLight()
	if light == nil || !light.SetOn || light.ColorTempMireds != nil || light.BrightnessPercent == nil || light.TransitionSeconds == nil {
		return false
	}
	b, t := light.BrightnessPercent, light.TransitionSeconds
	return b.Minimum == 0 && b.Maximum == 100 && b.Step == nil &&
		t.Minimum == 0 && t.Maximum == 6553 && t.Step != nil && *t.Step == 1
}

func validLightEncoding(e LightEncoding) bool {
	return e.valid && e.scale >= 1 && e.scale <= 65535 && e.fingerprint != ([sha256.Size]byte{}) && (!e.ikea30W || e.scale == 254)
}

type lightEncodingSnapshot struct {
	kind        lightProfileKind
	encoding    LightEncoding
	endpoint    string
	binding     Binding
	casetaAlias domain.HAAlias
	description *registry.CapabilityDescription
}

func (s *lightEncodingSnapshot) sameDiscovery(kind lightProfileKind, encoding LightEncoding, binding Binding, alias domain.HAAlias, description *registry.CapabilityDescription) bool {
	return s != nil && s.kind == kind && s.encoding == encoding && s.binding == binding && s.casetaAlias == alias && proto.Equal(s.description, description)
}

type lightProfileKind uint8

const (
	lightProfileUnspecified lightProfileKind = iota
	lightProfileMQTT
	lightProfileCasetaHA100V1
)

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
	if snapshot.kind != lightProfileMQTT {
		// A direct SetLight sent to a qualified non-MQTT worker preserves its
		// native percentage. No MQTT quantization applies to that route.
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

// targetForPreparedHABrightness is the one projection authority for an HA
// source brightness. Direct SetLight continues to use targetForLight above.
// The HA 0..255 value is never reconstructed from an already quantized q.
func targetForPreparedHABrightness(brightness uint8, snapshot *lightEncodingSnapshot) (lightTarget, error) {
	if brightness == 0 || snapshot == nil {
		return lightTarget{}, ErrInvalidInput
	}
	target := lightTarget{profile: snapshot, proofAvailable: true}
	switch snapshot.kind {
	case lightProfileMQTT:
		if !validLightEncoding(snapshot.encoding) {
			return lightTarget{}, ErrInvalidInput
		}
		q := uint32(math.RoundToEven(float64(brightness) * float64(snapshot.encoding.scale) / 255))
		if q < 1 {
			q = 1 // HA MQTT's positive async_turn_on minimum.
		}
		if snapshot.encoding.ikea30W && q < 2 {
			q = 2 // NHA-180's qualified converter minimum.
		}
		target.brightness, target.hasBrightness = q, true
		target.effective = float64(q) * 100 / float64(snapshot.encoding.scale)
	case lightProfileCasetaHA100V1:
		// The worker receives the already converted 0..100 percentage in
		// the original wire. Its native target is not an MQTT brightness q.
	default:
		return lightTarget{}, ErrInvalidInput
	}
	return target, nil
}
