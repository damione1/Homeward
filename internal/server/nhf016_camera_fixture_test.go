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

	"google.golang.org/protobuf/types/known/timestamppb"
)

type nhf016CameraControl string

const (
	nhf016CameraPauseImages   nhf016CameraControl = "pause-images"
	nhf016CameraResumeImages  nhf016CameraControl = "resume-images"
	nhf016CameraExpire        nhf016CameraControl = "expire-leases"
	nhf016CameraRevoke        nhf016CameraControl = "revoke-read"
	nhf016CameraAllow         nhf016CameraControl = "allow-read"
	nhf016CameraOutage        nhf016CameraControl = "outage"
	nhf016CameraRestore       nhf016CameraControl = "restore-network"
	nhf016CameraSnapshot      nhf016CameraControl = "snapshot"
	nhf016CameraMissingImage  nhf016CameraControl = "missing-image"
	nhf016CameraWrongCamera   nhf016CameraControl = "wrong-camera"
	nhf016CameraRestoreImages nhf016CameraControl = "restore-images"
	nhf016CameraChangeEvents  nhf016CameraControl = "change-events"
)

func (a *nhf016CameraControl) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch nhf016CameraControl(value) {
	case nhf016CameraPauseImages, nhf016CameraResumeImages, nhf016CameraExpire, nhf016CameraRevoke, nhf016CameraAllow, nhf016CameraOutage, nhf016CameraRestore, nhf016CameraSnapshot, nhf016CameraMissingImage, nhf016CameraWrongCamera, nhf016CameraRestoreImages, nhf016CameraChangeEvents:
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
	r := nhf016NewCameraRig(t, 10*time.Minute)
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
			case nhf016CameraPauseImages, nhf016CameraResumeImages, nhf016CameraExpire, nhf016CameraRevoke, nhf016CameraAllow, nhf016CameraOutage, nhf016CameraRestore, nhf016CameraSnapshot:
			default:
				t.Fatal("unparsed camera control")
			}
			if targeted && command.Entity == "" || !targeted && command.Entity != "" {
				t.Fatal("unexpected camera control identity")
			}
			switch command.Action {
			case nhf016CameraRevoke:
				nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
			case nhf016CameraAllow:
				nha162SetPolicy(t, r.nha162RPCRig, r.admin, nhf016AllCameras)
			case nhf016CameraPauseImages:
				if pause != nil {
					t.Fatal("images already paused")
				}
				pause = make(chan struct{})
				r.handler.mu.Lock()
				r.handler.imageGate = pause
				r.handler.mu.Unlock()
			case nhf016CameraResumeImages:
				r.handler.mu.Lock()
				if pause != nil {
					close(pause)
				}
				pause = nil
				r.handler.imageGate = nil
				r.handler.mu.Unlock()
			case nhf016CameraExpire:
				r.handler.mu.Lock()
				for _, lease := range r.handler.leases {
					lease.wire.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second))
					nhf016EndCameraLease(lease, nhf016LeaseExpired)
				}
				r.handler.mu.Unlock()
			case nhf016CameraMissingImage, nhf016CameraWrongCamera, nhf016CameraRestoreImages, nhf016CameraChangeEvents:
				entity, err := nhf016CameraID(command.Entity)
				if err != nil {
					t.Fatal(err)
				}
				r.handler.mu.Lock()
				record := r.handler.records[entity]
				if record == nil {
					r.handler.mu.Unlock()
					t.Fatal("unknown synthetic camera")
				}
				switch command.Action {
				case nhf016CameraMissingImage:
					record.imageState = nhf016ImageMissing
				case nhf016CameraWrongCamera:
					record.imageState = nhf016ImageWrongCamera
				case nhf016CameraRestoreImages:
					record.imageState = nhf016ImageAvailable
				case nhf016CameraChangeEvents:
					record.revision++
					close(record.changed)
					record.changed = make(chan struct{})
				default:
					r.handler.mu.Unlock()
					t.Fatal("wrong targeted camera control")
				}
				r.handler.mu.Unlock()
			case nhf016CameraOutage:
				r.handler.mu.Lock()
				r.handler.offline = true
				r.handler.mu.Unlock()
				r.cameraServer.CloseClientConnections()
			case nhf016CameraRestore:
				r.handler.mu.Lock()
				r.handler.offline = false
				r.handler.mu.Unlock()
			case nhf016CameraSnapshot:
				r.handler.mu.Lock()
				leases := len(r.handler.leases)
				r.handler.mu.Unlock()
				nhf003WritePrivate(t, out, fmt.Sprintf("stats-%02d.json", command.Sequence), map[string]any{"image_requests": r.handler.imageRequests.Load(), "active_reads": r.handler.activeReads.Load(), "active_streams": r.handler.activeStreams.Load(), "retained_leases": leases})
			default:
				t.Fatal("unparsed camera control")
			}
			sequence = command.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", sequence), command)
		}
	}
}
