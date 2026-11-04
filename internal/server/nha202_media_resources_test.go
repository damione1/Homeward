package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/mediasessions"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type nha202WallClock struct{}

func (nha202WallClock) Now() time.Time { return time.Now() }

type nha202MediaRig struct {
	*nha162RPCRig
	api     apiv2connect.MediaServiceClient
	handler *mediaResourceHandler
	owners  []*nha202MediaOwner
	home    apitypes.HomeName
}

func nha202NewMediaAPI(t *testing.T) *nha202MediaRig {
	t.Helper()
	r := nha162NewRPCRig(t)
	nha162SetPolicy(t, r, r.admin, nhf016AllCameras)
	home, err := r.lists.registry.HomeName(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	leaseOwner, err := mediasessions.NewService(r.store, r.coordinator.authority, nha202WallClock{})
	if err != nil {
		t.Fatal(err)
	}
	owners := []*nha202MediaOwner{nha202NewMediaOwner(t, "driveway", mediasessions.Camera, color.NRGBA{36, 125, 190, 255}), nha202NewMediaOwner(t, "_driveway", mediasessions.Camera, color.NRGBA{185, 76, 42, 255}), nha202NewMediaOwner(t, "synthetic-player", mediasessions.Artwork, color.NRGBA{145, 22, 100, 255})}
	var registrations []mediaSourceRegistration
	for _, owner := range owners {
		registrations = append(registrations, mediaSourceRegistration{entity: owner.binding.Entity(), slot: owner.binding.Slot(), owner: owner})
	}
	handler, err := newMediaResourceHandler(leaseOwner, r.coordinator.authority, r.lists.registry, registrations)
	if err != nil {
		leaseOwner.Close()
		t.Fatal(err)
	}
	plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: r.store})
	if err != nil {
		t.Fatal(err)
	}
	if err := registerMediaResourceRoutes(plan, handler); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, r.store, r.coordinator, administrationNames{home: r.lists.registry, store: r.store})
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(func() { server.CloseClientConnections(); leaseOwner.Close(); server.Close() })
	return &nha202MediaRig{r, apiv2connect.NewMediaServiceClient(server.Client(), server.URL), handler, owners, home}
}
func (r *nha202MediaRig) source(t *testing.T, index int) string {
	t.Helper()
	binding := r.owners[index].binding
	name, err := mediaSourceName(r.home, binding.Entity(), binding.Slot())
	if err != nil {
		t.Fatal(err)
	}
	return name.String()
}
func (r *nha202MediaRig) open(t *testing.T, index int) *api.MediaSession {
	t.Helper()
	kind := api.MediaContentKind_MEDIA_CONTENT_KIND_CAMERA
	if index == 2 {
		kind = api.MediaContentKind_MEDIA_CONTENT_KIND_ARTWORK
	}
	session := &api.MediaSession{MediaSource: r.source(t, index), ContentKind: kind}
	if index == 2 {
		session.ArtworkReference = r.owners[index].reference(r.binding(t, r.adminToken))
	}
	response, err := r.api.CreateMediaSession(r.ctx, nha162Bearer(&api.CreateMediaSessionRequest{Parent: r.home.String(), MediaSession: session}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg
}
func (r *nha202MediaRig) binding(t *testing.T, token string) [32]byte {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, apiv2connect.MediaServiceGetMediaSourceProcedure, nil)
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Authorization", "Bearer "+token)
	_, ctx, release, err := (&sessionAuth{store: r.store, coordinator: r.coordinator}).AuthenticateWithLease(r.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current, err := r.coordinator.authority.Current(principal)
	if err != nil {
		t.Fatal(err)
	}
	_, binding, err := current.Binding()
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
func nha202SnapshotRequest(name string) *api.ReadMediaSessionRequest {
	return &api.ReadMediaSessionRequest{Name: name, Target: &api.MediaImageTarget{Value: &api.MediaImageTarget_Snapshot{Snapshot: &emptypb.Empty{}}}}
}
func TestNHA202MediaSessionTLSExactSourcesResultDedupAndCredentialBinding(t *testing.T) {
	r := nha202NewMediaAPI(t)
	ctx, access := r.ctx, r.adminToken
	page, err := r.api.ListMediaSources(ctx, nha162Bearer(&api.ListMediaSourcesRequest{Parent: r.home.String(), PageSize: 1}, access))
	if err != nil || len(page.Msg.MediaSources) != 1 || page.Msg.NextPageToken == "" {
		t.Fatal("source page", err)
	}
	final, err := r.api.ListMediaSources(ctx, nha162Bearer(&api.ListMediaSourcesRequest{Parent: r.home.String(), PageSize: 256, PageToken: page.Msg.NextPageToken}, access))
	if err != nil || len(final.Msg.MediaSources) != 2 {
		t.Fatal("source page size change", err)
	}
	for _, source := range append(page.Msg.MediaSources, final.Msg.MediaSources...) {
		if source.DataProvenance != api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC || source.Quality == nil {
			t.Fatal("provenance and quality lost")
		}
	}
	request := &api.CreateMediaSessionRequest{Parent: r.home.String(), MediaSession: &api.MediaSession{MediaSource: r.source(t, 0), ContentKind: api.MediaContentKind_MEDIA_CONTENT_KIND_CAMERA}, RequestId: "452f82d2-8da2-4d1f-bda6-07a0d487a7b4"}
	created, err := r.api.CreateMediaSession(ctx, nha162Bearer(request, access))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := r.api.CreateMediaSession(ctx, nha162Bearer(request, access))
	if err != nil || !proto.Equal(created.Msg, replay.Msg) {
		t.Fatal("exact session replay", err)
	}
	changed := proto.Clone(request).(*api.CreateMediaSessionRequest)
	changed.MediaSession.MediaSource = r.source(t, 1)
	_, err = r.api.CreateMediaSession(ctx, nha162Bearer(changed, access))
	nha162Code(t, err, connect.CodeAlreadyExists)
	another := r.open(t, 1)
	left, err := r.api.ReadMediaSession(ctx, nha162Bearer(nha202SnapshotRequest(created.Msg.Name), access))
	if err != nil {
		t.Fatal(err)
	}
	right, err := r.api.ReadMediaSession(ctx, nha162Bearer(nha202SnapshotRequest(another.Name), access))
	if err != nil {
		t.Fatal(err)
	}
	if left.Msg.Image.MediaSession != created.Msg.Name || right.Msg.Image.MediaSession != another.Name || sha256.Sum256(left.Msg.Image.Png) == sha256.Sum256(right.Msg.Image.Png) {
		t.Fatal("distinct sources exchanged pixels")
	}
	otherToken := r.login(t, "NHA162 Admin", "NHA162 synthetic admin password canary")
	_, err = r.api.GetMediaSession(ctx, nha162Bearer(&api.GetMediaSessionRequest{Name: created.Msg.Name}, otherToken))
	nha162Code(t, err, connect.CodeNotFound)
	deletion := &api.DeleteMediaSessionRequest{Name: created.Msg.Name, RequestId: "c8e868d5-424e-4622-90db-03e8c8c40749"}
	if _, err = r.api.DeleteMediaSession(ctx, nha162Bearer(deletion, access)); err != nil {
		t.Fatal(err)
	}
	if _, err = r.api.DeleteMediaSession(ctx, nha162Bearer(deletion, access)); err != nil {
		t.Fatal("exact delete result replay", err)
	}
	_, err = r.api.ReadMediaSession(ctx, nha162Bearer(nha202SnapshotRequest(created.Msg.Name), access))
	nha162Code(t, err, connect.CodeNotFound)
}
func TestNHA202MediaProjectionEventsAndArtworkRefScopeTLS(t *testing.T) {
	r := nha202NewMediaAPI(t)
	ctx, access := r.ctx, r.adminToken
	session := r.open(t, 0)
	first, err := r.api.QueryMediaSessionEvents(ctx, nha162Bearer(&api.QueryMediaSessionEventsRequest{Name: session.Name, PageSize: 1}, access))
	if err != nil || len(first.Msg.MediaEvents) != 1 || first.Msg.TotalSize != 3 || first.Msg.EventsEtag == "" {
		t.Fatal("bounded native events", err)
	}
	second, err := r.api.QueryMediaSessionEvents(ctx, nha162Bearer(&api.QueryMediaSessionEventsRequest{Name: session.Name, PageSize: 256, PageToken: first.Msg.NextPageToken}, access))
	if err != nil || len(second.Msg.MediaEvents) != 2 {
		t.Fatal("event page", err)
	}
	r.owners[0].invalidate()
	_, err = r.api.QueryMediaSessionEvents(ctx, nha162Bearer(&api.QueryMediaSessionEventsRequest{Name: session.Name, PageToken: first.Msg.NextPageToken}, access))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	r.owners[0].mu.Lock()
	r.owners[0].wrongImage = true
	r.owners[0].mu.Unlock()
	_, err = r.api.ReadMediaSession(ctx, nha162Bearer(nha202SnapshotRequest(session.Name), access))
	nha162Code(t, err, connect.CodeDataLoss)
	artwork := r.open(t, 2)
	_, err = r.api.ReadMediaSession(ctx, nha162Bearer(nha202SnapshotRequest(artwork.Name), access))
	if err != nil {
		t.Fatal("artwork image", err)
	}
	foreign := r.login(t, "NHA162 Admin", "NHA162 synthetic admin password canary")
	ref := r.owners[2].reference(r.binding(t, access))
	_, err = r.api.CreateMediaSession(ctx, nha162Bearer(&api.CreateMediaSessionRequest{Parent: r.home.String(), MediaSession: &api.MediaSession{MediaSource: r.source(t, 2), ContentKind: api.MediaContentKind_MEDIA_CONTENT_KIND_ARTWORK, ArtworkReference: ref}}, foreign))
	nha162Code(t, err, connect.CodePermissionDenied)
	request := &api.CreateMediaSessionRequest{Parent: r.home.String(), MediaSession: &api.MediaSession{MediaSource: r.source(t, 2), ContentKind: api.MediaContentKind_MEDIA_CONTENT_KIND_ARTWORK, ArtworkReference: ref}, RequestId: "731ffca7-9cc8-4265-a57f-2e273a75e6ac"}
	initial, err := r.api.CreateMediaSession(ctx, nha162Bearer(request, access))
	if err != nil {
		t.Fatal(err)
	}
	r.owners[2].mu.Lock()
	r.owners[2].artworkExpiry = time.Now().Add(-time.Second)
	r.owners[2].mu.Unlock()
	replay, err := r.api.CreateMediaSession(ctx, nha162Bearer(request, access))
	if err != nil || !proto.Equal(initial.Msg, replay.Msg) {
		t.Fatal("expired source reference replay changed original result", err)
	}
	_, err = r.api.ReadMediaSession(ctx, nha162Bearer(nha202SnapshotRequest(initial.Msg.Name), access))
	nha162Code(t, err, connect.CodeNotFound)
}
func TestNHA202MediaDeleteCancelsAndJoinsBlockedNativeReadTLS(t *testing.T) {
	r := nha202NewMediaAPI(t)
	session := r.open(t, 0)
	gate := make(chan struct{})
	r.owners[0].mu.Lock()
	r.owners[0].imageGate = gate
	r.owners[0].mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, err := r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(session.Name), r.adminToken))
		result <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for r.owners[0].activeImages.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("read did not enter native source")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := r.api.DeleteMediaSession(r.ctx, nha162Bearer(&api.DeleteMediaSessionRequest{Name: session.Name}, r.adminToken)); err != nil {
		t.Fatal(err)
	}
	if r.owners[0].activeImages.Load() != 0 {
		t.Fatal("delete returned before native read joined")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("retired read returned pixels")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked read outlived delete")
	}
}
func TestNHA202MediaNativeVideoSessionScopeAndOwnedCancellationTLS(t *testing.T) {
	r := nha202NewMediaAPI(t)
	source, err := nha202LoadVideoSource("../../qualification/frontend/video")
	if err != nil {
		t.Fatal(err)
	}
	r.owners[0].mu.Lock()
	r.owners[0].video = source
	r.owners[0].mu.Unlock()
	session := r.open(t, 0)
	ctx, cancel := context.WithCancel(r.ctx)
	stream, err := r.api.StreamMediaSession(ctx, nha162Bearer(&api.StreamMediaSessionRequest{Name: session.Name}, r.adminToken))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if !stream.Receive() || stream.Msg().Sequence != 1 || stream.Msg().GetHeader() == nil || stream.Msg().GetHeader().MediaSession.Name != session.Name {
		cancel()
		t.Fatal("native header", stream.Err())
	}
	if !stream.Receive() || stream.Msg().Sequence != 2 || stream.Msg().GetSample() == nil || stream.Msg().GetSample().MediaSession != session.Name || len(stream.Msg().GetSample().Avcc) == 0 {
		cancel()
		t.Fatal("native first keyframe", stream.Err())
	}
	cancel()
	stream.Close()
	deadline := time.Now().Add(3 * time.Second)
	for r.owners[0].activeVideos.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("native video producer did not join")
		}
		time.Sleep(time.Millisecond)
	}
	_, err = r.api.GetMediaSession(r.ctx, nha162Bearer(&api.GetMediaSessionRequest{Name: session.Name}, r.adminToken))
	nha162Code(t, err, connect.CodeNotFound)
}
