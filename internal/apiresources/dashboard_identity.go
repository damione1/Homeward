package apiresources

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
)

// DashboardNameForIdentity preserves the native source identity unchanged.
// Lookup compares names within the authorized native collection; there is no
// inverse hash, source-ID normalization, alias inference or second registry.
func DashboardNameForIdentity(home apitypes.HomeName, id authorization.DashboardID) (apitypes.DashboardName, error) {
	raw, err := id.Value()
	if err != nil || !home.Valid() {
		return apitypes.DashboardName{}, apitypes.ErrResourceName
	}
	digest := sha256.Sum256([]byte(raw))
	return apitypes.ParseDashboardName(home.String() + "/dashboards/dashboard-" + hex.EncodeToString(digest[:24]))
}
