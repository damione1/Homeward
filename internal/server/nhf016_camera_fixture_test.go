package server

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/mediasessions"
	"github.com/jackc/pgx/v5"
)

type nhf016CameraControl string

const (
	nhf016CameraPauseImages      nhf016CameraControl = "pause-images"
	nhf016CameraResumeImages     nhf016CameraControl = "resume-images"
	nhf016CameraExpire           nhf016CameraControl = "expire-leases"
	nhf016CameraRevoke           nhf016CameraControl = "revoke-read"
	nhf016CameraAllow            nhf016CameraControl = "allow-read"
	nhf016CameraOutage           nhf016CameraControl = "outage"
	nhf016CameraRestore          nhf016CameraControl = "restore-network"
	nhf016CameraSnapshot         nhf016CameraControl = "snapshot"
	nhf016CameraMissingImage     nhf016CameraControl = "missing-image"
	nhf016CameraWrongCamera      nhf016CameraControl = "wrong-camera"
	nhf016CameraRestoreImages    nhf016CameraControl = "restore-images"
	nhf016CameraChangeEvents     nhf016CameraControl = "change-events"
	nhf017CameraVideoUnsupported nhf016CameraControl = "video-unsupported"
	nhf017CameraVideoUnavailable nhf016CameraControl = "video-unavailable"
	nhf017CameraVideoAvailable   nhf016CameraControl = "video-available"
)

func (a *nhf016CameraControl) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch nhf016CameraControl(value) {
	case nhf016CameraPauseImages, nhf016CameraResumeImages, nhf016CameraExpire, nhf016CameraRevoke, nhf016CameraAllow, nhf016CameraOutage, nhf016CameraRestore, nhf016CameraSnapshot, nhf016CameraMissingImage, nhf016CameraWrongCamera, nhf016CameraRestoreImages, nhf016CameraChangeEvents, nhf017CameraVideoUnsupported, nhf017CameraVideoUnavailable, nhf017CameraVideoAvailable:
		*a = nhf016CameraControl(value)
		return nil
	default:
		return errors.New("unknown synthetic camera control")
	}
}

// Explicit isolated binary only; native accounts/SQL/TLS, no camera integration.
func TestNHF016CameraServer(t *testing.T) {
	out, root := os.Getenv("HOMEWARD_NHF016_FIXTURE"), os.Getenv("HOMEWARD_NHF016_SOURCES")
	if out == "" {
		t.Skip("explicit isolated NHF016 fixture only")
	}
	if os.Getenv("HOMEWARD_NHF016_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out || !filepath.IsAbs(root) {
		t.Fatal("fresh absolute synthetic-only camera fixture required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	r := nhf016NewResourceRig(t, 10*time.Minute)
	if videoRoot := os.Getenv("HOMEWARD_NHF017_VIDEO"); videoRoot != "" {
		if os.Getenv("HOMEWARD_NHF017_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(videoRoot) || filepath.Clean(videoRoot) != videoRoot {
			t.Fatal("explicit absolute synthetic-only video fixture required")
		}
		source, err := nha202LoadVideoSource(videoRoot)
		if err != nil {
			t.Fatal(err)
		}
		for _, owner := range r.owners {
			owner.mu.Lock()
			owner.video = source
			owner.mu.Unlock()
		}
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.cameraServer.Certificate().Raw})
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": r.cameraServer.URL, "ca_pem": string(certificate), "reader": map[string]string{"login_name": "NHA162 Admin", "password": "NHA162 synthetic admin password canary"}, "production_cameras": false})
	defer nhf003WritePrivate(t, out, "result.json", map[string]any{"synthetic_only": true, "real_device_driver_instantiated": false, "real_device_writes": 0})
	var pause chan struct{}
	var sequence uint64
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			t.Fatal(r.ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			file, err := os.Open(filepath.Join(out, "control.json"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var command struct {
				Sequence uint64              `json:"sequence"`
				Action   nhf016CameraControl `json:"action"`
				Entity   string              `json:"entity_id,omitempty"`
			}
			decoder := json.NewDecoder(io.LimitReader(file, 4097))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&command)
			extra := decoder.Decode(new(any))
			file.Close()
			if err != nil || extra != io.EOF || command.Sequence == 0 || command.Sequence > 128 {
				t.Fatal("invalid camera control", err)
			}
			if command.Sequence == sequence {
				continue
			}
			if command.Sequence != sequence+1 {
				t.Fatal("out-of-order camera control")
			}
			targeted := false
			switch command.Action {
			case nhf016CameraMissingImage, nhf016CameraWrongCamera, nhf016CameraRestoreImages, nhf016CameraChangeEvents:
				targeted = true
			case nhf016CameraPauseImages, nhf016CameraResumeImages, nhf016CameraExpire, nhf016CameraRevoke, nhf016CameraAllow, nhf016CameraOutage, nhf016CameraRestore, nhf016CameraSnapshot, nhf017CameraVideoUnsupported, nhf017CameraVideoUnavailable, nhf017CameraVideoAvailable:
			default:
				t.Fatal("unparsed camera control")
			}
			if targeted && command.Entity == "" || !targeted && command.Entity != "" {
				t.Fatal("unexpected camera control identity")
			}
			switch command.Action {
			case nhf017CameraVideoUnsupported, nhf017CameraVideoUnavailable, nhf017CameraVideoAvailable:
				for _, owner := range r.owners {
					owner.mu.Lock()
					owner.videoUnsupported = command.Action == nhf017CameraVideoUnsupported
					owner.videoUnavailable = command.Action == nhf017CameraVideoUnavailable
					owner.mu.Unlock()
				}
			case nhf016CameraRevoke:
				nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
			case nhf016CameraAllow:
				nha162SetPolicy(t, r.nha162RPCRig, r.admin, nhf016AllCameras)
			case nhf016CameraPauseImages:
				if pause != nil {
					t.Fatal("images already paused")
				}
				pause = make(chan struct{})
				for _, owner := range r.owners {
					owner.mu.Lock()
					owner.imageGate = pause
					owner.mu.Unlock()
				}
			case nhf016CameraResumeImages:
				if pause != nil {
					close(pause)
				}
				pause = nil
				for _, owner := range r.owners {
					owner.mu.Lock()
					owner.imageGate = nil
					owner.mu.Unlock()
				}
			case nhf016CameraExpire:
				r.clock.advance(mediasessions.MaxLifetime + time.Second)
				for _, owner := range r.owners {
					owner.invalidate()
				}
			case nhf016CameraMissingImage, nhf016CameraWrongCamera, nhf016CameraRestoreImages, nhf016CameraChangeEvents:
				owner := r.owners[command.Entity]
				if owner == nil {
					t.Fatal("unknown exact synthetic MediaSource")
				}
				if command.Action == nhf016CameraChangeEvents {
					owner.invalidate()
				} else {
					owner.mu.Lock()
					owner.missingImage = command.Action == nhf016CameraMissingImage
					owner.wrongImage = command.Action == nhf016CameraWrongCamera
					owner.mu.Unlock()
				}
			case nhf016CameraOutage, nhf016CameraRestore:
				for _, owner := range r.owners {
					owner.mu.Lock()
					owner.offline = command.Action == nhf016CameraOutage
					owner.mu.Unlock()
				}
				if command.Action == nhf016CameraOutage {
					r.cameraServer.CloseClientConnections()
				}
			case nhf016CameraSnapshot:
				var imageRequests, activeReads, activeStreams, videoStreams, videoSamples int64
				for _, owner := range r.owners {
					imageRequests += owner.imageRequests.Load()
					activeReads += owner.activeImages.Load()
					videoStreams += owner.activeVideos.Load()
					videoSamples += owner.videoSamples.Load()
					owner.mu.Lock()
					activeStreams += int64(len(owner.subscriptions))
					owner.mu.Unlock()
				}
				var leases int64
				if err = r.store.InTransaction(r.ctx, func(tx pgx.Tx) error {
					return tx.QueryRow(r.ctx, `SELECT count(*) FROM public.api_media_sessions WHERE retire_time IS NULL AND expire_time>$1`, r.clock.Now()).Scan(&leases)
				}); err != nil {
					t.Fatal(err)
				}
				nhf003WritePrivate(t, out, fmt.Sprintf("stats-%02d.json", command.Sequence), map[string]any{"image_requests": imageRequests, "active_reads": activeReads, "active_streams": activeStreams, "retained_leases": leases, "video_streams": videoStreams, "video_samples": videoSamples})
			default:
				t.Fatal("unparsed camera control")
			}
			sequence = command.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", sequence), command)
		}
	}
}
