package observability

import (
	"crypto/hmac"
	"crypto/sha256"
	"unicode/utf8"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
)

type IntegrationReference struct{ value [16]byte }

func (c *CursorCodec) IntegrationReference(instance domain.IntegrationInstanceID) (IntegrationReference, error) {
	text, err := instance.Value()
	if err != nil || !c.valid() || len(text) > 256 || !utf8.ValidString(text) {
		return IntegrationReference{}, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_INTEGRATION_INSTANCE_ID)
	}
	// Derive a distinct keyed-hash key; never use the AES key directly for data.
	derive := hmac.New(sha256.New, c.key[:])
	_, _ = derive.Write([]byte("homeward-observability-integration-reference-key-v1"))
	key := derive.Sum(nil)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(c.epoch.value[:])
	_, _ = mac.Write([]byte(text))
	digest := mac.Sum(nil)
	var reference IntegrationReference
	copy(reference.value[:], digest[:16])
	if reference.value == [16]byte{} {
		return IntegrationReference{}, failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAVAILABLE, obs.ObservabilityField_OBSERVABILITY_FIELD_AUTHOR)
	}
	return reference, nil
}
func (r IntegrationReference) Wire() (*obs.IntegrationReference, error) {
	if r.value == [16]byte{} {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_AUTHOR)
	}
	return &obs.IntegrationReference{Value: append([]byte(nil), r.value[:]...)}, nil
}
