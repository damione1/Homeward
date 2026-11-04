package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/view"
)

// Only source setup remains here. All public Dashboard requests are served by
// the production service composed by Run, from the native published generation.
type nhf005Dashboard struct {
	file        string
	compilation imported.CompilationProfile
	catalogue   *nhf007Catalogue
	authority   *authorization.Authority
	activation  view.Configuration
	mu          sync.Mutex
	owner       contracts.AccountID
	owners      map[contracts.AccountID]contracts.AccountID
}

const nhf005DashboardID = "synthetic-dashboard"

func (h *nhf005Dashboard) install(owner string, bindings map[string]domain.EntityID) error {
	id, err := contracts.ParseAccountID(owner)
	if err != nil {
		return err
	}
	// Entity references are resolved by the actual registry at publication, never
	// by this historical fixture map or by a public name-to-native-ID adapter.
	for alias, entity := range bindings {
		if alias == "" {
			return errors.New("empty fixture alias")
		}
		if _, err := entity.Value(); err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.owner = id
	h.mu.Unlock()
	return nil
}
func nhf005DashboardFixture() (*nhf005Dashboard, error) {
	file, catalogue := os.Getenv("HOMEWARD_NHF005_DASHBOARD"), os.Getenv("HOMEWARD_NHF007_CATALOGUE")
	if catalogue != "" {
		if file != "" || !filepath.IsAbs(catalogue) {
			return nil, errors.New("synthetic dashboard catalogue must be absolute and exclusive")
		}
		return &nhf005Dashboard{catalogue: &nhf007Catalogue{file: catalogue}}, nil
	}
	if file == "" {
		return nil, nil
	}
	if !filepath.IsAbs(file) {
		return nil, errors.New("synthetic dashboard source must be absolute")
	}
	return &nhf005Dashboard{file: file}, nil
}
func (h *nhf005Dashboard) start(t *testing.T, ctx context.Context, client connect.HTTPClient, origin, token string) {
	t.Helper()
	if h == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	publisher := nhf005Publisher(t, ctx, client, origin, token)
	h.mu.Lock()
	owners := []contracts.AccountID{h.owner}
	if len(h.owners) > 0 {
		owners = nil
		for _, owner := range h.owners {
			owners = append(owners, owner)
		}
	}
	h.mu.Unlock()
	sort.Slice(owners, func(i, j int) bool { return nha203AccountText(t, owners[i]) < nha203AccountText(t, owners[j]) })
	previous := map[string]bool{}
	publish := func() error {
		entries := []nhf007Entry{{ID: nhf005DashboardID, URLPath: "synthetic", File: h.file}}
		root := ""
		if h.catalogue != nil {
			catalogue, err := h.catalogue.load()
			if err != nil {
				return err
			}
			entries = catalogue.Dashboards
			root = filepath.Dir(h.catalogue.file)
		}
		current := map[string]bool{}
		for index, owner := range owners {
			for _, entry := range entries {
				identity, urlPath := entry.ID, entry.URLPath
				routePrefix := ""
				if len(owners) > 1 {
					identity = fmt.Sprintf("%s-owner-%d", identity, index+1)
					routePrefix = fmt.Sprintf("owner-%d-", index+1)
					urlPath = routePrefix + urlPath
				}
				current[identity] = true
				file := entry.File
				if root != "" {
					file = filepath.Join(root, file)
				}
				if err := publisher.publish(ctx, identity, file, nha203AccountText(t, owner), urlPath, routePrefix); err != nil {
					return err
				}
			}
		}
		for identity := range previous {
			if !current[identity] {
				return errors.New("catalogue removal requires native source removal; the harness cannot synthesize it")
			}
		}
		previous = current
		return nil
	}
	if err := publish(); err != nil {
		t.Fatal("native dashboard source publication", err)
	}
	stopped, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { cancel(); close(stopped); <-done })
	go func() {
		defer close(done)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopped:
				return
			case <-tick.C:
				if err := publish(); err != nil {
					if ctx.Err() == nil {
						t.Errorf("native dashboard source publication: %v", err)
					}
					return
				}
			}
		}
	}()
}

func (h *nhf005Dashboard) ready(t *testing.T, ctx context.Context, client connect.HTTPClient, origin, token string) {
	t.Helper()
	parent := nha203PublicHomeName(t, ctx, client, origin, token)
	service := apiv2connect.NewDashboardServiceClient(client, origin)
	page, err := service.ListDashboards(ctx, nha056SessionRequest(&api.ListDashboardsRequest{Parent: parent}, token))
	if err != nil || len(page.Msg.Dashboards) == 0 {
		t.Fatal("native dashboard catalogue readiness", err)
	}
	resource, err := service.GetDashboard(ctx, nha056SessionRequest(&api.GetDashboardRequest{Name: page.Msg.Dashboards[0].Name}, token))
	if err != nil || resource.Msg.Layout == nil || resource.Msg.Etag == "" {
		t.Fatal("native dashboard resource readiness", err)
	}
}
