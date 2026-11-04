package apiresources

import api "github.com/damione1/Homeward/gen/home/api/v2"

// WithActionAccess attaches an established exact-slot grant to an immutable
// projection. Hardware support and media route dependencies remain independent.
func (c Capability) WithActionAccess(access api.ActionAccess) (Capability, error) {
	if c.value == nil || (access != api.ActionAccess_ACTION_ACCESS_ALLOWED && access != api.ActionAccess_ACTION_ACCESS_DENIED) {
		return Capability{}, ErrProjection
	}
	c.value = c.Resource()
	c.value.ActionAccess = access
	return c, nil
}
