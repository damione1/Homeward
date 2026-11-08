package wled

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"google.golang.org/protobuf/proto"
)

const sample = `{"state":{"on":true,"bri":128,"ps":2,"seg":[{"id":0,"start":0,"stop":30,"on":true,"bri":200,"col":[[1,2,3,0]],"lc":1,"fx":0,"pal":0}]},"info":{"mac":"aabbccddeeff","ver":"0.15.0"},"effects":["Solid","Blink"],"palettes":["Default"]}`

func doc(t *testing.T) document {
	t.Helper()
	var d document
	if json.Unmarshal([]byte(sample), &d) != nil || !d.valid("aabbccddeeff") {
		t.Fatal("fixture")
	}
	return d
}
func TestNHA083IdentityAbsentFieldsAndCapabilities(t *testing.T) {
	d := doc(t)
	if d.valid("112233445566") {
		t.Fatal("identity mismatch")
	}
	d.State.On = nil
	if d.valid("aabbccddeeff") {
		t.Fatal("missing power inferred")
	}
	d = doc(t)
	r := RouteInput{Label: "Demo", Segment: proto.Uint32(0)}
	if !description(r, d, nil).GetLight().Rgb {
		t.Fatal("RGB absent")
	}
	d.State.Segments[0].Capabilities = nil
	if description(r, d, nil).GetLight().Rgb {
		t.Fatal("RGB guessed from color array")
	}
	if description(RouteInput{Label: "Master"}, d, nil).GetLight().Rgb {
		t.Fatal("controller advertises segment RGB")
	}
}
func TestNHA083SettingsNoDuplicateHardwareOrRoutes(t *testing.T) {
	input := Input{Instance: "demo-wled", Controllers: []ControllerInput{{Address: "192.168.50.20", MAC: "aabbccddeeff", Routes: []RouteInput{{Alias: "light.demo", Label: "Demo"}, {Alias: "light.demo_segment", Label: "Segment", Segment: proto.Uint32(0)}}}}}
	s, e := ParseSettings(input)
	if e != nil {
		t.Fatal(e)
	}
	*input.Controllers[0].Routes[1].Segment = 2
	if *s.Controllers()[0].Routes[1].Segment != 0 {
		t.Fatal("mutable settings")
	}
	input.Controllers = append(input.Controllers, input.Controllers[0])
	if _, e = ParseSettings(input); e == nil {
		t.Fatal("duplicate hardware")
	}
	input.Controllers = input.Controllers[:1]
	input.Controllers[0].Address = "127.0.0.1"
	if _, e = ParseSettings(input); e == nil {
		t.Fatal("loopback admitted")
	}
}
func TestNHA083ExactSegmentControllerAndPresetPayloads(t *testing.T) {
	d := doc(t)
	r := route{input: RouteInput{Segment: proto.Uint32(0)}}
	r.description = description(r.input, d, nil)
	body, e := payload(r, d, &actions.SetLight{On: proto.Bool(true), BrightnessPercent: proto.Float64(50), Rgb: &registry.RgbColor{Red: proto.Uint32(1), Green: proto.Uint32(2), Blue: proto.Uint32(3)}})
	if e != nil {
		t.Fatal(e)
	}
	var decoded map[string]json.RawMessage
	_ = json.Unmarshal(body, &decoded)
	if len(decoded) != 1 || decoded["seg"] == nil {
		t.Fatalf("segment leaked into global state: %s", body)
	}
	if strings.Contains(string(body), "toggle") {
		t.Fatal("toggle")
	}
	d.State.Segments[0].Brightness = proto.Uint32(128)
	if !matches(r, d, &actions.SetLight{BrightnessPercent: proto.Float64(50)}) {
		t.Fatal("quantized readback rejected")
	}
	d.State.On = proto.Bool(false)
	if matches(r, d, &actions.SetLight{On: proto.Bool(true)}) {
		t.Fatal("off master confirmed on segment")
	}
	if _, e = payload(r, d, &actions.SetLight{Preset: proto.String("2")}); e == nil {
		t.Fatal("global preset through segment")
	}
	master := route{input: RouteInput{Label: "Master"}}
	master.description = description(master.input, d, &registry.ControlChoices{Choices: []*registry.ControlChoice{{Key: "2", DisplayName: "Evening"}}})
	body, e = payload(master, d, &actions.SetLight{Preset: proto.String("2")})
	if e != nil || string(body) != `{"ps":2}` {
		t.Fatalf("preset %s %v", body, e)
	}
	if _, e = payload(master, d, &actions.SetLight{Preset: proto.String("7")}); e == nil {
		t.Fatal("invented preset")
	}
}
func TestNHA083HTTPReadOnlyAndBoundedTransport(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "GET" {
			writes++
		}
		switch r.URL.Path {
		case "/json":
			_, _ = w.Write([]byte(sample))
		case "/presets.json":
			_, _ = w.Write([]byte(`{"0":{},"2":{"n":"Evening"},"7":{"n":"Focus"},"251":{"n":"Invalid"}}`))
		case "/redirect":
			http.Redirect(w, r, "/json", http.StatusFound)
		case "/oversize":
			_, _ = w.Write([]byte(strings.Repeat(" ", maxJSON+1)))
		}
	}))
	defer server.Close()
	c := newClient()
	defer c.http.CloseIdleConnections()
	address := strings.TrimPrefix(server.URL, "http://")
	if _, e := c.read(context.Background(), address, "aabbccddeeff"); e != nil {
		t.Fatal(e)
	}
	if got := c.presets(context.Background(), address); len(got.GetChoices()) != 2 || got.Choices[0].Key != "2" {
		t.Fatal(got)
	}
	var out any
	for _, path := range []string{"/redirect", "/oversize"} {
		if c.request(context.Background(), address, path, nil, &out) == nil {
			t.Fatal(path)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if writes != 0 {
		t.Fatal("read performs write")
	}
}
func TestNHA083WebsocketReconnectNeverSendsMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	messages := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.Error(w, "offline", 503)
			return
		}
		ws, e := websocket.Accept(w, r, nil)
		if e != nil {
			return
		}
		defer ws.CloseNow()
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(sample))
		_, b, e := ws.Read(ctx)
		if e == nil {
			messages <- b
		}
	}))
	defer server.Close()
	p := &pilot{ctx: ctx, client: newClient()}
	c := &controller{input: ControllerInput{Address: strings.TrimPrefix(server.URL, "http://"), MAC: "aabbccddeeff"}, gate: make(chan struct{}, 1)} // Hold reconciliation gate: exercise the subscription without a Host.
	c.gate <- struct{}{}
	_ = p.listen(ctx, c)
	select {
	case b := <-messages:
		t.Fatalf("unexpected client message %s", b)
	default:
	}
}
