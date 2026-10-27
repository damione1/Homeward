package server

// Native-video qualification facade only. No production registration, provider
// URL, codec process, transcode, audio or real camera/device driver.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	cv "github.com/damione1/Homeward/gen/home/cameras/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type nhf017VideoMode uint8

const (
	nhf017VideoAvailable nhf017VideoMode = iota
	nhf017VideoUnsupported
	nhf017VideoNegotiationFailure
)

type nhf017VideoSource struct {
	sps, pps           []byte
	width, height, fps uint32
	samples            [][]byte
}

// Trusted original synthetic fixture, bounded and hashed against its retained
// provenance. No request supplies paths, bytes, codec names or shell arguments.
func nhf017LoadVideoSource(root string) (*nhf017VideoSource, error) {
	file, err := os.Open(filepath.Join(root, "provenance.json"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		SHA256 string `json:"sha256"`
		Width  uint32 `json:"width"`
		Height uint32 `json:"height"`
		FPS    uint32 `json:"fps"`
		Frames int    `json:"frames"`
		Audio  bool   `json:"audio"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 8193))
	err = decoder.Decode(&manifest)
	extra := decoder.Decode(new(any))
	file.Close()
	if err != nil || extra != io.EOF || manifest.Audio || manifest.Width == 0 || manifest.Width > 1920 || manifest.Height == 0 || manifest.Height > 1080 || manifest.FPS == 0 || manifest.FPS > 30 || manifest.Frames < 10 || manifest.Frames > 120 {
		return nil, errors.New("invalid synthetic video manifest")
	}
	file, err = os.Open(filepath.Join(root, "synthetic.h264"))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 16*1024*1024+1))
	file.Close()
	if err != nil || len(raw) > 16*1024*1024 {
		return nil, errors.New("synthetic video exceeds byte limit")
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != manifest.SHA256 {
		return nil, errors.New("synthetic video source digest changed")
	}
	type start struct{ begin, payload int }
	starts := []start{}
	for offset := 0; offset+3 <= len(raw); {
		length := 0
		switch {
		case bytes.HasPrefix(raw[offset:], []byte{0, 0, 0, 1}):
			length = 4
		case bytes.HasPrefix(raw[offset:], []byte{0, 0, 1}):
			length = 3
		}
		if length == 0 {
			offset++
			continue
		}
		starts = append(starts, start{offset, offset + length})
		offset += length
		if len(starts) > 120*64 {
			return nil, errors.New("too many synthetic video NALs")
		}
	}
	source := &nhf017VideoSource{width: manifest.Width, height: manifest.Height, fps: manifest.FPS}
	nals := 0
	for index, start := range starts {
		end := len(raw)
		if index+1 < len(starts) {
			end = starts[index+1].begin
		}
		nal := raw[start.payload:end]
		if len(nal) == 0 || nal[0]&0x80 != 0 {
			return nil, errors.New("invalid synthetic video NAL")
		}
		switch nal[0] & 31 {
		case 7:
			if len(nal) < 4 || len(nal) > 1024 || nal[1] != 66 || source.sps != nil && !bytes.Equal(source.sps, nal) {
				return nil, errors.New("invalid synthetic baseline SPS")
			}
			source.sps = bytes.Clone(nal)
			continue
		case 8:
			if len(nal) < 2 || len(nal) > 1024 || source.pps != nil && !bytes.Equal(source.pps, nal) {
				return nil, errors.New("invalid synthetic PPS")
			}
			source.pps = bytes.Clone(nal)
			continue
		case 9:
			if len(source.samples) >= 120 {
				return nil, errors.New("too many synthetic video samples")
			}
			source.samples = append(source.samples, nil)
			nals = 0
		case 1, 5, 6:
		default:
			return nil, errors.New("unexpected synthetic video NAL")
		}
		nals++
		if len(source.samples) == 0 || nals > 64 {
			return nil, errors.New("invalid synthetic video access unit")
		}
		sample := &source.samples[len(source.samples)-1]
		if len(nal)+len(*sample)+4 > 512*1024 {
			return nil, errors.New("synthetic video sample exceeds limit")
		}
		*sample = binary.BigEndian.AppendUint32(*sample, uint32(len(nal)))
		*sample = append(*sample, nal...)
	}
	if len(source.samples) != manifest.Frames || source.sps == nil || source.pps == nil {
		return nil, errors.New("incomplete synthetic video")
	}
	return source, nil
}

func (m *nhf016CameraMock) ReadVideo(ctx context.Context, request *connect.Request[cv.ReadVideoRequest], stream *connect.ServerStream[cv.ReadVideoResponse]) error {
	if !nhf016KnownCameraMessage(request.Msg) || !nhf016ValidCameraRef(request.Msg.Session) {
		return nhf016CameraError(connect.CodeInvalidArgument)
	}
	permit, err := m.permit(ctx, request.Msg.Session.EntityId)
	if err != nil {
		return err
	}
	defer permit.Release()
	principal, actor, err := nhf016CameraPrincipal(ctx)
	if err != nil {
		return err
	}
	subscription, err := m.authority.Subscribe(ctx, principal)
	if err != nil {
		return nhf016CameraRPCError(err)
	}
	defer subscription.Close()
	var owned *nhf016CameraLease
	m.videoStreams.Add(1)
	defer m.videoStreams.Add(-1)
	defer func() {
		if owned != nil {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.videoOwners[actor] == owned {
				delete(m.videoOwners, actor)
			}
			nhf016EndCameraLease(owned, nhf016LeaseClosed)
		}
	}()
	var sequence, sampleIndex uint64
	for {
		var lease *cv.Lease
		var done <-chan struct{}
		var source *nhf017VideoSource
		var state nhf016SessionStatus
		var mode nhf017VideoMode
		err = m.withCamera(permit.Context(), request.Msg.Session.EntityId, func(current authorization.Current, currentActor domain.ActorID, _ *nhf016CameraRecord) error {
			currentLease, status, err := m.session(current, currentActor, request.Msg.Session)
			if err != nil {
				return err
			}
			state = status
			source = m.videoSource
			mode = m.videoMode
			if currentLease != nil {
				lease = proto.Clone(currentLease.wire).(*cv.Lease)
				done = currentLease.done
			}
			if state == nhf016SessionValid && owned == nil {
				if m.videoOwners[currentActor] != nil || len(m.videoOwners) >= 8 {
					return nhf016CameraError(connect.CodeResourceExhausted)
				}
				if m.videoOwners == nil {
					m.videoOwners = make(map[domain.ActorID]*nhf016CameraLease)
				}
				m.videoOwners[currentActor] = currentLease
				owned = currentLease
			}
			return nil
		})
		if err != nil {
			return err
		}
		response := &cv.ReadVideoResponse{Sequence: sequence + 1}
		terminal := false
		switch state {
		case nhf016SessionExpired:
			response.Frame = &cv.ReadVideoResponse_Expired{Expired: &emptypb.Empty{}}
			terminal = true
		case nhf016SessionRevoked:
			response.Frame = &cv.ReadVideoResponse_Revoked{Revoked: &emptypb.Empty{}}
			terminal = true
		case nhf016SessionWrongCamera:
			return nhf016CameraError(connect.CodePermissionDenied)
		case nhf016SessionValid:
			switch mode {
			case nhf017VideoUnsupported:
				response.Frame = &cv.ReadVideoResponse_Unsupported{Unsupported: &emptypb.Empty{}}
				terminal = true
			case nhf017VideoNegotiationFailure:
				response.Frame = &cv.ReadVideoResponse_Unavailable{Unavailable: &emptypb.Empty{}}
				terminal = true
			case nhf017VideoAvailable:
				if source == nil {
					response.Frame = &cv.ReadVideoResponse_Unsupported{Unsupported: &emptypb.Empty{}}
					terminal = true
				} else if sequence == 0 {
					response.Frame = &cv.ReadVideoResponse_Header{Header: &cv.VideoHeader{Lease: lease, Codec: cv.VideoCodec_VIDEO_CODEC_H264_BASELINE, Sps: source.sps, Pps: source.pps, Width: source.width, Height: source.height, FramesPerSecond: source.fps}}
				} else {
					response.Frame = &cv.ReadVideoResponse_Sample{Sample: &cv.VideoSample{Session: proto.Clone(lease.Session).(*cv.SessionRef), PresentationMicros: sampleIndex * 1_000_000 / uint64(source.fps), Avcc: source.samples[sampleIndex%uint64(len(source.samples))]}}
					sampleIndex++
				}
			default:
				return nhf016CameraError(connect.CodeInternal)
			}
		default:
			return nhf016CameraError(connect.CodeInternal)
		}
		if !permit.Ready() {
			return nhf016CameraError(connect.CodePermissionDenied)
		}
		if err = stream.Send(response); err != nil {
			return err
		}
		sequence++
		if response.GetSample() != nil {
			m.videoSamples.Add(1)
		}
		if terminal {
			return nil
		}
		// Header and first keyframe are adjacent. Later frames are server-paced,
		// bounded by lease expiry; authorization changes/close wake immediately.
		if sequence == 1 {
			continue
		}
		wait := time.Second / time.Duration(source.fps)
		if remaining := time.Until(lease.ExpiresAt.AsTime()); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-permit.Context().Done():
			timer.Stop()
			return nhf016CameraRPCError(context.Cause(permit.Context()))
		case <-done:
			timer.Stop()
		case <-subscription.Changes():
			timer.Stop()
		case <-timer.C:
		}
	}
}
