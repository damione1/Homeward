package wled

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
)

const maxJSON = 1 << 20

// Pointers retain absent fields: malformed/partial payloads never invent off.
type segment struct {
	ID           *uint32    `json:"id"`
	Start        *uint32    `json:"start"`
	Stop         *uint32    `json:"stop"`
	On           *bool      `json:"on"`
	Brightness   *uint32    `json:"bri"`
	Colors       [][]uint32 `json:"col"`
	Capabilities *uint32    `json:"lc"`
	Effect       *uint32    `json:"fx"`
	Palette      *uint32    `json:"pal"`
}
type lightState struct {
	On         *bool     `json:"on"`
	Brightness *uint32   `json:"bri"`
	Preset     *int      `json:"ps"`
	Segments   []segment `json:"seg"`
}
type info struct {
	MAC     string `json:"mac"`
	Version string `json:"ver"`
	Live    bool   `json:"live"`
	LEDs    struct {
		Capabilities []uint32 `json:"seglc"`
	} `json:"leds"`
}
type document struct {
	State    *lightState `json:"state"`
	Info     *info       `json:"info"`
	Effects  []string    `json:"effects"`
	Palettes []string    `json:"palettes"`
}
type preset struct {
	Name string `json:"n"`
}
type client struct{ http *http.Client }

func newClient() *client {
	return &client{http: &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *client) request(ctx context.Context, address, path string, payload []byte, out any) error {
	method := http.MethodGet
	var body io.Reader
	if payload != nil {
		method = http.MethodPost
		body = bytes.NewReader(payload)
	}
	req, e := http.NewRequestWithContext(ctx, method, "http://"+address+path, body)
	if e != nil {
		return ErrUnavailable
	}
	// No GetBody and no idempotency header: never retry an ambiguous POST.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	response, e := c.http.Do(req)
	if e != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	b, e := io.ReadAll(io.LimitReader(response.Body, maxJSON+1))
	if e != nil || len(b) > maxJSON || json.Unmarshal(b, out) != nil {
		return ErrUnavailable
	}
	return nil
}
func (c *client) read(ctx context.Context, address, mac string) (document, error) {
	var d document
	e := c.request(ctx, address, "/json", nil, &d)
	if e != nil || !d.valid(mac) {
		return document{}, ErrUnavailable
	}
	return d, nil
}
func (d document) valid(mac string) bool {
	if d.Info == nil || d.State == nil || strings.ToLower(strings.ReplaceAll(d.Info.MAC, ":", "")) != mac || d.Info.Version == "" || d.State.On == nil || d.State.Brightness == nil || *d.State.Brightness > 255 || len(d.State.Segments) > 32 {
		return false
	}
	seen := map[uint32]bool{}
	for _, s := range d.State.Segments {
		if s.ID == nil || *s.ID > 31 || seen[*s.ID] || s.On == nil || s.Brightness == nil || *s.Brightness > 255 {
			return false
		}
		seen[*s.ID] = true
	}
	return true
}
func (d document) find(id uint32) (segment, bool) {
	for _, s := range d.State.Segments {
		if s.ID != nil && *s.ID == id && s.Start != nil && s.Stop != nil && *s.Stop > *s.Start {
			return s, true
		}
	}
	return segment{}, false
}
func (d document) rgb(s segment) bool {
	if s.Capabilities != nil {
		return *s.Capabilities&1 != 0
	}
	id := int(*s.ID)
	return d.Info != nil && id < len(d.Info.LEDs.Capabilities) && d.Info.LEDs.Capabilities[id]&1 != 0
}
func (c *client) presets(ctx context.Context, address string) *registry.ControlChoices {
	var values map[string]json.RawMessage
	if c.request(ctx, address, "/presets.json", nil, &values) != nil {
		return nil
	}
	var ids []int
	for k, raw := range values {
		n, e := strconv.Atoi(k)
		var p preset
		if e == nil && n >= 1 && n <= 250 && strconv.Itoa(n) == k && json.Unmarshal(raw, &p) == nil && p.Name != "" && len(p.Name) <= 128 {
			ids = append(ids, n)
		}
	}
	sort.Ints(ids)
	result := &registry.ControlChoices{}
	for _, n := range ids {
		var p preset
		_ = json.Unmarshal(values[strconv.Itoa(n)], &p)
		result.Choices = append(result.Choices, &registry.ControlChoice{Key: strconv.Itoa(n), DisplayName: p.Name})
	}
	if len(ids) == 0 {
		return nil
	}
	return result
}
func description(r RouteInput, d document, presets *registry.ControlChoices) *registry.CapabilityDescription {
	light := &registry.LightConstraints{SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100}}
	if r.Segment == nil {
		light.Presets = presets
	} else if s, ok := d.find(*r.Segment); ok {
		light.Rgb = d.rgb(s)
	}
	return &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "light", DisplayName: r.Label, Constraints: &registry.CapabilityDescription_Light{Light: light}}
}
func containsPreset(c *registry.ControlChoices, key string) bool {
	for _, v := range c.GetChoices() {
		if v.Key == key {
			return true
		}
	}
	return false
}
