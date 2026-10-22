package configactivation

import "context"

// CompileEditableSourceGroup performs semantic preparation only. Activation
// repeats it under the existing Owner operation before workspace staging.
func (o *Owner) CompileEditableSourceGroup(ctx context.Context, replacement SourceGroup) (SourceCompilation, error) {
	if o == nil || !usableContext(ctx) || !replacement.valid() {
		return SourceCompilation{}, ErrInvalidStartup
	}
	o.mu.RLock()
	current := o.current
	o.mu.RUnlock()
	if !current.Valid() {
		return SourceCompilation{}, ErrInvalidStartup
	}
	groups, err := replaceSourceGroup(current.extraGroups, &replacement)
	if err != nil {
		return SourceCompilation{}, err
	}
	input, _, err := o.captureSourceContext(ctx)
	if err != nil {
		return SourceCompilation{}, err
	}
	compiler, err := o.sourceCompiler(ctx, input, current.operational)
	if err != nil {
		return SourceCompilation{}, err
	}
	compilation, err := CompileSourceGroups(groups, compiler.manifest, compiler.catalogue)
	if err != nil {
		return SourceCompilation{}, err
	}
	compilation.compilationContext = input
	return compilation, nil
}
