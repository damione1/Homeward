package server

import (
	"context"
	"crypto/hmac"
	"errors"
	"sort"
	"sync"
	"time"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/tasknames"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type dashboardCapture struct {
	handler     *dashboardHandler
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	once        sync.Once
	transferred bool
	principal   authorization.Principal
	current     authorization.Current
	stamp       view.ConfigStamp
	catalogue   sequencer.Cursor
	zone        string
	permits     []*authorization.Permit
	documents   []*api.Dashboard
	digest      string
}

func (c *dashboardCapture) release() {
	c.once.Do(func() {
		c.cancel()
		<-c.done
		for _, permit := range c.permits {
			permit.Release()
		}
		c.permits = nil
	})
}
func (c *dashboardCapture) close() {
	if c != nil && !c.transferred {
		c.release()
	}
}
func (c *dashboardCapture) ready() bool {
	if c == nil || c.ctx.Err() != nil {
		return false
	}
	for _, permit := range c.permits {
		if !permit.Ready() {
			return false
		}
	}
	current, err := c.handler.authority.Current(c.principal)
	if err != nil {
		return false
	}
	_, before, _ := c.current.Binding()
	_, after, err := current.Binding()
	if err != nil || !hmac.Equal(before[:], after[:]) {
		return false
	}
	stamp, err := c.handler.owner.CurrentConfig(c.ctx)
	if err != nil || stamp != c.stamp {
		return false
	}
	catalogue, err := c.handler.sequence.CataloguePosition()
	if err != nil || catalogue != c.catalogue {
		return false
	}
	zone, err := c.handler.displayZone(c.ctx)
	return err == nil && zone == c.zone
}
func (c *dashboardCapture) retain(ctx context.Context) error {
	if !c.ready() {
		return view.ErrViewChanged
	}
	retained, err := retainPublicResponse(ctx, c.ctx, c.ready, c.release)
	if err == nil {
		c.transferred = retained
	}
	return err
}
func (h *dashboardHandler) displayZone(ctx context.Context) (string, error) {
	if nilReactiveInterface(h.profiles) {
		return "", nil
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return "", err
	}
	zone, err := h.profiles.DisplayTimeZone(ctx, actor)
	if err != nil {
		return "", err
	}
	if zone != "" {
		if zone == "Local" || len(zone) > 128 {
			return "", view.ErrInvalidInput
		}
		if _, err = time.LoadLocation(zone); err != nil {
			return "", view.ErrInvalidInput
		}
	}
	return zone, nil
}

// Register native invalidations before materializing any object. All native
// leases and subscriptions survive handler return when transferred to HTTP.
func (h *dashboardHandler) beginCapture(ctx context.Context) (result *dashboardCapture, err error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	c := &dashboardCapture{handler: h, ctx: lifetime, cancel: cancel, done: make(chan struct{}), principal: principal}
	var cleanups []func()
	fail := func() {
		cancel()
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	rights, err := h.authority.Subscribe(lifetime, principal)
	if err != nil {
		fail()
		return nil, err
	}
	cleanups = append(cleanups, rights.Close)
	config, stopConfig, err := h.owner.SubscribeConfigChanges(lifetime)
	if err != nil {
		fail()
		return nil, err
	}
	cleanups = append(cleanups, stopConfig)
	var profile <-chan struct{}
	if !nilReactiveInterface(h.profiles) {
		actor, actorErr := coreRPCActor(ctx)
		if actorErr != nil {
			fail()
			return nil, actorErr
		}
		var stop func()
		profile, stop, err = h.profiles.SubscribeDisplayProfileChanges(lifetime, actor)
		if err != nil || profile == nil || stop == nil {
			fail()
			if err == nil {
				err = view.ErrInvalidInput
			}
			return nil, err
		}
		cleanups = append(cleanups, stop)
	}
	registry, _, err := h.sequence.SubscribeUI()
	if err != nil {
		fail()
		return nil, err
	}
	cleanups = append(cleanups, func() { _ = registry.Close() })
	c.current, err = h.authority.Current(principal)
	if err == nil {
		err = c.current.AdmitRoute(dashboardPermission())
	}
	if err == nil {
		c.stamp, err = h.owner.CurrentConfig(lifetime)
	}
	if err == nil {
		c.catalogue, err = h.sequence.CataloguePosition()
	}
	if err == nil {
		c.zone, err = h.displayZone(lifetime)
	}
	if err != nil {
		fail()
		return nil, err
	}
	registryDone := make(chan struct{})
	go func() {
		defer close(registryDone)
		for {
			if _, err := registry.Next(lifetime); err != nil {
				cancel()
				return
			}
			position, err := h.sequence.CataloguePosition()
			if err != nil || position != c.catalogue {
				cancel()
				return
			}
		}
	}()
	go func() {
		defer close(c.done)
		select {
		case <-lifetime.Done():
		case <-config:
		case <-rights.Changes():
		case <-profile:
		}
		cancel()
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
		<-registryDone
	}()
	return c, nil
}

type dashboardEntityBinding struct {
	name         apitypes.EntityName
	capabilities []apiresources.Capability
}
type dashboardBindings struct {
	aliases     map[string]dashboardEntityBinding
	descriptor  view.EntityViewDescriptor
	watch       *view.EntityWatch
	transferred bool
}

func (b *dashboardBindings) close() {
	if b != nil && b.watch != nil && !b.transferred {
		_ = b.watch.Close()
	}
}
func (b *dashboardBindings) retain(ctx context.Context) error {
	retained, err := retainPublicResponse(ctx, b.watch.Context(), func() bool { return b.watch.Context().Err() == nil }, func() { _ = b.watch.Close() })
	if err == nil {
		b.transferred = retained
	}
	return err
}
func addDashboardBinding(bindings map[string]dashboardEntityBinding, home apitypes.HomeName, record integrations.RegistryRecord, capabilities []apiresources.Capability) error {
	if record.Retired() || len(capabilities) == 0 {
		return nil
	}
	_, entity, err := record.IDs()
	if err != nil {
		return err
	}
	name, err := apitypes.EntityNameForIdentity(home, entity)
	if err != nil {
		return err
	}
	original, current, aliases, err := record.Aliases()
	if err != nil {
		return err
	}
	aliases = append(aliases, original, current)
	for _, alias := range aliases {
		text, err := alias.Value()
		if err != nil {
			return err
		}
		if previous, exists := bindings[text]; exists && previous.name != name {
			return authorization.ErrUnavailable
		}
		bindings[text] = dashboardEntityBinding{name: name, capabilities: capabilities}
	}
	return nil
}
func (h *dashboardHandler) currentBindings(ctx context.Context, home apitypes.HomeName, current authorization.Current) (*dashboardBindings, error) {
	snapshot, err := h.sequence.CaptureBounded(8 << 20)
	if err != nil {
		return nil, err
	}
	result := &dashboardBindings{aliases: make(map[string]dashboardEntityBinding)}
	permission := homeReadPermission()
	for entity, record := range snapshot.Registry {
		if record.Retired() {
			continue
		}
		var readable []*registrywire.CapabilityDescription
		for _, description := range snapshot.Capabilities[entity] {
			if description == nil {
				return nil, view.ErrInvalidInput
			}
			slot, err := authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
			if err != nil {
				return nil, err
			}
			object, err := authorization.EntityObject(entity, slot)
			if err != nil {
				return nil, err
			}
			if err = current.Authorize(permission, object); err == nil {
				readable = append(readable, description)
			} else if !errors.Is(err, authorization.ErrDenied) {
				return nil, err
			}
		}
		if len(readable) == 0 {
			continue
		}
		capabilities, err := apiresources.ProjectCapabilities(home, entity, readable, nil)
		if err != nil {
			return nil, err
		}
		if err = addDashboardBinding(result.aliases, home, record, capabilities); err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (h *dashboardHandler) captureBindings(ctx context.Context, r *api.QueryDashboardRequest) (result *dashboardBindings, err error) {
	descriptor, current, err := h.home.entityView(ctx, r.EntityView)
	if err != nil {
		return nil, err
	}
	binding, err := homePageBinding(current, r.EntityView, descriptor.Filter.Source()+"\x00"+descriptor.SnapshotToken, "WatchEntityView")
	if err != nil {
		return nil, err
	}
	checkpoint, err := h.home.openEntityViewToken(r.ResumeToken, binding)
	if err != nil {
		return nil, err
	}
	if len(checkpoint.Resume) == 0 || checkpoint.Page != "" || len(checkpoint.Names) != 0 || len(checkpoint.Snapshots) != 0 || checkpoint.Offset != 0 {
		return nil, view.ErrPageToken
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	watch, err := h.home.views.WatchEntityViewResume(ctx, actor, descriptor.Name, checkpoint.Resume)
	if err != nil {
		return nil, err
	}
	b := &dashboardBindings{aliases: make(map[string]dashboardEntityBinding), descriptor: descriptor, watch: watch}
	defer func() {
		if err != nil {
			b.close()
		}
	}()
	pageToken := ""
	charged := 0
	for {
		page, readErr := h.home.views.ReadEntityView(watch.Context(), actor, descriptor.Name, pageToken, view.MaxPageSize)
		if readErr != nil {
			return nil, readErr
		}
		for _, item := range page.Items {
			capabilities, present := item.ProjectedCapabilities()
			if !present {
				return nil, view.ErrInvalidInput
			}
			for _, capability := range capabilities {
				size, err := capability.RetainedBytes()
				if err != nil {
					return nil, err
				}
				if size > view.MaxSnapshotBytes-charged {
					return nil, view.ErrResourceExhausted
				}
				charged += size
			}
			if err = addDashboardBinding(b.aliases, descriptor.Name.Home(), item.Registry(), capabilities); err != nil {
				return nil, err
			}
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	final, _, err := h.home.entityView(watch.Context(), r.EntityView)
	if err != nil {
		return nil, err
	}
	if final.SnapshotToken != descriptor.SnapshotToken {
		return nil, view.ErrViewChanged
	}
	return b, nil
}
func (h *dashboardHandler) capture(ctx context.Context, home apitypes.HomeName, wanted *apitypes.DashboardName, bindings *dashboardBindings) (result *dashboardCapture, err error) {
	c, err := h.beginCapture(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			c.close()
		}
	}()
	definitions, stamp, err := h.owner.CurrentDashboards(c.ctx)
	if err != nil {
		return nil, err
	}
	if stamp != c.stamp {
		return nil, view.ErrViewChanged
	}
	if len(definitions) > 128 {
		return nil, view.ErrResourceExhausted
	}
	versions, err := h.owner.DashboardSourceVersions(c.ctx, stamp)
	if err != nil {
		return nil, err
	}
	var authorized []imported.Definition
	names := make(map[authorization.DashboardID]apitypes.DashboardName)
	seen := make(map[string]bool)
	for _, definition := range definitions {
		owner, present := definition.Owner()
		if !present {
			continue
		}
		object, err := authorization.DashboardObject(definition.Source().ID(), owner)
		if err != nil {
			return nil, err
		}
		permit, err := h.authority.Permit(c.ctx, c.principal, dashboardPermission(), object)
		if errors.Is(err, authorization.ErrDenied) {
			continue
		}
		if err != nil {
			return nil, err
		}
		c.permits = append(c.permits, permit)
		name, err := apiresources.DashboardNameForIdentity(home, definition.Source().ID())
		if err != nil {
			return nil, err
		}
		if seen[name.String()] {
			return nil, view.ErrInvalidInput
		}
		seen[name.String()] = true
		names[definition.Source().ID()] = name
		authorized = append(authorized, definition)
	}
	index, err := imported.NewNavigationIndex(authorized)
	if err != nil {
		return nil, err
	}
	if bindings == nil {
		bindings, err = h.currentBindings(c.ctx, home, c.current)
		if err != nil {
			return nil, err
		}
	}
	options := imported.APIProjection{
		Home: home,
		Entity: func(alias domain.HAAlias) (apitypes.EntityName, bool) {
			text, _ := alias.Value()
			value, present := bindings.aliases[text]
			return value.name, present
		},
		Capabilities: func(alias domain.HAAlias) []apiresources.Capability {
			text, _ := alias.Value()
			return bindings.aliases[text].capabilities
		},
		Dashboard: func(id authorization.DashboardID) (apitypes.DashboardName, bool) {
			name, present := names[id]
			return name, present
		},
		Calendar: func(entity domain.EntityID) (calendarnames.CalendarName, bool) {
			permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_CALENDARS, auth.ScopeOperation_SCOPE_OPERATION_READ)
			object, err := authorization.CalendarObject(entity)
			if err != nil || c.current.Authorize(permission, object) != nil {
				return calendarnames.CalendarName{}, false
			}
			name, err := calendarnames.Calendar(home, entity)
			return name, err == nil
		},
		TaskList: func(id domain.ListID) (tasknames.ListName, bool) {
			permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_LISTS, auth.ScopeOperation_SCOPE_OPERATION_READ)
			object, err := authorization.ListObject(id)
			if err != nil || c.current.Authorize(permission, object) != nil {
				return tasknames.ListName{}, false
			}
			name, err := tasknames.List(home, id)
			return name, err == nil
		},
		Navigation: &index,
	}
	charged := 0
	for _, definition := range authorized {
		name := names[definition.Source().ID()]
		if wanted != nil && name != *wanted {
			continue
		}
		version, present := versions[definition.Source().ID()]
		if !present {
			return nil, authorization.ErrUnavailable
		}
		layout, err := imported.ProjectDashboardDefinition(definition, options)
		if err != nil {
			return nil, err
		}
		title := "Dashboard"
		if views := definition.Views(); len(views) > 0 && views[0].Title() != "" {
			title = views[0].Title()
		}
		document := &api.Dashboard{Name: name.String(), DisplayName: title, UpdateTime: timestamppb.New(version.UpdatedAt), Layout: layout}
		if c.zone != "" {
			document.DisplayProfile = &api.DashboardDisplayProfile{TimeZone: c.zone}
		}
		raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(document)
		if err != nil {
			return nil, err
		}
		document.Etag = "W/\"" + h.opaque([]byte("DashboardRepresentation/v2"), raw) + "\""
		size := proto.Size(document)
		if size > maxDashboardResourceBytes || size > maxDashboardCatalogueBytes-charged {
			return nil, view.ErrResourceExhausted
		}
		charged += size
		c.documents = append(c.documents, document)
	}
	sort.Slice(c.documents, func(i, j int) bool { return c.documents[i].Name < c.documents[j].Name })
	parts := [][]byte{[]byte("DashboardCatalogue/v2"), []byte(home.String())}
	for _, document := range c.documents {
		parts = append(parts, []byte(document.Name), []byte(document.Etag))
	}
	c.digest = h.opaque(parts...)
	if !c.ready() {
		return nil, view.ErrViewChanged
	}
	return c, nil
}
