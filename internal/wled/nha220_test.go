package wled

import (
	"context"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA220ControllerAndSegmentCommandsDoNotAliasEndpoints(t *testing.T) {
	r := newRoutesRig(t, []RouteInput{{Alias: "light.synthetic_master", Label: "Synthetic master"}, {Alias: "light.synthetic_segment", Label: "Synthetic segment", Segment: proto.Uint32(0)}})
	var master, segment route
	for _, route := range r.c.routes {
		if route.input.Segment == nil {
			master = route
		} else {
			segment = route
		}
	}
	device, masterID, _ := master.binding.IDs()
	segmentDevice, segmentID, _ := segment.binding.IDs()
	if device != segmentDevice || masterID == segmentID {
		t.Fatal("fixture does not share one physical device with distinct entities")
	}
	send := func(id string, route route, args *actions.SetLight) {
		t.Helper()
		request := proto.Clone(r.request).(*actions.CommandRequest)
		request.CommandId = id
		request.Endpoint = route.description.Endpoint
		_, entity, _ := route.binding.IDs()
		name, _ := entity.Value()
		request.Target = &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: name}}
		request.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: args}}
		request.ExpiresAt = timestamppb.New(time.Now().Add(time.Second))
		response, err := r.service.Dispatch(context.Background(), r.actor, request)
		if err != nil || response.Error != nil {
			t.Fatalf("native dual-route dispatch failed: %v %v", response, err)
		}
		commandID, _ := domain.ParseCommandID(id)
		status, err := r.host.ActionStatus(commandID)
		if err != nil || status.Status != actions.ActionStatus_ACTION_STATUS_CONFIRMED {
			t.Fatalf("not confirmed %v %v", status, err)
		}
	}
	send("master-preset", master, &actions.SetLight{Preset: proto.String("2")})
	if r.transport.writes != 1 || len(r.transport.bodies[0]) != 1 || r.transport.bodies[0]["ps"] != float64(2) {
		t.Fatal("preset was fanned out or changed segment/master fields")
	}
	before := *r.transport.d.State.Brightness
	send("segment-rgb", segment, &actions.SetLight{Rgb: &registry.RgbColor{Red: proto.Uint32(12), Green: proto.Uint32(34), Blue: proto.Uint32(56)}})
	if r.transport.writes != 2 || *r.transport.d.State.Brightness != before {
		t.Fatal("segment write changed master or duplicated transport")
	}
	body := r.transport.bodies[1]
	if len(body) != 1 {
		t.Fatal("segment write contains implicit master fields")
	}
	segments, ok := body["seg"].([]any)
	if !ok || len(segments) != 1 {
		t.Fatal("segment write fanned out")
	}
	exact := segments[0].(map[string]any)
	if len(exact) != 2 || exact["id"] != float64(0) {
		t.Fatal("segment selector/fields changed")
	}
	colors := exact["col"].([]any)
	color := colors[0].([]any)
	if len(colors) != 1 || len(color) != 4 || color[0] != float64(12) || color[1] != float64(34) || color[2] != float64(56) || color[3] != float64(0) {
		t.Fatal("RGB payload was not exact")
	}

	send("segment-off", segment, &actions.SetLight{On: proto.Bool(false)})
	if r.transport.writes != 3 || len(r.transport.bodies[2]) != 1 {
		t.Fatal("segment power wrote implicit root fields")
	}
	power := r.transport.bodies[2]["seg"].([]any)
	if len(power) != 1 || len(power[0].(map[string]any)) != 2 || power[0].(map[string]any)["id"] != float64(0) || power[0].(map[string]any)["on"] != false {
		t.Fatal("segment power selector changed")
	}
	send("segment-level-on", segment, &actions.SetLight{On: proto.Bool(true), BrightnessPercent: proto.Float64(50)})
	if r.transport.writes != 4 || len(r.transport.bodies[3]) != 1 || *r.transport.d.State.Brightness != before || !*r.transport.d.State.On {
		t.Fatal("segment level changed master or root fields")
	}
	levels := r.transport.bodies[3]["seg"].([]any)
	if len(levels) != 1 {
		t.Fatal("level fanned out")
	}
	level := levels[0].(map[string]any)
	if len(level) != 3 || level["id"] != float64(0) || level["on"] != true || level["bri"] != float64(128) {
		t.Fatal("level was not quantized on the exact segment")
	}
	if master.description.Endpoint != "controller" || segment.description.Endpoint != "segment-0" {
		t.Fatal("route endpoint contract drifted")
	}
	for i, endpoint := range []string{"light", "segment-0"} {
		request := proto.Clone(r.request).(*actions.CommandRequest)
		request.CommandId = []string{"retired-shared-endpoint", "wrong-route-endpoint"}[i]
		request.Endpoint = endpoint
		response, err := r.service.Dispatch(context.Background(), r.actor, request)
		if err == nil && (response == nil || response.Error == nil) {
			t.Fatal("foreign/shared endpoint admitted")
		}
		if r.transport.writes != 4 {
			t.Fatal("invalid endpoint reached HTTP transport")
		}
	}

}
