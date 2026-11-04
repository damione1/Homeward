package apiresources

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"

	"google.golang.org/protobuf/proto"
)

// DefinitionFence binds a definition to an authorized native catalogue cut.
// It carries no observed-state sequence, filter or response timestamp. Its
// construction is private to trusted composition, never a public request value.
type DefinitionFence struct{ digest [32]byte }

func NewDefinitionFence(configRevision uint64, configHash [32]byte, catalogueEpoch string, catalogueSequence uint64, admissionEpoch string, authorizationBinding [32]byte) (DefinitionFence, error) {
	if configRevision == 0 || configHash == ([32]byte{}) || authorizationBinding == ([32]byte{}) || len(catalogueEpoch) != 32 || len(admissionEpoch) == 0 || len(admissionEpoch) > 256 {
		return DefinitionFence{}, ErrProjection
	}
	if epoch, err := hex.DecodeString(catalogueEpoch); err != nil || len(epoch) != 16 {
		return DefinitionFence{}, ErrProjection
	}
	h := sha256.New()
	h.Write([]byte("homeward.capability-definition-fence/v2\x00"))
	var number [8]byte
	for _, value := range []uint64{configRevision, catalogueSequence} {
		binary.BigEndian.PutUint64(number[:], value)
		h.Write(number[:])
	}
	h.Write(configHash[:])
	h.Write(authorizationBinding[:])
	definitionString(h, catalogueEpoch)
	definitionString(h, admissionEpoch)
	var result DefinitionFence
	copy(result.digest[:], h.Sum(nil))
	return result, nil
}

func definitionString(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write(size[:])
	h.Write([]byte(value))
}

// BindDefinition returns an immutable copy with a weak HTTP-style etag: a
// capability's observations may change while its executable definition does not.
// The current server must independently authorize and compare this token before
// execution. Possession of a token never supplies authority or a native route.
func (c Capability) BindDefinition(fence DefinitionFence) (Capability, error) {
	if fence.digest == ([32]byte{}) || c.value == nil || c.trait.id() == "" {
		return Capability{}, ErrProjection
	}
	definition := c.Resource()
	definition.Etag = ""
	message := definition.ProtoReflect()
	trait := message.WhichOneof(message.Descriptor().Oneofs().ByName("trait"))
	if trait == nil {
		return Capability{}, ErrProjection
	}
	value := message.Get(trait).Message()
	if !value.IsValid() {
		return Capability{}, ErrProjection
	}
	if observation := value.Descriptor().Fields().ByName("observation"); observation != nil {
		value.Clear(observation)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(definition)
	if err != nil {
		return Capability{}, ErrProjection
	}
	schema, endpoint, err := c.source.Values()
	if err != nil {
		return Capability{}, ErrProjection
	}
	h := sha256.New()
	h.Write([]byte("homeward.capability-definition/v2\x00"))
	h.Write(fence.digest[:])
	for _, text := range []string{schema, endpoint, c.actionMessage} {
		definitionString(h, text)
	}
	h.Write(encoded)
	c.value = c.Resource()
	c.value.Etag = `W/"` + hex.EncodeToString(h.Sum(nil)) + `"`
	return c, nil
}
