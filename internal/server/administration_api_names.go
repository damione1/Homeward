package server

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
)

// administrationHomeIdentity is injected from the registry owner. It cannot
// synthesize a default home from a host name, account or configuration label.
type administrationHomeIdentity interface {
	HomeName(context.Context) (apitypes.HomeName, error)
}

type administrationNames struct {
	home  administrationHomeIdentity
	store *storage.Store
}

func (n administrationNames) name(ctx context.Context, kind storage.APIResourceType, nativeKey string) (string, error) {
	if nilReactiveInterface(n.home) || n.store == nil {
		return "", storage.ErrAPIResourceUnavailable
	}
	home, err := n.home.HomeName(ctx)
	if err != nil || !home.Valid() {
		return "", storage.ErrAPIResourceUnavailable
	}
	identity, err := n.store.LookupAPIResourceIdentity(ctx, kind, nativeKey)
	if err != nil {
		return "", err
	}
	return home.String() + "/" + kind.Collection() + "/" + identity.ResourceID(), nil
}
func (n administrationNames) resolve(ctx context.Context, kind storage.APIResourceType, name string) (string, error) {
	if nilReactiveInterface(n.home) || n.store == nil {
		return "", storage.ErrAPIResourceUnavailable
	}
	home, err := n.home.HomeName(ctx)
	if err != nil || !home.Valid() {
		return "", storage.ErrAPIResourceUnavailable
	}
	prefix := home.String() + "/" + kind.Collection() + "/"
	id, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return "", storage.ErrAPIResourceNotFound
	}
	if strings.ContainsRune(id, '/') {
		return "", storage.ErrAPIResourceIdentity
	}
	identity, err := n.store.ResolveAPIResourceIdentity(ctx, kind, id)
	if err != nil {
		return "", err
	}
	return identity.NativeKey(), nil
}

func (n administrationNames) parent(ctx context.Context, value string) error {
	parsed, err := apitypes.ParseHomeName(value)
	if err != nil {
		return storage.ErrAPIResourceIdentity
	}
	if nilReactiveInterface(n.home) {
		return storage.ErrAPIResourceIdentity
	}
	home, err := n.home.HomeName(ctx)
	if err != nil || !home.Valid() {
		return storage.ErrAPIResourceIdentity
	}
	if parsed != home {
		return storage.ErrAPIResourceNotFound
	}
	return nil
}

func configurationDocumentEtag(version *api.DocumentVersion) string {
	if version == nil || version.Revision < 1 || len(version.SourceSha256) != 32 {
		return ""
	}
	return `W/"` + base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(version.Revision, 10)+":"+base64.RawURLEncoding.EncodeToString(version.SourceSha256))) + `"`
}

func firstAdministrationNames(values []administrationNames) administrationNames {
	if len(values) != 1 {
		return administrationNames{}
	}
	return values[0]
}

func (n administrationNames) many(ctx context.Context, kind storage.APIResourceType, keys []string) (map[string]string, error) {
	if nilReactiveInterface(n.home) || n.store == nil {
		return nil, storage.ErrAPIResourceUnavailable
	}
	home, err := n.home.HomeName(ctx)
	if err != nil || !home.Valid() {
		return nil, storage.ErrAPIResourceUnavailable
	}
	identities, err := n.store.LookupAPIResourceIdentities(ctx, kind, keys)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(identities))
	for key, id := range identities {
		result[key] = home.String() + "/" + kind.Collection() + "/" + id.ResourceID()
	}
	return result, nil
}

func (n administrationNames) configurationDocument(ctx context.Context, name apitypes.ConfigurationDocumentName) (string, error) {
	if !name.Valid() {
		return "", storage.ErrAPIResourceIdentity
	}
	if nilReactiveInterface(n.home) || n.store == nil {
		return "", storage.ErrAPIResourceUnavailable
	}
	current, err := n.home.HomeName(ctx)
	if err != nil || !current.Valid() {
		return "", storage.ErrAPIResourceUnavailable
	}
	if current != name.Home() {
		return "", storage.ErrAPIResourceNotFound
	}
	identity, err := n.store.ResolveAPIResourceIdentity(ctx, storage.APIConfigurationDocuments, name.ResourceID())
	if err != nil {
		return "", err
	}
	return identity.NativeKey(), nil
}

// Schema versions are immutable natural keys, rather than user display names.
// This lossless encoding keeps that native key and kind distinct without a hash.
func configurationSchemaResourceName(parent string, schema storage.ConfigSchemaMetadata) (string, error) {
	id := "s-" + strconv.Itoa(int(schema.Kind())) + "-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(schema.SchemaVersion())))
	if len(id) > 63 {
		return "", storage.ErrAPIResourceIntegrity
	}
	return parent + "/configurationSchemas/" + id, nil
}
