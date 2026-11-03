package sonos

import (
	"bytes"
	"context"
	"encoding/xml"
	"strconv"
	"time"

	media "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SoCo v0.30.9 soco/core.py play/pause/stop/volume and services.py
// send_command establish the local AVTransport/RenderingControl mapping.
// Homeward rejects unsupported inputs rather than clamping, follows no group
// coordinator, and performs exactly one write with no physical replay/retry.
type mediaCodec struct{}

func (mediaCodec) Validate(message proto.Message) error {
	raw, err := proto.Marshal(message)
	if err != nil {
		return ErrUnavailable
	}
	_, err = decodeAction(raw)
	return err
}

func decodeAction(raw []byte) (*media.MediaAction, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, ErrUnavailable
	}
	a := &media.MediaAction{}
	if proto.Unmarshal(raw, a) != nil || len(a.ProtoReflect().GetUnknown()) != 0 || a.Route == nil || len(a.Route.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrUnavailable
	}
	r := a.Route
	if r.EntityId == "" || r.Endpoint != Endpoint || r.CoordinatorEntityId != r.EntityId || r.TopologyRevision == 0 || r.AdmissionEpoch == "" || r.ConfigRevision == 0 {
		return nil, ErrUnavailable
	}
	switch v := a.Command.(type) {
	case *media.MediaAction_Operation:
		if v.Operation != media.Operation_OPERATION_PLAY && v.Operation != media.Operation_OPERATION_PAUSE && v.Operation != media.Operation_OPERATION_STOP {
			return nil, ErrUnavailable
		}
	case *media.MediaAction_VolumeBasisPoints:
		if v.VolumeBasisPoints > 10000 || v.VolumeBasisPoints%100 != 0 {
			return nil, ErrUnavailable
		}
	default:
		return nil, ErrUnavailable
	}
	return a, nil
}

func (p *pilot) CommandDriver() integrations.CommandDriver { return p }

var _ integrations.SupervisedCommandPilot = (*pilot)(nil)

func (p *pilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrUnavailable
	}
	_, entity, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	ext := request.GetArguments().GetExtension()
	if request.CapabilitySchemaId != Schema || request.Endpoint != Endpoint || ext == nil || ext.SchemaId != Schema || ext.Major != 1 || !bytes.Equal(ext.DescriptorSha256, p.runtime.digest[:]) || request.ExpiresAt == nil || request.ExpiresAt.CheckValid() != nil || !time.Now().Before(request.ExpiresAt.AsTime()) {
		return ErrUnavailable
	}
	action, err := decodeAction(ext.ProtobufPayload)
	entityText, _ := entity.Value()
	if err != nil || action.Route.EntityId != entityText || action.Route.AdmissionEpoch != request.AdmissionEpoch || action.Route.ConfigRevision != request.ConfigRevision {
		return ErrUnavailable
	}
	p.mu.Lock()
	player, configured := p.players[entity]
	r, parent := p.reader, p.ctx
	p.mu.Unlock()
	if !configured || r == nil || parent == nil || parent.Err() != nil || ctx.Err() != nil {
		return ErrUnavailable
	}
	budget, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(parent, cancel)
	defer stop()
	// Waits are cancellable; nothing is retained for a restart or detached
	// from this dispatch. Polling shares the gate to avoid publishing a read
	// begun before a mutation after that mutation's fresh readback.
	release, acquired := p.lockCommands(budget)
	if !acquired {
		return ErrUnavailable
	}
	defer release()
	// A queued command may have expired while another command was completing.
	current := func() bool {
		if budget.Err() != nil || !time.Now().Before(request.ExpiresAt.AsTime()) || !command.ReadyForTransport() {
			return false
		}
		snapshot, ok := p.runtime.Snapshot(entity)
		return ok && snapshot.Route != nil && snapshot.Route.EntityId == action.Route.EntityId && snapshot.Route.Endpoint == action.Route.Endpoint && snapshot.Route.CoordinatorEntityId == action.Route.CoordinatorEntityId && snapshot.Route.TopologyRevision == action.Route.TopologyRevision && len(snapshot.Members) == 1 && snapshot.Members[0].EntityId == entityText
	}
	if !current() {
		return ErrUnavailable
	}
	before, err := r.identityTopology(budget, player)
	if err != nil || !standalone(before, player.UID) {
		p.invalidateObservation(budget, entity)
		return ErrUnavailable
	}
	service, name, args := "AVTransport", "", "<InstanceID>0</InstanceID>"
	switch v := action.Command.(type) {
	case *media.MediaAction_Operation:
		switch v.Operation {
		case media.Operation_OPERATION_PLAY:
			name, args = "Play", args+"<Speed>1</Speed>"
		case media.Operation_OPERATION_PAUSE:
			name = "Pause"
		case media.Operation_OPERATION_STOP:
			name = "Stop"
		}
	case *media.MediaAction_VolumeBasisPoints:
		service, name = "RenderingControl", "SetVolume"
		args += "<Channel>Master</Channel><DesiredVolume>" + strconv.FormatUint(uint64(v.VolumeBasisPoints/100), 10) + "</DesiredVolume>"
	}
	raw, err := r.requestFenced(budget, player.Address, "/MediaRenderer/"+service+"/Control", service, name, args, current)
	if err != nil || !validACK(raw, service, name) {
		p.invalidateObservation(budget, entity)
		return ErrUnavailable
	}
	if err := command.Acknowledge(); err != nil {
		return err
	}
	// A SOAP ACK is only acknowledgement. Only a fresh, matching real readback
	// from the same verified standalone player can confirm this exact action.
	for {
		if !current() {
			return nil
		}
		after, err := r.read(budget, player)
		if err != nil || !standalone(after, player.UID) {
			p.invalidateObservation(budget, entity)
			return nil
		}
		playback, err := r.transport(budget, player)
		if err != nil {
			p.invalidateObservation(budget, entity)
			return nil
		}
		after.playback = playback.playback
		finalTopology, err := r.identityTopology(budget, player)
		if err != nil || !standalone(finalTopology, player.UID) {
			p.invalidateObservation(budget, entity)
			return nil
		}
		if !current() {
			return nil
		}
		p.runtime.mu.Lock()
		cached, present := p.runtime.records[entity]
		if present && cached.owner == p && cached.snapshot.Route.TopologyRevision == action.Route.TopologyRevision && parent.Err() == nil {
			cached.snapshot = proto.Clone(cached.snapshot).(*media.Snapshot)
			cached.snapshot.VolumeBasisPoints = proto.Uint32(after.volume)
			cached.snapshot.Title, cached.snapshot.Artist, cached.snapshot.Collection = playback.title, playback.artist, playback.collection
			cached.snapshot.PositionMillis, cached.snapshot.DurationMillis = playback.position, playback.duration
			cached.snapshot.Playback = media.PlaybackState_PLAYBACK_STATE_UNSPECIFIED
			switch after.playback {
			case "TRANSITIONING":
				cached.snapshot.Playback = media.PlaybackState_PLAYBACK_STATE_BUFFERING
			case "PLAYING":
				cached.snapshot.Playback = media.PlaybackState_PLAYBACK_STATE_PLAYING
			case "PAUSED_PLAYBACK":
				cached.snapshot.Playback = media.PlaybackState_PLAYBACK_STATE_PAUSED
			case "STOPPED", "NO_MEDIA_PRESENT":
				cached.snapshot.Playback = media.PlaybackState_PLAYBACK_STATE_STOPPED
			}
			cached.captured = time.Now()
			cached.snapshot.ReceivedAt = timestamppb.New(cached.captured)
			p.runtime.records[entity] = cached
		}
		p.runtime.mu.Unlock()
		if p.host != nil {
			_ = publishVolume(budget, p.host, p.bindings[entity], &after.volume, time.Now())
		}
		if matchesAction(action, after) {
			if !current() {
				return nil
			}
			return command.ConfirmProtocol(readbackProof{action: action, observed: after, uid: player.UID})
		}
		if _, transport := action.Command.(*media.MediaAction_Operation); !transport || after.playback != "TRANSITIONING" {
			return nil
		}
		// Sonos may acknowledge transport before reaching its stable state. Only
		// this observed transition permits more reads; the single POST above is
		// never repeated. The original deadline and session cancellation bound all
		// waiting, and every iteration rechecks UID/topology and the Host fence.
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-budget.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (p *pilot) invalidateObservation(ctx context.Context, entity domain.EntityID) {
	p.runtime.mu.Lock()
	if cached, present := p.runtime.records[entity]; present && cached.owner == p {
		delete(p.runtime.records, entity)
	}
	p.runtime.mu.Unlock()
	if p.host != nil {
		_ = publishVolume(ctx, p.host, p.bindings[entity], nil, time.Now())
	}
}

func (p *pilot) lockCommands(ctx context.Context) (func(), bool) {
	p.mu.Lock()
	if p.commands == nil {
		p.commands = make(chan struct{}, 1)
	}
	gate := p.commands
	p.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, true
	case <-ctx.Done():
		return nil, false
	}
}

func standalone(o observation, uid string) bool {
	return o.commandSafe && o.coordinator == uid && len(o.members) == 1 && o.members[0] == uid
}

func matchesAction(a *media.MediaAction, o observation) bool {
	switch v := a.Command.(type) {
	case *media.MediaAction_VolumeBasisPoints:
		return v.VolumeBasisPoints == o.volume
	case *media.MediaAction_Operation:
		switch v.Operation {
		case media.Operation_OPERATION_PLAY:
			return o.playback == "PLAYING"
		case media.Operation_OPERATION_PAUSE:
			return o.playback == "PAUSED_PLAYBACK"
		case media.Operation_OPERATION_STOP:
			return o.playback == "STOPPED" || o.playback == "NO_MEDIA_PRESENT"
		}
	}
	return false
}

type readbackProof struct {
	action   *media.MediaAction
	observed observation
	uid      string
}

func (p readbackProof) QualifyProtocolConfirmation(command integrations.AdmittedCommand) error {
	r, err := command.Request()
	if err != nil || r.GetArguments().GetExtension() == nil {
		return ErrUnavailable
	}
	a, err := decodeAction(r.GetArguments().GetExtension().ProtobufPayload)
	if err != nil || !proto.Equal(a, p.action) || !standalone(p.observed, p.uid) || !matchesAction(a, p.observed) {
		return ErrUnavailable
	}
	return nil
}

func validACK(raw []byte, service, action string) bool {
	if len(raw) == 0 {
		return false
	}
	if _, err := fields(raw); err != nil {
		return false
	}
	var envelope struct {
		XMLName xml.Name `xml:"Envelope"`
		Bodies  []struct {
			XMLName xml.Name
			Text    string `xml:",chardata"`
			Entries []struct {
				XMLName xml.Name
				Content string `xml:",innerxml"`
			} `xml:",any"`
		} `xml:"Body"`
	}
	return xml.Unmarshal(raw, &envelope) == nil && envelope.XMLName.Space == "http://schemas.xmlsoap.org/soap/envelope/" && len(envelope.Bodies) == 1 && envelope.Bodies[0].XMLName.Space == envelope.XMLName.Space && len(bytes.TrimSpace([]byte(envelope.Bodies[0].Text))) == 0 && len(envelope.Bodies[0].Entries) == 1 && envelope.Bodies[0].Entries[0].XMLName.Local == action+"Response" && envelope.Bodies[0].Entries[0].XMLName.Space == "urn:schemas-upnp-org:service:"+service+":1" && len(bytes.TrimSpace([]byte(envelope.Bodies[0].Entries[0].Content))) == 0
}

func publishVolume(ctx context.Context, host integrations.SessionHost, binding integrations.Binding, volume *uint32, at time.Time) error {
	availability, freshness := domain.AvailabilityUnavailable, domain.FreshnessStale
	var value *domain.StateValue
	var observedAt *time.Time
	if volume != nil {
		availability, freshness = domain.AvailabilityAvailable, domain.FreshnessFresh
		unit, err := domain.NewUnitCatalog().Parse("%")
		if err != nil {
			return err
		}
		measurement, err := domain.NewMeasurement(float64(*volume)/100, unit, 0, domain.SemanticMeasurement, domain.AggregationCurrent)
		if err != nil {
			return err
		}
		v, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			return err
		}
		value, observedAt = &v, &at
	}
	quality, err := domain.NewQuality(availability, freshness, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	observation, err := integrations.NewStateObservation(VolumeEndpoint, quality, value, observedAt, at, nil)
	if err != nil {
		return err
	}
	return host.Publish(ctx, binding, observation)
}
