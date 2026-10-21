package registry

// ScopeManifest returns the immutable admission snapshot owned by this
// Repository. Its maps remain private; callers cannot change the active set.
func (r *Repository) ScopeManifest() (Manifest, error) {
	if r == nil {
		return Manifest{}, ErrInvalidInput
	}
	if _, ok := r.manifest.Revision(); !ok {
		return Manifest{}, ErrInvalidManifest
	}
	return r.manifest, nil
}
