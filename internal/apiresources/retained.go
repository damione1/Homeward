package apiresources

import (
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

// RetainedBytes conservatively charges the immutable public value, private
// source binding and native media route to the existing view capture budget.
// It owns no quota or storage; callers apply their existing aggregate limits.
func (c Capability) RetainedBytes() (int, error) {
	schema, endpoint, err := c.source.Values()
	if err != nil || c.value == nil || c.value.Trait == nil || c.trait.id() == "" {
		return 0, ErrProjection
	}
	if _, err := apitypes.ParseCapabilityName(c.value.Name); err != nil {
		return 0, ErrProjection
	}
	public, private := proto.Size(c.value), proto.Size(c.nativeMediaRoute)
	maximum := uint64(^uint(0) >> 1)
	charge := uint64(512) + uint64(len(schema)) + uint64(len(endpoint)) + uint64(len(c.actionMessage))
	if charge > maximum {
		return 0, ErrProjection
	}
	for _, size := range []int{public, private} {
		if size < 0 || uint64(size) > (maximum-charge)/4 {
			return 0, ErrProjection
		}
		charge += 4 * uint64(size)
	}
	return int(charge), nil
}
