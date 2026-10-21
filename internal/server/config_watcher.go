package server

import (
	"context"

	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/internal/configdocuments"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type configurationWatcherRuntime struct{ watcher *configdocuments.Watcher }

func (r *configurationWatcherRuntime) BeginClose() {
	if r != nil {
		r.watcher.BeginClose()
	}
}
func (r *configurationWatcherRuntime) Done() <-chan struct{} {
	if r != nil {
		return r.watcher.Done()
	}
	var watcher *configdocuments.Watcher
	return watcher.Done()
}
func (r *configurationWatcherRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	return r.watcher.Close(ctx)
}

func startConfigurationWatcher(ctx context.Context, service *configdocuments.Service, clock configdocuments.WatcherClock) (*configurationWatcherRuntime, error) {
	if service == nil {
		return nil, nil
	}
	if clock == nil {
		clock = configdocuments.RealWatcherClock{}
	}
	watcher, err := configdocuments.NewWatcherWithClock(service, clock)
	if err != nil {
		return nil, err
	}
	if err := watcher.Start(ctx); err != nil {
		watcher.BeginClose()
		return nil, err
	}
	return &configurationWatcherRuntime{watcher: watcher}, nil
}

func configWorkspaceDiagnostic(diagnostic configdocuments.WorkspaceDiagnostic) *wire.WorkspaceDiagnostic {
	var code wire.DiagnosticCode
	switch diagnostic.Kind() {
	case configdocuments.CandidateRejected:
		code = wire.DiagnosticCode_DIAGNOSTIC_CODE_CANDIDATE_REJECTED
	case configdocuments.WorkspaceChanged:
		code = wire.DiagnosticCode_DIAGNOSTIC_CODE_WORKSPACE_CHANGED
	case configdocuments.WorkspaceReconciling:
		code = wire.DiagnosticCode_DIAGNOSTIC_CODE_RECONCILIATION_REQUIRED
	case configdocuments.ActivationUnavailable:
		code = wire.DiagnosticCode_DIAGNOSTIC_CODE_ACTIVATION_UNAVAILABLE
	case configdocuments.DiagnosticUnknown:
		return nil
	default:
		return nil
	}
	hash := diagnostic.SourceSHA256()
	return &wire.WorkspaceDiagnostic{Document: &wire.DocumentId{Value: diagnostic.Document().String()}, ObservedSourceSha256: append([]byte(nil), hash[:]...), ObservedAt: timestamppb.New(diagnostic.UpdatedAt()), Code: code}
}
