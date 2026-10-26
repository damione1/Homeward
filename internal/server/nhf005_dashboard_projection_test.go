package server

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/view"
)

// projectDashboardOverview translates configured views and exact card bindings.
// The importer owns Lovelace semantics; there are no installation names or
// alias-fragment rules here. Full card/layout rendering remains negotiated.
func projectDashboardOverview(definition imported.Definition, stamp view.ConfigStamp, authorizationRevision uint64,
	mode dashboardv1.ServingMode, resolve func(domain.HAAlias) (domain.EntityID, bool)) (*dashboardv1.NativeOverview, error) {
	revision, hash, err := stamp.Components()
	if err != nil || authorizationRevision == 0 || resolve == nil || (definition.Profile() != imported.Profile && definition.Profile() != imported.RecipesProfile) ||
		(mode != dashboardv1.ServingMode_SERVING_MODE_CONFIGURED && mode != dashboardv1.ServingMode_SERVING_MODE_SYNTHETIC) {
		return nil, ErrInvalidPublicPlan
	}
	result := &dashboardv1.NativeOverview{Version: 1, ConfigRevision: revision,
		ConfigSha256: append([]byte(nil), hash[:]...), Mode: mode, AuthorizationRevision: authorizationRevision}
	views := definition.Views()
	if len(views) > 128 {
		return nil, ErrInvalidPublicPlan
	}
	result.Title = "Dashboard"
	if len(views) != 0 && views[0].Title() != "" {
		result.Title = views[0].Title()
	}
	var collect func([]imported.Card, func(imported.Card, string))
	collect = func(cards []imported.Card, visit func(imported.Card, string)) {
		for _, card := range cards {
			if card.Status() != imported.StatusSupported || len(card.Visibility()) != 0 {
				continue
			}
			if alias, present := card.Entity(); present {
				if id, known := resolve(alias); known {
					if text, err := id.Value(); err == nil {
						visit(card, text)
					}
				}
			}
			collect(card.Children(), visit)
		}
	}
	for index, configured := range views {
		if configured.Status() != imported.StatusSupported || len(configured.Visibility()) != 0 {
			continue
		}
		route := configured.NativeRoute()
		if route == "" || configured.Title() == "" {
			return nil, ErrInvalidPublicPlan
		}
		room := &dashboardv1.OverviewRoom{Id: route, Title: configured.Title()}
		seen := map[string]bool{}
		for _, section := range configured.Sections() {
			if section.Status() != imported.StatusSupported || len(section.Visibility()) != 0 {
				continue
			}
			collect(section.Cards(), func(card imported.Card, entity string) {
				if !seen[entity] {
					room.EntityIds = append(room.EntityIds, entity)
					seen[entity] = true
				}
				if index == 0 && len(result.Observations) < 64 {
					title, present := card.Name()
					if !present {
						if alias, present := card.Entity(); present {
							title, _ = alias.Value()
						}
					}
					result.Observations = append(result.Observations, &dashboardv1.OverviewObservation{
						Id: route + ":" + card.Location().Path(), Title: title, EntityId: entity,
						Icon: dashboardv1.NativeIcon_NATIVE_ICON_INFORMATION})
				}
			})
		}
		if index != 0 {
			result.Rooms = append(result.Rooms, room)
		}
	}
	if len(result.Observations) > 64 {
		return nil, fmt.Errorf("dashboard overview exceeds 64 configured observations")
	}
	return result, nil
}

func TestNHF005DashboardProjectionUsesExternalSourceAndExactBindings(t *testing.T) {
	id, err := authorization.ParseDashboardID("synthetic-dashboard")
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := view.NewConfigStamp(1, sha256.Sum256([]byte("synthetic config")))
	if err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"home.yaml", "alternate.yaml"} {
		source, err := imported.NewSource(id, "synthetic", filename)
		if err != nil {
			t.Fatal(err)
		}
		root, err := filepath.Abs("../../qualification/frontend/dashboards")
		if err != nil {
			t.Fatal(err)
		}
		result, err := imported.Import(root, source)
		if err != nil || result.Status() != imported.StatusSupported {
			t.Fatalf("source %s: %v %v %v", filename, err, result.Status(), result.Diagnostics())
		}
		definition, present := result.Definition()
		if !present {
			t.Fatal("no definition")
		}
		projected, err := projectDashboardOverview(definition, stamp, 1, dashboardv1.ServingMode_SERVING_MODE_SYNTHETIC, func(alias domain.HAAlias) (domain.EntityID, bool) {
			value, err := alias.Value()
			if err != nil {
				return domain.EntityID{}, false
			}
			entity, err := domain.ParseEntityID("synthetic:" + value)
			return entity, err == nil
		})
		if err != nil || len(projected.Rooms) == 0 || len(projected.Observations) == 0 {
			t.Fatalf("source %s: %v", filename, err)
		}
		for _, observation := range projected.Observations {
			if observation.EntityId == "" {
				t.Fatal("missing exact identity")
			}
		}
		if projected.Title != definition.Views()[0].Title() {
			t.Fatal("title did not come from source")
		}
	}
}
