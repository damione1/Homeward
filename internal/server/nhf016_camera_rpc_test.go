package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	cv "github.com/damione1/Homeward/gen/home/cameras/v1"
	"github.com/damione1/Homeward/gen/home/cameras/v1/camerasv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const nhf016AllCameras = `  - scope: {family: media, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`

type nhf016CameraSource struct {
	Entity string   `json:"entity_id"`
	Title  string   `json:"title"`
	Source string   `json:"source"`
	RGB    [3]uint8 `json:"rgb"`
	Events uint32   `json:"events"`
}

func nhf016ReadCameraSources(root string) ([]nhf016CameraSource, error) {
	file, err := os.Open(filepath.Join(root, "cameras.json"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 8193))
	decoder.DisallowUnknownFields()
	var input struct {
		SyntheticOnly bool                 `json:"synthetic_only"`
		Cameras       []nhf016CameraSource `json:"cameras"`
	}
	if err = decoder.Decode(&input); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || !input.SyntheticOnly || len(input.Cameras) != 2 {
		return nil, fmt.Errorf("two bounded synthetic-only camera configurations required")
	}
	seen := make(map[string]bool)
	for _, camera := range input.Cameras {
		if _, err = nhf016CameraID(camera.Entity); err != nil || seen[camera.Entity] || camera.Title == "" || len(camera.Title) > 1024 || camera.Source == "" || len(camera.Source) > 1024 || camera.Events > 10000 {
			return nil, fmt.Errorf("invalid synthetic camera source")
		}
		seen[camera.Entity] = true
	}
	return input.Cameras, nil
}
func nhf016NewCameraMock(sources []nhf016CameraSource) (*nhf016CameraMock, error) {
	m := &nhf016CameraMock{records: make(map[domain.EntityID]*nhf016CameraRecord), leases: make(map[[32]byte]*nhf016CameraLease), lifetime: nhf016LeaseLifetime, now: time.Now}
	if _, err := rand.Read(m.cursorKey[:]); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, source := range sources {
		entity, err := nhf016CameraID(source.Entity)
		if err != nil {
			return nil, err
		}
		bitmap := image.NewNRGBA(image.Rect(0, 0, 96, 64))
		for y := 0; y < 64; y++ {
			for x := 0; x < 96; x++ {
				bitmap.SetNRGBA(x, y, color.NRGBA{R: source.RGB[0], G: source.RGB[1], B: source.RGB[2], A: 255})
			}
		}
		var encoded bytes.Buffer
		if err = png.Encode(&encoded, bitmap); err != nil {
			return nil, err
		}
		record := &nhf016CameraRecord{metadata: &cv.Camera{EntityId: source.Entity, Title: source.Title, Source: source.Source, Availability: cv.Availability_AVAILABILITY_AVAILABLE, Provenance: cv.DataProvenance_DATA_PROVENANCE_SYNTHETIC}, png: encoded.Bytes(), revision: 1, changed: make(chan struct{})}
		for index := uint32(0); index < source.Events; index++ {
			record.events = append(record.events, &cv.Event{EntityId: source.Entity, EventId: fmt.Sprintf("event-%05d", index), Title: "Événement synthétique", Source: source.Source, CapturedAt: timestamppb.New(now.Add(-time.Duration(index+1) * time.Second)), HasImage: index%3 != 2})
		}
		m.records[entity] = record
	}
	return m, nil
}

type nhf016CameraRig struct {
	*nha162RPCRig
	handler      *nhf016CameraMock
	api          camerasv1connect.CameraServiceClient
	cameraServer *httptest.Server
}

func nhf016NewCameraRig(t *testing.T, lifetime time.Duration) *nhf016CameraRig {
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
	m, err := nhf016NewCameraMock(sources)
	if err != nil {
		t.Fatal(err)
	}
	m.authority = base.coordinator.authority
	plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: base.store})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.register(plan); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, base.store, base.coordinator)
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
	t.Cleanup(func() { server.CloseClientConnections(); server.Close() })
	client := server.Client()
	client.Timeout = 20 * time.Second
	return &nhf016CameraRig{base, m, camerasv1connect.NewCameraServiceClient(client, server.URL), server}
}
func (r *nhf016CameraRig) catalog(t *testing.T) []*cv.Camera {
	t.Helper()
	response, err := r.api.ListCameras(r.ctx, nha162Bearer(&cv.ListCamerasRequest{}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.Cameras
}
func (r *nhf016CameraRig) open(t *testing.T, id string) *cv.Lease {
	t.Helper()
	response, err := r.api.OpenSession(r.ctx, nha162Bearer(&cv.OpenSessionRequest{EntityId: id}, r.adminToken))
	if err != nil || response.Msg.GetLease() == nil {
		t.Fatal("camera lease", err)
	}
	return response.Msg.GetLease()
}
func nhf016Snapshot(lease *cv.Lease) *cv.ReadImageRequest {
	return &cv.ReadImageRequest{Key: &cv.ImageKey{Session: proto.Clone(lease.Session).(*cv.SessionRef), Target: &cv.ImageKey_Snapshot{Snapshot: &emptypb.Empty{}}}}
}

func TestNHF016CameraTLSExactSourcesAndSessionBinding(t *testing.T) {
	r := nhf016NewCameraRig(t, 60*time.Second)
	if _, err := r.api.ListCameras(r.ctx, connect.NewRequest(&cv.ListCamerasRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("anonymous camera metadata", err)
	}
	cameras := r.catalog(t)
	if len(cameras) != 2 || cameras[0].Title != cameras[1].Title || cameras[0].EntityId == cameras[1].EntityId {
		t.Fatal("exact separate synthetic camera identities required")
	}
	first, second := r.open(t, cameras[0].EntityId), r.open(t, cameras[1].EntityId)
	left, err := r.api.ReadImage(r.ctx, nha162Bearer(nhf016Snapshot(first), r.adminToken))
	if err != nil || left.Msg.GetImage() == nil {
		t.Fatal(err)
	}
	right, err := r.api.ReadImage(r.ctx, nha162Bearer(nhf016Snapshot(second), r.adminToken))
	if err != nil || right.Msg.GetImage() == nil {
		t.Fatal(err)
	}
	if sha256.Sum256(left.Msg.GetImage().Png) == sha256.Sum256(right.Msg.GetImage().Png) {
		t.Fatal("camera images exchanged")
	}
	wrong := nhf016Snapshot(first)
	wrong.Key.Session.EntityId = second.Session.EntityId
	response, err := r.api.ReadImage(r.ctx, nha162Bearer(wrong, r.adminToken))
	if err != nil || response.Msg.GetWrongCamera() == nil {
		t.Fatal("foreign camera not refused distinctly", err)
	}
	otherToken := r.login(t, "NHA162 Admin", "NHA162 synthetic admin password canary")
	response, err = r.api.ReadImage(r.ctx, nha162Bearer(nhf016Snapshot(first), otherToken))
	if err != nil || response.Msg.GetRevoked() == nil {
		t.Fatal("another native session reused camera reference", err)
	}
	if _, err = r.api.CloseSession(r.ctx, nha162Bearer(&cv.CloseSessionRequest{Session: first.Session}, otherToken)); err != nil {
		t.Fatal(err)
	}
	response, err = r.api.ReadImage(r.ctx, nha162Bearer(nhf016Snapshot(first), r.adminToken))
	if err != nil || response.Msg.GetImage() == nil {
		t.Fatal("foreign login invalidated original lease", err)
	}
	for _, lease := range []*cv.Lease{first, second} {
		if _, err = r.api.CloseSession(r.ctx, nha162Bearer(&cv.CloseSessionRequest{Session: lease.Session}, r.adminToken)); err != nil {
			t.Fatal(err)
		}
	}
	response, err = r.api.ReadImage(r.ctx, nha162Bearer(nhf016Snapshot(first), r.adminToken))
	if err != nil || response.Msg.GetRevoked() == nil {
		t.Fatal("closed reference still exposes pixels", err)
	}
}

func TestNHF016CameraTLSPaginationAndBoundedLeaseCapacity(t *testing.T) {
	r := nhf016NewCameraRig(t, 60*time.Second)
	cameras := r.catalog(t)
	lease := r.open(t, cameras[0].EntityId)
	response, err := r.api.ListEvents(r.ctx, nha162Bearer(&cv.ListEventsRequest{Session: lease.Session, PageSize: 1}, r.adminToken))
	if err != nil || len(response.Msg.GetPage().NextCursor) != 40 {
		t.Fatal("first bounded event page", err)
	}
	page := response.Msg.GetPage()
	request := &cv.ListEventsRequest{Session: lease.Session, PageSize: 1, Cursor: bytes.Clone(page.NextCursor)}
	next, err := r.api.ListEvents(r.ctx, nha162Bearer(request, r.adminToken))
	if err != nil || next.Msg.GetPage().Events[0].EventId == page.Events[0].EventId {
		t.Fatal("pagination duplicated an event", err)
	}
	request.Cursor[0] ^= 1
	bad, err := r.api.ListEvents(r.ctx, nha162Bearer(request, r.adminToken))
	if err != nil || bad.Msg.GetResyncRequired() == nil {
		t.Fatal("tampered cursor admitted", err)
	}
	request.Cursor = bytes.Clone(page.NextCursor)
	r.handler.mu.Lock()
	record := r.handler.records[mustNHF016CameraID(t, lease.Session.EntityId)]
	record.revision++
	close(record.changed)
	record.changed = make(chan struct{})
	r.handler.mu.Unlock()
	bad, err = r.api.ListEvents(r.ctx, nha162Bearer(request, r.adminToken))
	if err != nil || bad.Msg.GetResyncRequired() == nil {
		t.Fatal("mixed event revisions admitted", err)
	}
	for range 7 {
		r.open(t, cameras[0].EntityId)
	}
	if _, err = r.api.OpenSession(r.ctx, nha162Bearer(&cv.OpenSessionRequest{EntityId: cameras[0].EntityId}, r.adminToken)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatal("unbounded camera leases", err)
	}
}
func mustNHF016CameraID(t *testing.T, raw string) domain.EntityID {
	t.Helper()
	id, err := nhf016CameraID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNHF016CameraTLSLongEventsRemainByteBounded(t *testing.T) {
	r := nhf016NewCameraRig(t, 60*time.Second)
	id := r.catalog(t)[0].EntityId
	r.handler.mu.Lock()
	record := r.handler.records[mustNHF016CameraID(t, id)]
	sample := proto.Clone(record.events[0]).(*cv.Event)
	record.events = nil
	for i := 0; i < 10000; i++ {
		event := proto.Clone(sample).(*cv.Event)
		event.EventId = fmt.Sprintf("wide-event-%05d", i)
		event.Source = strings.Repeat("s", 1024)
		event.CapturedAt = timestamppb.New(time.Now().Add(-time.Duration(i+1) * time.Second))
		record.events = append(record.events, event)
	}
	r.handler.mu.Unlock()
	lease := r.open(t, id)
	var cursor []byte
	count := 0
	previous := ""
	for {
		response, err := r.api.ListEvents(r.ctx, nha162Bearer(&cv.ListEventsRequest{Session: lease.Session, PageSize: 64, Cursor: cursor}, r.adminToken))
		if err != nil || response.Msg.GetPage() == nil {
			t.Fatal("bounded long event page", err)
		}
		page := response.Msg.GetPage()
		if proto.Size(response.Msg) > 64*1024 || len(page.Events) > 64 || len(page.Events) == 0 || page.Total != 10000 {
			t.Fatal("event page violated byte/row bounds")
		}
		for _, event := range page.Events {
			if event.EntityId != id || event.EventId <= previous {
				t.Fatal("event omitted, reordered or duplicated")
			}
			previous = event.EventId
			count++
		}
		cursor = page.NextCursor
		if len(cursor) == 0 {
			break
		}
	}
	if count != 10000 {
		t.Fatal("long capture incomplete", count)
	}
}

func TestNHF016CameraTLSExpiryAndRevocationWhileDownloading(t *testing.T) {
	r := nhf016NewCameraRig(t, 60*time.Second)
	camera := r.catalog(t)[0]
	r.handler.mu.Lock()
	r.handler.lifetime = 200 * time.Millisecond
	r.handler.imageGate = make(chan struct{})
	r.handler.mu.Unlock()
	lease := r.open(t, camera.EntityId)
	result, err := r.api.ReadImage(r.ctx, nha162Bearer(nhf016Snapshot(lease), r.adminToken))
	if err != nil || result.Msg.GetExpired() == nil {
		t.Fatal("expired paused image exposed pixels", err)
	}
	r.handler.mu.Lock()
	r.handler.lifetime = nhf016LeaseLifetime
	r.handler.mu.Unlock()
	lease = r.open(t, camera.EntityId)
	completed := make(chan error, 1)
	go func() {
		response, err := r.api.ReadImage(r.ctx, nha162Bearer(nhf016Snapshot(lease), r.adminToken))
		if err == nil && response.Msg.GetImage() != nil {
			err = fmt.Errorf("revoked image exposed pixels")
		}
		completed <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.handler.activeReads.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("paused image did not start")
		}
		time.Sleep(time.Millisecond)
	}
	nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("revoked download did not fail closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoked download was not cancelled")
	}
	if r.handler.activeReads.Load() != 0 {
		t.Fatal("private image read not joined")
	}
}

func TestNHF016CameraTLSWatchJoinsAfterCloseAndObjectRevocation(t *testing.T) {
	r := nhf016NewCameraRig(t, 60*time.Second)
	lease := r.open(t, r.catalog(t)[0].EntityId)
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	stream, err := r.api.WatchSession(ctx, nha162Bearer(&cv.WatchSessionRequest{Session: lease.Session}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil || stream.Msg().Sequence != 1 {
		t.Fatal("native camera Ready absent", stream.Err())
	}
	if _, err = r.api.CloseSession(r.ctx, nha162Bearer(&cv.CloseSessionRequest{Session: lease.Session}, r.adminToken)); err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || stream.Msg().GetRevoked() == nil || stream.Msg().Sequence != 2 {
		t.Fatal("closed private camera stream not retired", stream.Err())
	}
	if stream.Receive() || stream.Err() != nil {
		t.Fatal("terminal frame did not close stream", stream.Err())
	}
	lease = r.open(t, lease.Session.EntityId)
	second, err := r.api.WatchSession(ctx, nha162Bearer(&cv.WatchSessionRequest{Session: lease.Session}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !second.Receive() || second.Msg().GetReady() == nil {
		t.Fatal("second camera stream", second.Err())
	}
	nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
	if second.Receive() || second.Err() == nil {
		t.Fatal("object revocation left camera stream alive")
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.handler.activeStreams.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("camera streams did not join")
		}
		time.Sleep(time.Millisecond)
	}
}
