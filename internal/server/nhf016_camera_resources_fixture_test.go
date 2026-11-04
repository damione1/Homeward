package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
)

// The synthetic source owns only bytes/events. The ordinary SQL session service
// remains the single lease authority, sharing the native Store and Authority.
type nhf016ResourceRig struct {
	*nha162RPCRig
	api          apiv2connect.MediaServiceClient
	handler      *mediaResourceHandler
	owners       map[string]*nha202MediaOwner
	home         apitypes.HomeName
	clock        *nhf016ResourceClock
	cameraServer *httptest.Server
}

func nhf016NewResourceRig(t *testing.T, lifetime time.Duration) *nhf016ResourceRig {
	t.Helper()
	base := nha162NewRPCRigWithLifetime(t, lifetime)
	nha162SetPolicy(t, base, base.admin, nhf016AllCameras)
	root := os.Getenv("HOMEWARD_NHF016_SOURCES")
	if root == "" {
		root = "../../qualification/frontend/cameras"
	}
	sources, err := nhf016ReadCameraSources(root)
	if err != nil {
		t.Fatal(err)
	}
	home, err := base.lists.registry.HomeName(base.ctx)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nhf016ResourceClock{}
	sessions, err := mediasessions.NewService(base.store, base.coordinator.authority, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sessions.Close)
	owners := make(map[string]*nha202MediaOwner)
	var registrations []mediaSourceRegistration
	for _, source := range sources {
		owner := nha202NewMediaOwner(t, source.Entity, mediasessions.Camera, color.NRGBA{R: source.RGB[0], G: source.RGB[1], B: source.RGB[2], A: 255})
		owner.label = source.Title
		owner.events = nil
		for index := uint32(0); index < source.Events; index++ {
			uid, err := mediasessions.ParseEventID(fmt.Sprintf("event-%05d", index))
			if err != nil {
				t.Fatal(err)
			}
			event, err := mediacontent.NewEvent(uid, "Événement synthétique", source.Source, time.Now().Add(-time.Duration(index+1)*time.Second), index%3 != 2)
			if err != nil {
				t.Fatal(err)
			}
			owner.events = append(owner.events, event)
		}
		name, err := mediaSourceName(home, owner.binding.Entity(), owner.binding.Slot())
		if err != nil {
			t.Fatal(err)
		}
		owners[name.String()] = owner
		registrations = append(registrations, mediaSourceRegistration{owner.binding.Entity(), owner.binding.Slot(), owner})
	}
	handler, err := newMediaResourceHandler(sessions, base.coordinator.authority, base.lists.registry, registrations)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: base.store})
	if err != nil {
		t.Fatal(err)
	}
	if err = registerMediaResourceRoutes(plan, handler); err != nil {
		t.Fatal(err)
	}
	if err = registerHomeDiscoveryRoutes(plan, base.lists.registry, base.coordinator.authority); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, base.store, base.coordinator, administrationNames{home: base.lists.registry, store: base.store})
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	data := shortPrivateDataDir(t)
	nha181CertificateFixture(t, data)
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(func() { server.CloseClientConnections(); sessions.Close(); server.Close() })
	client := server.Client()
	client.Timeout = 20 * time.Second
	return &nhf016ResourceRig{base, apiv2connect.NewMediaServiceClient(client, server.URL), handler, owners, home, clock, server}
}
func (r *nhf016ResourceRig) catalog(t *testing.T) []*api.MediaSource {
	t.Helper()
	v, e := r.api.ListMediaSources(r.ctx, nha162Bearer(&api.ListMediaSourcesRequest{Parent: r.home.String()}, r.adminToken))
	if e != nil {
		t.Fatal(e)
	}
	return v.Msg.MediaSources
}
func (r *nhf016ResourceRig) open(t *testing.T, name string) *api.MediaSession {
	t.Helper()
	v, e := r.api.CreateMediaSession(r.ctx, nha162Bearer(&api.CreateMediaSessionRequest{Parent: r.home.String(), MediaSession: &api.MediaSession{MediaSource: name, ContentKind: api.MediaContentKind_MEDIA_CONTENT_KIND_CAMERA}}, r.adminToken))
	if e != nil {
		t.Fatal(e)
	}
	return v.Msg
}
func (r *nhf016ResourceRig) ownerByEntity(t *testing.T, raw string) *nha202MediaOwner {
	t.Helper()
	entity, err := domain.ParseEntityID(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range r.owners {
		if owner.binding.Entity() == entity {
			return owner
		}
	}
	t.Fatal("unknown synthetic camera")
	return nil
}
func (r *nhf016ResourceRig) closeSession(ctx context.Context, name string, token string) error {
	_, err := r.api.DeleteMediaSession(ctx, nha162Bearer(&api.DeleteMediaSessionRequest{Name: name}, token))
	return err
}

// Wall time keeps advancing during a long fixture; only explicit expiration
// controls advance its offset. No public request can alter this clock.
type nhf016ResourceClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *nhf016ResourceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().UTC().Add(c.offset)
}
func (c *nhf016ResourceClock) advance(by time.Duration) { c.mu.Lock(); c.offset += by; c.mu.Unlock() }

// Trusted fixture setup uses the sole ordinary SQL owner to shorten a lifetime;
// the public Create contract never accepts a caller-selected lifetime.
func (r *nhf016ResourceRig) openLifetime(t *testing.T, source string, lifetime time.Duration) *api.MediaSession {
	t.Helper()
	owner := r.owners[source]
	if owner == nil {
		t.Fatal("unknown source")
	}
	request := httptest.NewRequest(http.MethodPost, apiv2connect.MediaServiceCreateMediaSessionProcedure, nil)
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Authorization", "Bearer "+r.adminToken)
	actor, ctx, release, err := (&sessionAuth{store: r.store, coordinator: r.coordinator}).AuthenticateWithLease(r.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx = context.WithValue(ctx, actorContextKey{}, actor)
	admission, err := r.handler.sessions.AdmitSource(ctx, owner.binding.Entity(), owner.binding.Slot())
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Release()
	input, err := mediasessions.NewInput(owner.binding, nil, lifetime, owner.revision)
	if err != nil {
		t.Fatal(err)
	}
	record, err := r.handler.sessions.Create(ctx, admission, input)
	if err != nil {
		t.Fatal(err)
	}
	value, err := mediaSessionWireRevision(r.home, record, owner.revision)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
