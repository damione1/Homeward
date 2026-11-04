package server

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/configdocuments"
)

// The caller's opaque instance etag is compared against the captured resource
// before this conversion. No public workspace hash or decoded etag becomes an
// execution capability. The native gate still rechecks all of these values.
func integrationExpected(snapshot configdocuments.Snapshot, requireClean bool) (configdocuments.ExpectedDocument, error) {
	workspace, present := snapshot.Workspace().SHA256()
	if !present || snapshot.Workspace().ReconciliationPending() || snapshot.Dirty() == configdocuments.DirtyUnavailable || requireClean && snapshot.Dirty() != configdocuments.Clean {
		return configdocuments.ExpectedDocument{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED)
	}
	document := snapshot.Document()
	return configdocuments.NewExpectedDocument(document.Ref().Document(), document.Ref().Revision(), document.SourceSHA256(), workspace)
}

func integrationSnapshotVersion(snapshot configdocuments.Snapshot) integrationDefinitionVersion {
	workspace, observed := snapshot.Workspace().SHA256()
	return integrationDefinitionVersion{ref: snapshot.Document().Ref(), hash: snapshot.Document().SourceSHA256(), generation: snapshot.Generation().Number(), manifest: snapshot.ManifestSHA256(), workspace: workspace, observed: observed, pending: snapshot.Workspace().ReconciliationPending()}
}
