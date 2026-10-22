package configactivation

import (
	"context"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configcompilation"
)

// SourceRuntimeFactory rebuilds compilation from detached historical inputs.
// Its output grants neither identity nor live dispatch/capability authority.
type SourceRuntimeFactory func(context.Context, configcompilation.Context, config.Config) (*SourceRuntimeCompiler, error)

func (o *Owner) sourceCompiler(ctx context.Context, input configcompilation.Context, operational config.Config) (*SourceRuntimeCompiler, error) {
	if !input.Valid() || o.runtime.SourceFactory == nil {
		return nil, ErrProgramCompositionRequired
	}
	compiler, err := o.runtime.SourceFactory(ctx, input, operational)
	if err != nil || compiler == nil {
		return nil, ErrProgramCompositionRequired
	}
	compilerContext := *compiler
	compilerContext.compilationContext = input
	return &compilerContext, nil
}

func (o *Owner) admitEditableSourceContext(ctx context.Context, request Request, current AdmittedGeneration) (Request, error) {
	groups, err := replaceSourceGroup(current.extraGroups, request.sourceReplacement)
	if err != nil {
		return Request{}, err
	}
	if len(groups) == 0 {
		return request, nil
	}
	if request.sourceContext != nil {
		if !request.sourceContext.Valid() {
			return Request{}, ErrInvalidStartup
		}
		return request, nil
	}
	input, _, err := o.captureSourceContext(ctx)
	if err != nil {
		return Request{}, err
	}
	compiler, err := o.sourceCompiler(ctx, input, current.operational)
	if err != nil {
		return Request{}, err
	}
	compilation, err := CompileSourceGroups(groups, compiler.manifest, compiler.catalogue)
	if err != nil {
		return Request{}, err
	}
	if request.sourceCompilation != nil && (request.sourceCompilation.hash != compilation.hash || !request.sourceCompilation.compilationContext.Valid() || request.sourceCompilation.compilationContext.SHA256() != input.SHA256()) {
		return Request{}, ErrWorkspacePreconditionConflict
	}
	request.sourceContext = &input
	return request, nil
}

func (g AdmittedGeneration) CompilationContext() (configcompilation.Context, bool) {
	return g.compilationContext, g.Valid() && g.compilationContext.Valid()
}
