package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// APIResourceIdentity is an admitted durable mapping, separate from provider
// identities, labels, source hashes and SQL revisions. Neither key is authority.
type APIResourceIdentity struct{ resourceType, nativeKey, resourceID string }

func (v APIResourceIdentity) NativeKey() string  { return v.nativeKey }
func (v APIResourceIdentity) ResourceID() string { return v.resourceID }

var ErrAPIResourceIdentity = errors.New("invalid API resource identity")
var ErrAPIResourceUnavailable = errors.New("API resource identity unavailable")
var ErrAPIResourceIntegrity = errors.New("invalid stored API resource identity")
var ErrAPIResourceNotFound = errors.New("API resource identity not found")

// APIResourceType is closed; callers cannot create arbitrary namespaces.
type APIResourceType uint8

const (
	APIResourceUnspecified APIResourceType = iota
	APIConfigurationDocuments
	APIDashboards
	APIIntegrationInstances
	APIAccounts
	APISessions
	APIFactors
	APIInvitations
	APIMcpKeys
	APIAutomations
	APIAutomationRuns
	APICompanionDevices
	APINotificationDestinations
	APINotificationDeliveries
)

func (v APIResourceType) Collection() string {
	switch v {
	case APIConfigurationDocuments:
		return "configurationDocuments"
	case APIDashboards:
		return "dashboards"
	case APIIntegrationInstances:
		return "integrationInstances"
	case APIAccounts:
		return "accounts"
	case APISessions:
		return "sessions"
	case APIFactors:
		return "factors"
	case APIInvitations:
		return "invitations"
	case APIMcpKeys:
		return "mcpKeys"
	case APIAutomations:
		return "automations"
	case APIAutomationRuns:
		return "runs"
	case APICompanionDevices:
		return "companionDevices"
	case APINotificationDestinations:
		return "notificationDestinations"
	case APINotificationDeliveries:
		return "notificationDeliveries"
	case APIResourceUnspecified:
		return ""
	default:
		return ""
	}
}
func apiResourceID(value string) bool {
	if len(value) < 1 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, b := range []byte(value) {
		if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-') {
			return false
		}
	}
	return true
}

// EnsureAPIResourceIdentity allocates once per existing domain identity. Caller
// must establish domain existence and authorization first; this is not Create.
func EnsureAPIResourceIdentityTx(ctx context.Context, tx pgx.Tx, kind APIResourceType, nativeKey string) (APIResourceIdentity, error) {
	if ctx == nil || tx == nil || kind.Collection() == "" || len(nativeKey) < 1 || len(nativeKey) > 2048 || strings.ContainsRune(nativeKey, 0) {
		return APIResourceIdentity{}, ErrAPIResourceIdentity
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return APIResourceIdentity{}, ErrAPIResourceUnavailable
	}
	id := "r-" + hex.EncodeToString(entropy[:])
	var stored string
	err := tx.QueryRow(ctx, `INSERT INTO homeward_api_resource_names(resource_type,native_key,resource_id) VALUES ($1,$2,$3) ON CONFLICT(resource_type,native_key) DO UPDATE SET native_key=EXCLUDED.native_key RETURNING resource_id`, kind.Collection(), nativeKey, id).Scan(&stored)
	if err != nil {
		return APIResourceIdentity{}, ErrAPIResourceUnavailable
	}
	if !apiResourceID(stored) {
		return APIResourceIdentity{}, ErrAPIResourceIntegrity
	}
	return APIResourceIdentity{resourceType: kind.Collection(), nativeKey: nativeKey, resourceID: stored}, nil
}

// ResolveAPIResourceIdentity never creates mappings from untrusted names.
func (s *Store) ResolveAPIResourceIdentity(ctx context.Context, kind APIResourceType, resourceID string) (APIResourceIdentity, error) {
	if ctx == nil || s == nil || s.pool == nil || kind.Collection() == "" || !apiResourceID(resourceID) {
		return APIResourceIdentity{}, ErrAPIResourceIdentity
	}
	var key string
	err := s.pool.QueryRow(ctx, `SELECT native_key FROM homeward_api_resource_names WHERE resource_type=$1 AND resource_id=$2`, kind.Collection(), resourceID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIResourceIdentity{}, ErrAPIResourceNotFound
	}
	if err != nil {
		return APIResourceIdentity{}, ErrAPIResourceUnavailable
	}
	return APIResourceIdentity{resourceType: kind.Collection(), nativeKey: key, resourceID: resourceID}, nil
}

// LookupAPIResourceIdentity only reads a mapping allocated by a native creation
// or migration transaction. Reads never create an alternative identity.
func (s *Store) LookupAPIResourceIdentity(ctx context.Context, kind APIResourceType, nativeKey string) (APIResourceIdentity, error) {
	if ctx == nil || s == nil || s.pool == nil || kind.Collection() == "" || nativeKey == "" {
		return APIResourceIdentity{}, ErrAPIResourceIdentity
	}
	var id string
	err := s.pool.QueryRow(ctx, `SELECT resource_id FROM homeward_api_resource_names WHERE resource_type=$1 AND native_key=$2`, kind.Collection(), nativeKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIResourceIdentity{}, ErrAPIResourceNotFound
	}
	if err != nil {
		return APIResourceIdentity{}, ErrAPIResourceUnavailable
	}
	if !apiResourceID(id) {
		return APIResourceIdentity{}, ErrAPIResourceIntegrity
	}
	return APIResourceIdentity{resourceType: kind.Collection(), nativeKey: nativeKey, resourceID: id}, nil
}

// LookupAPIResourceIdentities resolves an admitted metadata page in one bounded
// read. A missing durable mapping is integrity loss, never a fabricated name.
func (s *Store) LookupAPIResourceIdentities(ctx context.Context, kind APIResourceType, keys []string) (map[string]APIResourceIdentity, error) {
	if ctx == nil || s == nil || s.pool == nil || kind.Collection() == "" || len(keys) > 500 {
		return nil, ErrAPIResourceIdentity
	}
	expected := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key == "" || len(key) > 2048 || strings.ContainsRune(key, 0) {
			return nil, ErrAPIResourceIdentity
		}
		expected[key] = struct{}{}
	}
	result := make(map[string]APIResourceIdentity, len(expected))
	if len(keys) == 0 {
		return result, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT native_key,resource_id FROM homeward_api_resource_names WHERE resource_type=$1 AND native_key=ANY($2)`, kind.Collection(), keys)
	if err != nil {
		return nil, ErrAPIResourceUnavailable
	}
	defer rows.Close()
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			return nil, ErrAPIResourceUnavailable
		}
		if _, ok := expected[key]; !ok || !apiResourceID(id) {
			return nil, ErrAPIResourceIntegrity
		}
		result[key] = APIResourceIdentity{resourceType: kind.Collection(), nativeKey: key, resourceID: id}
	}
	if rows.Err() != nil {
		return nil, ErrAPIResourceUnavailable
	}
	if len(result) != len(expected) {
		return nil, ErrAPIResourceIntegrity
	}
	return result, nil
}
