package configactivation

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func TestNHA197SourceFactoryPreservesFailureCause(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := manifest.Revision()
	text, err := revision.Value()
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := manifest.Digest()
	input, err := configcompilation.NewContext(text, digest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("synthetic native codec failure")
	owner := &Owner{runtime: Runtime{SourceFactory: func(context.Context, configcompilation.Context, config.Config) (*SourceRuntimeCompiler, error) {
		return nil, cause
	}}}
	compiler, err := owner.sourceCompiler(context.Background(), input, config.Config{})
	if compiler != nil || !errors.Is(err, ErrProgramCompositionRequired) || !errors.Is(err, cause) {
		t.Fatalf("factory failure was admitted or hidden: %v", err)
	}
}
