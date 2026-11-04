package mediacontent

import (
	"bytes"
	"context"
	"encoding/binary"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mediasessions"
)

// Owner is the native content boundary. It must compare the supplied exact
// binding before every content read and return no provider URL or credential.
// An implementation retains its pixels, native stream and network/TLS policy;
// the generic resource layer never synthesizes a provider or hardware endpoint.
type Owner interface {
	Source(context.Context, domain.EntityID, authorization.CapabilitySlot) (Source, error)
	AdmitArtwork(context.Context, mediasessions.SourceBinding, [32]byte, []byte) error
	Events(context.Context, mediasessions.SourceBinding) (EventCollection, error)
	Image(context.Context, mediasessions.SourceBinding, [32]byte, []byte, mediasessions.ImageTarget) (Image, error)
	Subscribe(context.Context, mediasessions.SourceBinding) (Subscription, error)
	Video(context.Context, mediasessions.SourceBinding) (VideoStream, error)
}
type Subscription interface {
	Changes() <-chan struct{}
	Close()
}
type VideoStream interface {
	Header() VideoHeader
	Next(context.Context) (VideoSample, error)
	Close()
}

type VideoHeader struct {
	binding            mediasessions.SourceBinding
	sps, pps           []byte
	width, height, fps int32
	valid              bool
}

func NewVideoHeader(binding mediasessions.SourceBinding, sps, pps []byte, width, height, fps int32) (VideoHeader, error) {
	if !binding.Valid() || binding.Kind() != mediasessions.Camera || len(sps) < 4 || len(sps) > 1024 || len(pps) < 2 || len(pps) > 1024 || sps[0]&0x9f != 7 || pps[0]&0x9f != 8 || sps[1] != 66 || width < 1 || width > 1920 || height < 1 || height > 1080 || fps < 1 || fps > 30 {
		return VideoHeader{}, ErrInvalid
	}
	return VideoHeader{binding, bytes.Clone(sps), bytes.Clone(pps), width, height, fps, true}, nil
}
func (h VideoHeader) Valid() bool            { return h.valid }
func (h VideoHeader) SPS() []byte            { return bytes.Clone(h.sps) }
func (h VideoHeader) PPS() []byte            { return bytes.Clone(h.pps) }
func (h VideoHeader) Width() int32           { return h.width }
func (h VideoHeader) Height() int32          { return h.height }
func (h VideoHeader) FramesPerSecond() int32 { return h.fps }

type VideoSample struct {
	binding  mediasessions.SourceBinding
	bytes    []byte
	offset   time.Duration
	keyframe bool
	valid    bool
}

// NewVideoSample admits one baseline AVCC access unit, with no in-band SPS/PPS,
// mixed key/predicted VCLs, open NAL types or submicrosecond timestamps.
func NewVideoSample(binding mediasessions.SourceBinding, raw []byte, offset time.Duration) (VideoSample, error) {
	if !binding.Valid() || binding.Kind() != mediasessions.Camera || len(raw) < 5 || len(raw) > 512<<10 || offset < 0 || offset > mediasessions.MaxLifetime || offset%time.Microsecond != 0 {
		return VideoSample{}, ErrInvalid
	}
	remaining := raw
	count := 0
	kind := byte(0)
	for len(remaining) > 0 {
		count++
		if count > 64 || len(remaining) < 4 {
			return VideoSample{}, ErrInvalid
		}
		length := uint64(binary.BigEndian.Uint32(remaining))
		remaining = remaining[4:]
		if length < 1 || length > uint64(len(remaining)) {
			return VideoSample{}, ErrInvalid
		}
		nal := remaining[:int(length)]
		remaining = remaining[int(length):]
		if nal[0]&0x80 != 0 {
			return VideoSample{}, ErrInvalid
		}
		next := nal[0] & 31
		switch next {
		case 1, 5:
			if kind != 0 && kind != next {
				return VideoSample{}, ErrInvalid
			}
			kind = next
		case 6, 9:
		default:
			return VideoSample{}, ErrInvalid
		}
	}
	if kind == 0 {
		return VideoSample{}, ErrInvalid
	}
	return VideoSample{binding, bytes.Clone(raw), offset, kind == 5, true}, nil
}
func (s VideoSample) Valid() bool           { return s.valid }
func (s VideoSample) AVCC() []byte          { return bytes.Clone(s.bytes) }
func (s VideoSample) Offset() time.Duration { return s.offset }
func (s VideoSample) Keyframe() bool        { return s.keyframe }

func (h VideoHeader) Matches(binding mediasessions.SourceBinding) bool { return h.binding == binding }
func (s VideoSample) Matches(binding mediasessions.SourceBinding) bool { return s.binding == binding }
