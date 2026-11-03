package sonos

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxXML = 256 << 10

// This closed transport has no exported action or URL entry point.
// URLs in topology, track metadata and artwork are never followed.
type reader struct{ client *http.Client }

func newReader() *reader {
	t := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext, ResponseHeaderTimeout: time.Second, MaxConnsPerHost: 2, MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second}
	return &reader{&http.Client{Transport: t, Timeout: 1500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnavailable }}}
}
func (r *reader) close() { r.client.CloseIdleConnections() }
func (r *reader) request(ctx context.Context, ip, path, service, action, arguments string) ([]byte, error) {
	return r.requestFenced(ctx, ip, path, service, action, arguments, nil)
}
func (r *reader) requestFenced(ctx context.Context, ip, path, service, action, arguments string, fence func() bool) ([]byte, error) {
	// The package exposes neither an arbitrary UPnP action nor an unfenced
	// mutation. Queue/group/source/EQ operations have no transport entry here.
	wantService, wantPath, mutation := "", "/xml/device_description.xml", false
	switch action {
	case "":
	case "GetZoneGroupState":
		wantService, wantPath = "ZoneGroupTopology", "/ZoneGroupTopology/Control"
	case "GetVolume", "SetVolume":
		wantService, wantPath = "RenderingControl", "/MediaRenderer/RenderingControl/Control"
		mutation = action == "SetVolume"
	case "GetTransportInfo", "GetPositionInfo", "Play", "Pause", "Stop":
		wantService, wantPath = "AVTransport", "/MediaRenderer/AVTransport/Control"
		mutation = action == "Play" || action == "Pause" || action == "Stop"
	default:
		return nil, ErrUnavailable
	}
	if service != wantService || path != wantPath || mutation && fence == nil {
		return nil, ErrUnavailable
	}
	method := http.MethodGet
	var body io.Reader
	if action != "" {
		method = http.MethodPost
		body = strings.NewReader(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:` + action + ` xmlns:u="urn:schemas-upnp-org:service:` + service + `:1">` + arguments + `</u:` + action + `></s:Body></s:Envelope>`)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+ip+":1400"+path, body)
	if err != nil {
		return nil, ErrUnavailable
	}
	if action != "" {
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("SOAPACTION", `"urn:schemas-upnp-org:service:`+service+`:1#`+action+`"`)
	}
	// In particular, never let net/http replay a mutation after a connection
	// failure. There is no idempotency header, redirect, application retry or
	// reconnect queue on this transport.
	req.GetBody = nil
	if ctx.Err() != nil || fence != nil && !fence() {
		return nil, ErrUnavailable
	}
	response, err := r.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrUnavailable
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, maxXML+1))
	if err != nil || len(b) > maxXML {
		return nil, ErrUnavailable
	}
	return b, nil
}

// Reject duplicate selected fields, excessive nesting/tokens and directives.
// Go's XML decoder does not resolve external entities.
func fields(raw []byte, names ...string) (map[string]string, error) {
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	result := map[string]string{}
	d := xml.NewDecoder(bytes.NewReader(raw))
	depth, count, roots := 0, 0, 0
	selected := ""
	var value strings.Builder
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		count++
		if e != nil || count > 12000 {
			return nil, ErrUnavailable
		}
		// Selected SOAP fields are scalars. Consume every token here as well:
		// DecodeElement would hide nested elements/directives from our bounds.
		if selected != "" {
			switch v := t.(type) {
			case xml.CharData:
				value.Write(v)
				if value.Len() > maxXML {
					return nil, ErrUnavailable
				}
			case xml.EndElement:
				result[selected] = value.String()
				selected = ""
				depth--
			case xml.Comment:
			default:
				return nil, ErrUnavailable
			}
			continue
		}
		switch v := t.(type) {
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(v)) != 0 {
				return nil, ErrUnavailable
			}
		case xml.Directive:
			return nil, ErrUnavailable
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return nil, ErrUnavailable
				}
			}
			depth++
			if depth > 24 {
				return nil, ErrUnavailable
			}
			if wanted[v.Name.Local] {
				if _, ok := result[v.Name.Local]; ok {
					return nil, ErrUnavailable
				}
				selected = v.Name.Local
				value.Reset()
			}
		case xml.EndElement:
			depth--
		}
	}
	if depth != 0 || roots != 1 {
		return nil, ErrUnavailable
	}
	return result, nil
}

type member struct {
	UID        string   `xml:"UUID,attr"`
	Invisible  string   `xml:"Invisible,attr"`
	Satellites []member `xml:"Satellite"`
}
type group struct {
	Coordinator string   `xml:"Coordinator,attr"`
	Members     []member `xml:"ZoneGroupMember"`
}
type topology struct {
	Groups []group `xml:"ZoneGroups>ZoneGroup"`
}
type observation struct {
	playback                  string
	volume                    uint32
	title, artist, collection *string
	position, duration        *uint64
	coordinator               string
	members                   []string
	commandSafe               bool
}

func millis(s string) *uint64 {
	p := strings.Split(s, ":")
	if len(p) != 3 {
		return nil
	}
	h, e := strconv.ParseUint(p[0], 10, 64)
	m, e2 := strconv.ParseUint(p[1], 10, 64)
	sec, e3 := strconv.ParseUint(p[2], 10, 64)
	if e != nil || e2 != nil || e3 != nil || m > 59 || sec > 59 || h > 168 {
		return nil
	}
	n := (h*3600 + m*60 + sec) * 1000
	if n > 7*24*60*60*1000 {
		return nil
	}
	return &n
}
func label(s string) *string {
	if s == "" || len(s) > 1024 {
		return nil
	}
	for _, r := range s {
		if r < 32 && r != '\n' || r == 127 {
			return nil
		}
	}
	return &s
}

func deviceUDN(raw []byte) (string, error) {
	if _, err := fields(raw); err != nil {
		return "", err
	}
	var root struct {
		XMLName xml.Name `xml:"root"`
		Devices []struct {
			UDNs []string `xml:"UDN"`
		} `xml:"device"`
	}
	if xml.Unmarshal(raw, &root) != nil || len(root.Devices) != 1 || len(root.Devices[0].UDNs) != 1 {
		return "", ErrUnavailable
	}
	// Embedded MediaServer/MediaRenderer UDNs identify services of this player,
	// not extra root players. Only root/device/UDN is the physical identity.
	return root.Devices[0].UDNs[0], nil
}
func (r *reader) read(ctx context.Context, p PlayerInput) (observation, error) {
	out, e := r.identityTopology(ctx, p)
	if e != nil {
		return out, e
	}
	b, e := r.request(ctx, p.Address, "/MediaRenderer/RenderingControl/Control", "RenderingControl", "GetVolume", "<InstanceID>0</InstanceID><Channel>Master</Channel>")
	if e != nil {
		return out, e
	}
	f, e := fields(b, "CurrentVolume")
	if e != nil {
		return out, e
	}
	v, e := strconv.ParseUint(f["CurrentVolume"], 10, 32)
	if e != nil || v > 100 {
		return out, ErrUnavailable
	}
	out.volume = uint32(v) * 100
	return out, nil
}

// identityTopology performs uncached reads against this exact configured IP.
// A mutation uses it immediately before the Host transport fence, and never
// follows a coordinator address or URL received from the network.
func (r *reader) identityTopology(ctx context.Context, p PlayerInput) (observation, error) {
	var out observation
	b, e := r.request(ctx, p.Address, "/xml/device_description.xml", "", "", "")
	if e != nil {
		return out, e
	}
	udn, e := deviceUDN(b)
	if e != nil || udn != "uuid:"+p.UID {
		return out, ErrUnavailable
	}
	b, e = r.request(ctx, p.Address, "/ZoneGroupTopology/Control", "ZoneGroupTopology", "GetZoneGroupState", "")
	if e != nil {
		return out, e
	}
	f, e := fields(b, "ZoneGroupState")
	if e != nil {
		return out, e
	}
	if _, e = fields([]byte(f["ZoneGroupState"])); e != nil {
		return out, e
	}
	var top topology
	if xml.Unmarshal([]byte(f["ZoneGroupState"]), &top) != nil || len(top.Groups) > 64 {
		return out, ErrUnavailable
	}
	found := false
	for _, g := range top.Groups {
		if len(g.Members) > 64 {
			return out, ErrUnavailable
		}
		for _, m := range g.Members {
			if m.UID == p.UID {
				if found {
					return out, ErrUnavailable
				}
				found = true
				out.coordinator = g.Coordinator
				// Invisible bonded units and satellites can still produce audio.
				// Display membership omits them, but write admission must not.
				out.commandSafe = g.Coordinator == p.UID && len(g.Members) == 1 && g.Members[0].UID == p.UID && g.Members[0].Invisible != "1" && len(g.Members[0].Satellites) == 0
				for _, n := range g.Members {
					if n.Invisible != "1" {
						out.members = append(out.members, n.UID)
					}
				}
			}
		}
	}
	if !found || len(out.members) == 0 || len(out.members) > 16 {
		return out, ErrUnavailable
	}
	sort.Strings(out.members)
	playerPresent, coordinatorPresent := false, false
	for i, uid := range out.members {
		if i > 0 && uid == out.members[i-1] {
			return out, ErrUnavailable
		}
		playerPresent = playerPresent || uid == p.UID
		coordinatorPresent = coordinatorPresent || uid == out.coordinator
	}
	if !playerPresent || !coordinatorPresent {
		return out, ErrUnavailable
	}
	// Followers' AVTransport data may refer to an obsolete local queue. The
	// polling cycle resolves the configured coordinator before reading playback.
	return out, nil
}
func (r *reader) transport(ctx context.Context, p PlayerInput) (observation, error) {
	var out observation
	b, e := r.request(ctx, p.Address, "/MediaRenderer/AVTransport/Control", "AVTransport", "GetTransportInfo", "<InstanceID>0</InstanceID>")
	if e != nil {
		return out, e
	}
	f, e := fields(b, "CurrentTransportState")
	if e != nil {
		return out, e
	}
	out.playback = f["CurrentTransportState"]
	b, e = r.request(ctx, p.Address, "/MediaRenderer/AVTransport/Control", "AVTransport", "GetPositionInfo", "<InstanceID>0</InstanceID>")
	if e != nil {
		return out, e
	}
	f, e = fields(b, "RelTime", "TrackDuration", "TrackMetaData")
	if e != nil {
		return out, e
	}
	out.position, out.duration = millis(f["RelTime"]), millis(f["TrackDuration"])
	if out.position != nil && out.duration != nil && *out.position > *out.duration {
		out.position = nil
	}
	meta := f["TrackMetaData"]
	if meta != "" && meta != "NOT_IMPLEMENTED" {
		m, err := fields([]byte(meta), "title", "creator", "album")
		if err == nil {
			out.title, out.artist, out.collection = label(m["title"]), label(m["creator"]), label(m["album"])
		}
	}
	return out, nil
}
