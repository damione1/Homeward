package server

import (
	"testing"

	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA055ConfigDocumentKindPreservesExistingAndSourceKinds(t *testing.T) {
	cases := []struct {
		stored storage.ConfigDocumentKind
		public wire.DocumentKind
	}{
		{storage.ConfigDocumentKindYAMLSource, wire.DocumentKind_DOCUMENT_KIND_YAML_SOURCE},
		{storage.ConfigDocumentKindAuthorizationPolicy, wire.DocumentKind_DOCUMENT_KIND_AUTHORIZATION_POLICY},
		{storage.ConfigDocumentKindMFAPolicy, wire.DocumentKind_DOCUMENT_KIND_MFA_POLICY},
		{storage.ConfigDocumentKindOperationalYAML, wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML},
		{storage.ConfigDocumentKindBootstrapLaunch, wire.DocumentKind_DOCUMENT_KIND_BOOTSTRAP_LAUNCH},
		{storage.ConfigDocumentKindDashboardSource, wire.DocumentKind_DOCUMENT_KIND_DASHBOARD_SOURCE},
		{storage.ConfigDocumentKindAutomationSource, wire.DocumentKind_DOCUMENT_KIND_AUTOMATION_SOURCE},
		{storage.ConfigDocumentKindScriptSource, wire.DocumentKind_DOCUMENT_KIND_SCRIPT_SOURCE},
		{storage.ConfigDocumentKindSceneSource, wire.DocumentKind_DOCUMENT_KIND_SCENE_SOURCE},
		{storage.ConfigDocumentKindOwnershipSource, wire.DocumentKind_DOCUMENT_KIND_OWNERSHIP_SOURCE},
	}
	for i, c := range cases {
		if actual := configDocumentKind(c.stored); actual != c.public || int32(actual) != int32(i+1) {
			t.Fatal("stored document kind lost or renumbered in public response", c.stored)
		}
	}
	for _, unknown := range []storage.ConfigDocumentKind{storage.ConfigDocumentKindInvalid, storage.ConfigDocumentKind(99)} {
		if configDocumentKind(unknown) != wire.DocumentKind_DOCUMENT_KIND_UNSPECIFIED {
			t.Fatal("unknown storage kind was projected as an admitted wire kind")
		}
	}
}
