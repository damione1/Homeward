package server

// Qualification source only: SYNTHETIC pixels/events/video from local fixtures.
// There is no production provider, URL, driver or memory lease authority here.
import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
)

type nha202MediaOwner struct {
	mu                                 sync.Mutex
	binding                            mediasessions.SourceBinding
	label                              string
	png                                []byte
	events                             []mediacontent.Event
	revision                           int64
	subscriptions                      map[*nha202MediaSubscription]struct{}
	key                                [32]byte
	artworkExpiry                      time.Time
	artworkDenied                      bool
	offline, wrongImage, missingImage  bool
	videoUnavailable, videoUnsupported bool
	imageGate                          <-chan struct{}
	video                              *nha202VideoSource
	activeImages, activeVideos         atomic.Int64
	imageRequests, videoSamples        atomic.Int64
}

func nha202NewMediaOwner(t *testing.T, id string, kind mediasessions.Kind, rgb color.NRGBA) *nha202MediaOwner {
	t.Helper()
	entity, err := domain.ParseEntityID(id)
	if err != nil {
		t.Fatal(err)
	}
	schema, endpoint := "home.cameras.v1.Camera", "camera"
	if kind == mediasessions.Artwork {
		schema, endpoint = "home.media.v1", "media"
	}
	slot, err := authorization.NewCapabilitySlot(schema, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var codec [32]byte
	codec[0] = 1
	binding, err := mediasessions.NewSourceBinding(entity, slot, kind, 1, "synthetic-nha202-epoch", codec)
	if err != nil {
		t.Fatal(err)
	}
	m := &nha202MediaOwner{binding: binding, label: "Duplicate synthetic title", revision: 1, subscriptions: map[*nha202MediaSubscription]struct{}{}, artworkExpiry: time.Now().Add(time.Minute)}
	if _, err := rand.Read(m.key[:]); err != nil {
		t.Fatal(err)
	}
	bitmap := image.NewNRGBA(image.Rect(0, 0, 96, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			bitmap.SetNRGBA(x, y, rgb)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, bitmap); err != nil {
		t.Fatal(err)
	}
	m.png = encoded.Bytes()
	if kind == mediasessions.Camera {
		for index, id := range []string{"event-1", "event-2", "event-3"} {
			uid, err := mediasessions.ParseEventID(id)
			if err != nil {
				t.Fatal(err)
			}
			event, err := mediacontent.NewEvent(uid, "Synthetic event", "Synthetic source", time.Now().Add(-time.Duration(index+1)*time.Minute), index != 2)
			if err != nil {
				t.Fatal(err)
			}
			m.events = append(m.events, event)
		}
	}
	return m
}
func (m *nha202MediaOwner) Source(ctx context.Context, entity domain.EntityID, slot authorization.CapabilitySlot) (mediacontent.Source, error) {
	if ctx.Err() != nil {
		return mediacontent.Source{}, context.Cause(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entity != m.binding.Entity() || slot != m.binding.Slot() {
		return mediacontent.Source{}, mediacontent.ErrSourceChanged
	}
	availability, freshness := domain.AvailabilityAvailable, domain.FreshnessFresh
	if m.offline {
		availability, freshness = domain.AvailabilityUnavailable, domain.FreshnessStale
	}
	quality, err := domain.NewQuality(availability, freshness, domain.ProvenanceObserved)
	if err != nil {
		return mediacontent.Source{}, err
	}
	return mediacontent.NewSource(m.binding, m.label, quality, mediacontent.Synthetic)
}
func (m *nha202MediaOwner) reference(credential [32]byte) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.referenceLocked(credential)
}
func (m *nha202MediaOwner) referenceLocked(credential [32]byte) []byte {
	mac := hmac.New(sha256.New, m.key[:])
	binding := m.binding.Digest()
	mac.Write(binding[:])
	mac.Write(credential[:])
	return mac.Sum(nil)
}
func (m *nha202MediaOwner) AdmitArtwork(ctx context.Context, binding mediasessions.SourceBinding, credential [32]byte, reference []byte) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if binding != m.binding {
		return mediacontent.ErrSourceChanged
	}
	if binding.Kind() != mediasessions.Artwork || credential == ([32]byte{}) || !hmac.Equal(reference, m.referenceLocked(credential)) {
		return authorization.ErrDenied
	}
	if m.artworkDenied {
		return authorization.ErrDenied
	}
	if !time.Now().Before(m.artworkExpiry) {
		return mediacontent.ErrNotFound
	}
	if m.offline {
		return mediacontent.ErrUnavailable
	}
	return nil
}
func (m *nha202MediaOwner) Events(ctx context.Context, binding mediasessions.SourceBinding) (mediacontent.EventCollection, error) {
	if ctx.Err() != nil {
		return mediacontent.EventCollection{}, context.Cause(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if binding != m.binding {
		return mediacontent.EventCollection{}, mediacontent.ErrSourceChanged
	}
	if binding.Kind() != mediasessions.Camera {
		return mediacontent.EventCollection{}, mediacontent.ErrUnsupported
	}
	return mediacontent.NewEventCollection(binding, m.revision, m.events)
}
func (m *nha202MediaOwner) Image(ctx context.Context, binding mediasessions.SourceBinding, credential [32]byte, reference []byte, target mediasessions.ImageTarget) (mediacontent.Image, error) {
	m.imageRequests.Add(1)
	m.activeImages.Add(1)
	defer m.activeImages.Add(-1)
	m.mu.Lock()
	gate := m.imageGate
	m.mu.Unlock()
	if gate != nil {
		select {
		case <-ctx.Done():
			return mediacontent.Image{}, context.Cause(ctx)
		case <-gate:
		}
	}
	if ctx.Err() != nil {
		return mediacontent.Image{}, context.Cause(ctx)
	}
	if binding.Kind() == mediasessions.Artwork {
		if err := m.AdmitArtwork(ctx, binding, credential, reference); err != nil {
			return mediacontent.Image{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if binding != m.binding {
		return mediacontent.Image{}, mediacontent.ErrSourceChanged
	}
	if m.offline {
		return mediacontent.Image{}, mediacontent.ErrUnavailable
	}
	if m.missingImage {
		return mediacontent.Image{}, mediacontent.ErrNotFound
	}
	captured := time.Now().Add(-time.Second)
	if event, selected := target.Event(); selected {
		found := false
		for _, candidate := range m.events {
			if candidate.ID() == event && candidate.HasImage() {
				captured = candidate.CaptureTime()
				found = true
				break
			}
		}
		if !found {
			return mediacontent.Image{}, mediacontent.ErrNotFound
		}
	}
	if m.wrongImage {
		entity, _ := domain.ParseEntityID("other-native-owner")
		var codec [32]byte
		codec[0] = 1
		foreign, err := mediasessions.NewSourceBinding(entity, binding.Slot(), binding.Kind(), 1, "foreign-epoch", codec)
		if err != nil {
			return mediacontent.Image{}, err
		}
		return mediacontent.NewImage(foreign, target, reference, m.png, captured)
	}
	return mediacontent.NewImage(binding, target, reference, m.png, captured)
}

type nha202MediaSubscription struct {
	owner   *nha202MediaOwner
	changes chan struct{}
	once    sync.Once
}

func (s *nha202MediaSubscription) Changes() <-chan struct{} { return s.changes }
func (s *nha202MediaSubscription) Close() {
	s.once.Do(func() { s.owner.mu.Lock(); delete(s.owner.subscriptions, s); s.owner.mu.Unlock() })
}
func (m *nha202MediaOwner) Subscribe(ctx context.Context, binding mediasessions.SourceBinding) (mediacontent.Subscription, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if binding != m.binding {
		return nil, mediacontent.ErrSourceChanged
	}
	if len(m.subscriptions) >= 64 {
		return nil, mediacontent.ErrUnavailable
	}
	sub := &nha202MediaSubscription{owner: m, changes: make(chan struct{}, 1)}
	m.subscriptions[sub] = struct{}{}
	return sub, nil
}
func (m *nha202MediaOwner) invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revision++
	for sub := range m.subscriptions {
		select {
		case sub.changes <- struct{}{}:
		default:
		}
	}
}
func (m *nha202MediaOwner) Video(ctx context.Context, binding mediasessions.SourceBinding) (mediacontent.VideoStream, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if binding != m.binding {
		return nil, mediacontent.ErrSourceChanged
	}
	if m.offline {
		return nil, mediacontent.ErrUnavailable
	}
	if m.videoUnavailable {
		return nil, mediacontent.ErrUnavailable
	}
	if m.video == nil || m.videoUnsupported {
		return nil, mediacontent.ErrUnsupported
	}
	source := m.video
	if source.width < 1 || source.width > 1920 || source.height < 1 || source.height > 1080 || source.fps < 1 || source.fps > 30 {
		return nil, mediacontent.ErrInvalid
	}
	header, err := mediacontent.NewVideoHeader(binding, source.sps, source.pps, int32(source.width), int32(source.height), int32(source.fps))
	if err != nil {
		return nil, err
	}
	m.activeVideos.Add(1)
	return &nha202NativeVideo{owner: m, binding: binding, source: source, header: header, start: time.Now()}, nil
}

type nha202NativeVideo struct {
	owner   *nha202MediaOwner
	binding mediasessions.SourceBinding
	source  *nha202VideoSource
	header  mediacontent.VideoHeader
	start   time.Time
	index   int64
	once    sync.Once
}

func (v *nha202NativeVideo) Header() mediacontent.VideoHeader { return v.header }
func (v *nha202NativeVideo) Close()                           { v.once.Do(func() { v.owner.activeVideos.Add(-1) }) }
func (v *nha202NativeVideo) Next(ctx context.Context) (mediacontent.VideoSample, error) {
	offset := time.Duration(v.index*1_000_000/int64(v.source.fps)) * time.Microsecond
	if offset > mediasessions.MaxLifetime {
		return mediacontent.VideoSample{}, errors.New("synthetic native video exceeds lifetime")
	}
	timer := time.NewTimer(time.Until(v.start.Add(offset)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return mediacontent.VideoSample{}, context.Cause(ctx)
	case <-timer.C:
	}
	raw := v.source.samples[v.index%int64(len(v.source.samples))]
	v.index++
	v.owner.videoSamples.Add(1)
	return mediacontent.NewVideoSample(v.binding, raw, offset)
}
