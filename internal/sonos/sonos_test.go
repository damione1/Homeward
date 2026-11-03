package sonos

import (
	"context"
	"html"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

const testUID = "RINCON_00112233445501400"

func testPlayer() PlayerInput {
	return PlayerInput{Address: "192.168.50.10", UID: testUID, Alias: "media_player.demo", Label: "Demo"}
}
func TestSonosSettingsRejectUnsafeDestinationsAndAmbiguity(t *testing.T) {
	p := testPlayer()
	if _, e := ParseSettings(Input{Instance: "demo-sonos", Players: []PlayerInput{p}}); e != nil {
		t.Fatal(e)
	}
	for _, address := range []string{"example.test", "8.8.8.8", "127.0.0.1", "169.254.169.254", "192.168.50.10:1400", "192.168.50.10/path", "user@192.168.50.10", "::1"} {
		p := testPlayer()
		p.Address = address
		if _, e := ParseSettings(Input{Instance: "demo-sonos", Players: []PlayerInput{p}}); e == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	for _, mutate := range []func(*PlayerInput){func(p *PlayerInput) { p.UID = "RINCON_invalid" }, func(p *PlayerInput) { p.Alias = "switch.demo" }, func(p *PlayerInput) { p.Label = "bad\x00label" }} {
		p := testPlayer()
		mutate(&p)
		if _, e := ParseSettings(Input{Instance: "demo-sonos", Players: []PlayerInput{p}}); e == nil {
			t.Fatal("invalid player admitted")
		}
	}
	if _, e := ParseSettings(Input{Instance: "demo-sonos", Players: []PlayerInput{p, p}}); e == nil {
		t.Fatal("duplicate player admitted")
	}
}

type readTransport struct {
	mu                                     sync.Mutex
	calls                                  []string
	wrongUID, unknownCoordinator, oversize bool
}

func (r *readTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Host != "192.168.50.10:1400" {
		return nil, ErrUnavailable
	}
	action := req.Header.Get("SOAPACTION")
	r.calls = append(r.calls, req.Method+" "+req.URL.Path+" "+action)
	var body string
	if req.Method == http.MethodGet && req.URL.Path == "/xml/device_description.xml" {
		uid := testUID
		if r.wrongUID {
			uid = "RINCON_99887766554401400"
		}
		body = "<root><device><UDN>uuid:" + uid + "</UDN><deviceList><device><UDN>uuid:" + uid + "_MS</UDN></device><device><UDN>uuid:" + uid + "_MR</UDN></device></deviceList></device></root>"
		if r.oversize {
			body = strings.Repeat("x", maxXML+1)
		}
	} else {
		if req.Method != http.MethodPost {
			return nil, ErrUnavailable
		}
		var expectedPath string
		var response string
		switch action {
		case `"urn:schemas-upnp-org:service:ZoneGroupTopology:1#GetZoneGroupState"`:
			expectedPath = "/ZoneGroupTopology/Control"
			coordinator := testUID
			if r.unknownCoordinator {
				coordinator = "RINCON_99887766554401400"
			}
			response = "<ZoneGroupState>" + html.EscapeString(`<ZoneGroupState><ZoneGroups><ZoneGroup Coordinator="`+coordinator+`"><ZoneGroupMember UUID="`+testUID+`"/></ZoneGroup></ZoneGroups></ZoneGroupState>`) + "</ZoneGroupState>"
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetVolume"`:
			expectedPath = "/MediaRenderer/RenderingControl/Control"
			response = "<CurrentVolume>7</CurrentVolume>"
		case `"urn:schemas-upnp-org:service:AVTransport:1#GetTransportInfo"`:
			expectedPath = "/MediaRenderer/AVTransport/Control"
			response = "<CurrentTransportState>PLAYING</CurrentTransportState>"
		case `"urn:schemas-upnp-org:service:AVTransport:1#GetPositionInfo"`:
			expectedPath = "/MediaRenderer/AVTransport/Control"
			response = "<RelTime>00:00:12</RelTime><TrackDuration>00:03:00</TrackDuration><TrackMetaData>" + html.EscapeString(`<DIDL-Lite><item><title>Demo track</title><creator>Demo artist</creator><album>Demo album</album><albumArtURI>http://169.254.169.254/never-follow</albumArtURI></item></DIDL-Lite>`) + "</TrackMetaData>"
		default:
			return nil, ErrUnavailable
		}
		if req.URL.Path != expectedPath {
			return nil, ErrUnavailable
		}
		if _, e := io.ReadAll(req.Body); e != nil {
			return nil, e
		}
		body = `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><Response>` + response + `</Response></s:Body></s:Envelope>`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestSonosNativeReadsAreBoundedReadOnlyAndExpire(t *testing.T) {
	r, e := NewRuntime()
	if e != nil {
		t.Fatal(e)
	}
	entity, e := domain.ParseEntityID("demo-sonos-player")
	if e != nil {
		t.Fatal(e)
	}
	catalog, e := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if e != nil {
		t.Fatal(e)
	}
	description := r.description("Demo")
	if e = r.RegisterCodec(catalog); e != nil {
		t.Fatal(e)
	}
	if e = catalog.RegisterDriverCapability(entity, description); e != nil {
		t.Fatal(e)
	}
	if description.GetExtension().GetActionMessage() != "home.media.v1.MediaAction" || description.SchemaId != "home.media.v1" {
		t.Fatal("native player does not advertise the common media action codec")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &pilot{runtime: r, ctx: ctx, cancel: cancel}
	fake := &readTransport{}
	reader := &reader{client: &http.Client{Transport: fake}}
	players := []PlayerInput{testPlayer()}
	ids := map[string]domain.EntityID{testUID: entity}
	p.poll(ctx, reader, players, ids)
	s, ok := r.Snapshot(entity)
	if !ok {
		t.Fatal("native observation unavailable")
	}
	if s.GetVolumeBasisPoints() != 700 || s.GetTitle() != "Demo track" || s.GetPositionMillis() != 12000 || s.GetDurationMillis() != 180000 || s.Route.CoordinatorEntityId != "demo-sonos-player" || s.Artwork != nil {
		t.Fatalf("wrong native snapshot: %v", s)
	}
	if s.ActionAccess.String() != "ACTION_ACCESS_DENIED" || len(s.Capabilities.Operations) != 3 || s.Capabilities.GetVolumeBasisPoints().GetStep() != 100 {
		t.Fatal("standalone capabilities or default access are incorrect")
	}
	if len(s.Route.AdmissionEpoch) != 32 {
		t.Fatal("invalid media facade epoch")
	}
	if len(fake.calls) != 5 {
		t.Fatalf("unexpected requests: %v", fake.calls)
	}
	s.Title = proto.String("mutated")
	again, _ := r.Snapshot(entity)
	if again.GetTitle() != "Demo track" {
		t.Fatal("snapshot aliases cache")
	}
	r.mu.Lock()
	record := r.records[entity]
	record.captured = time.Now().Add(-46 * time.Second)
	r.records[entity] = record
	r.mu.Unlock()
	if _, ok = r.Snapshot(entity); ok {
		t.Fatal("expired data presented as fresh")
	}
	p.poll(ctx, reader, players, ids)
	fake.mu.Lock()
	fake.wrongUID = true
	fake.mu.Unlock()
	p.poll(ctx, reader, players, ids)
	if _, ok = r.Snapshot(entity); ok {
		t.Fatal("wrong device identity retained a fresh observation")
	}
	fake.mu.Lock()
	fake.wrongUID = false
	fake.unknownCoordinator = true
	fake.mu.Unlock()
	p.poll(ctx, reader, players, ids)
	if _, ok = r.Snapshot(entity); ok {
		t.Fatal("unconfigured coordinator admitted")
	}
	fake.mu.Lock()
	fake.unknownCoordinator = false
	fake.oversize = true
	fake.mu.Unlock()
	p.poll(ctx, reader, players, ids)
	if _, ok = r.Snapshot(entity); ok {
		t.Fatal("oversized response admitted")
	}
	fake.mu.Lock()
	fake.oversize = false
	fake.mu.Unlock()
	p.poll(ctx, reader, players, ids)
	cancel()
	if _, ok = r.Snapshot(entity); ok {
		t.Fatal("cancelled generation retained readable data")
	}
	p.clear()
}
func TestSonosXMLRejectsDuplicateFieldsDirectivesAndDeepNesting(t *testing.T) {
	for _, raw := range []string{`<root><device><UDN>a</UDN><UDN>b</UDN></device></root>`, `<root><device><UDN>a</UDN></device><device><UDN>b</UDN></device></root>`} {
		if _, e := deviceUDN([]byte(raw)); e == nil {
			t.Fatal("ambiguous physical identity admitted")
		}
	}
	for _, raw := range []string{`<!DOCTYPE x [<!ENTITY e SYSTEM "file:///private">]><x/>`, `<r><CurrentVolume>1</CurrentVolume><CurrentVolume>2</CurrentVolume></r>`, strings.Repeat("<x>", 25) + strings.Repeat("</x>", 25), `<x>`, `<x/>junk`, `junk<x/>`, `<x/><x/>`, `<r><CurrentVolume>7<x>ignored</x></CurrentVolume></r>`, `<r><CurrentVolume>7<!DOCTYPE x></CurrentVolume></r>`, `<r><CurrentVolume>7<CurrentVolume>8</CurrentVolume></CurrentVolume></r>`} {
		if _, e := fields([]byte(raw), "CurrentVolume"); e == nil {
			t.Fatal("invalid XML admitted")
		}
	}
	for _, raw := range []string{"NOT_IMPLEMENTED", "999999999999999:00:00", "01:99:00", "00:00:99", "168:00:01"} {
		if millis(raw) != nil {
			t.Fatal("invalid duration admitted")
		}
	}
}

// A real regrouping can be observed at different times by different players.
// No network is used; every response below passes through the production parser.
type groupedTransport struct {
	groups map[string]string
}

func (g groupedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	const secondUID = "RINCON_00112233446601400"
	uid := testUID
	if req.URL.Host == "192.168.50.11:1400" {
		uid = secondUID
	} else if req.URL.Host != "192.168.50.10:1400" {
		return nil, ErrUnavailable
	}
	copy := req.Clone(req.Context())
	urlCopy := *req.URL
	urlCopy.Host = "192.168.50.10:1400"
	copy.URL = &urlCopy
	response, err := (&readTransport{}).RoundTrip(copy)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	body := string(raw)
	if req.URL.Path == "/xml/device_description.xml" {
		body = strings.ReplaceAll(body, testUID, uid)
	}
	if strings.Contains(req.Header.Get("SOAPACTION"), "#GetZoneGroupState") {
		body = "<Response><ZoneGroupState>" + html.EscapeString("<ZoneGroupState><ZoneGroups>"+g.groups[uid]+"</ZoneGroups></ZoneGroupState>") + "</ZoneGroupState></Response>"
	}
	response.Body = io.NopCloser(strings.NewReader(body))
	return response, nil
}
func TestSonosRejectsContradictoryAndDuplicateGroupMembership(t *testing.T) {
	const secondUID = "RINCON_00112233446601400"
	groupXML := func(coordinator string, members ...string) string {
		raw := `<ZoneGroup Coordinator="` + coordinator + `">`
		for _, uid := range members {
			raw += `<ZoneGroupMember UUID="` + uid + `"/>`
		}
		return raw + "</ZoneGroup>"
	}
	consistent := groupXML(secondUID, testUID, secondUID)
	cases := []struct {
		name, first, second string
		wantFirst           bool
	}{
		{"same group in different order", consistent, groupXML(secondUID, secondUID, testUID), true},
		{"coordinator reports another group", consistent, groupXML(secondUID, secondUID), false},
		{"duplicate other member", groupXML(secondUID, testUID, secondUID, secondUID), consistent, false},
		{"coordinator missing from members", groupXML(secondUID, testUID), consistent, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime, err := NewRuntime()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			firstID, _ := domain.ParseEntityID("group-player-a")
			secondID, _ := domain.ParseEntityID("group-player-b")
			first := testPlayer()
			second := PlayerInput{Address: "192.168.50.11", UID: secondUID, Alias: "media_player.second", Label: "Second"}
			p := &pilot{runtime: runtime, ctx: ctx, cancel: cancel}
			reader := &reader{client: &http.Client{Transport: groupedTransport{groups: map[string]string{testUID: tc.first, secondUID: tc.second}}}}
			p.poll(ctx, reader, []PlayerInput{first, second}, map[string]domain.EntityID{testUID: firstID, secondUID: secondID})
			if _, ok := runtime.Snapshot(firstID); ok != tc.wantFirst {
				t.Fatalf("first snapshot present=%v want %v", ok, tc.wantFirst)
			}
		})
	}
}
