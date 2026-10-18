package hilo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
)

type negotiateResponse struct {
	URL             string `json:"url"`
	AccessToken     string `json:"accessToken"`
	ConnectionToken string `json:"connectionToken"`
	ConnectionID    string `json:"connectionId"`
}

func negotiateURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", ErrProviderShape
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/negotiate"
	q := u.Query()
	q.Set("negotiateVersion", "1")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p *Pilot) negotiate(ctx context.Context, endpoint string, headers http.Header) (negotiateResponse, error) {
	target, err := negotiateURL(endpoint)
	if err != nil {
		return negotiateResponse{}, err
	}
	request, err := integrations.NewCloudRequest(http.MethodPost, target, headers, nil)
	if err != nil {
		return negotiateResponse{}, ErrProviderShape
	}
	response, err := p.client.Observe(ctx, request)
	if err != nil {
		return negotiateResponse{}, err
	}
	var parsed negotiateResponse
	if json.Unmarshal(response.Bytes(), &parsed) != nil {
		return negotiateResponse{}, ErrProviderShape
	}
	return parsed, nil
}

func (p *Pilot) edgeAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.User != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	base := p.settings.apiBase
	if u.Scheme == "http" && base.Scheme == "http" && u.Host == base.Host {
		return true
	}
	if u.Scheme != "https" {
		return false
	}
	return u.Host == base.Host || strings.HasSuffix(strings.ToLower(u.Hostname()), ".service.signalr.net")
}

func writeHub(ctx context.Context, stream *integrations.CloudObservationStream, bytes []byte) error {
	message, err := integrations.NewCloudWebSocketMessage(integrations.CloudStreamWebSocketText, bytes)
	if err != nil {
		return err
	}
	return stream.Write(ctx, message)
}

// SignalR's JSON protocol requires a client ping every 15 seconds. This is
// transport upkeep only: it never refreshes an observed value or source lease.
func pingHub(ctx context.Context, stream *integrations.CloudObservationStream, fail func(error), done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeHub(ctx, stream, []byte("{\"type\":6}\x1e")); err != nil {
				if ctx.Err() == nil {
					fail(err)
				}
				return
			}
		}
	}
}

func readHub(ctx context.Context, stream *integrations.CloudObservationStream) ([]hubFrame, error) {
	message, err := stream.Receive(ctx)
	if err != nil {
		return nil, err
	}
	kind, err := message.Kind()
	if err != nil || kind != integrations.CloudStreamWebSocketText {
		return nil, ErrProviderShape
	}
	data, err := message.Bytes()
	if err != nil {
		return nil, err
	}
	return decodeHubFrames(data)
}

func (p *Pilot) openDeviceHub(ctx context.Context, token credentialJSON) (*integrations.CloudObservationStream, []hubFrame, error) {
	first, err := p.negotiate(ctx, p.settings.APIEndpoint("/DeviceHub"), bearerHeaders(token, true))
	if err != nil {
		return nil, nil, err
	}
	if first.URL == "" || first.AccessToken == "" || !p.edgeAllowed(first.URL) {
		return nil, nil, ErrProviderShape
	}
	second, err := p.negotiate(ctx, first.URL, http.Header{"Authorization": {"Bearer " + first.AccessToken}})
	if err != nil {
		return nil, nil, err
	}
	connectionToken := second.ConnectionToken
	if connectionToken == "" {
		connectionToken = second.ConnectionID
	}
	if connectionToken == "" || len(connectionToken) > 4096 {
		return nil, nil, ErrProviderShape
	}
	u, err := url.Parse(first.URL)
	if err != nil {
		return nil, nil, ErrProviderShape
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	q := u.Query()
	q.Set("id", connectionToken)
	u.RawQuery = q.Encode()
	request, err := integrations.NewCloudStreamRequest(integrations.CloudStreamWebSocket, http.MethodGet, u.String(), http.Header{"Authorization": {"Bearer " + first.AccessToken}}, nil)
	if err != nil {
		return nil, nil, ErrProviderShape
	}
	stream, err := p.client.OpenObservationStream(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	if err := writeHub(ctx, stream, []byte("{\"protocol\":\"json\",\"version\":1}\x1e")); err != nil {
		_ = stream.Close(context.Background())
		return nil, nil, err
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	frames, err := readHub(handshakeCtx, stream)
	if err != nil || len(frames) == 0 || frames[0].Type != 0 || frames[0].Error != "" {
		_ = stream.Close(context.Background())
		return nil, nil, ErrProviderShape
	}
	command, err := hubInvocation("SubscribeToLocation", p.settings.locationID)
	if err != nil || writeHub(ctx, stream, command) != nil {
		_ = stream.Close(context.Background())
		return nil, nil, ErrProviderShape
	}
	return stream, frames[1:], nil
}

func (p *Pilot) awaitInventory(ctx context.Context, stream *integrations.CloudObservationStream, pending []hubFrame) ([]hubDevice, []hubFrame, error) {
	limitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for limitCtx.Err() == nil {
		if len(pending) == 0 {
			frames, err := readHub(limitCtx, stream)
			if err != nil {
				return nil, nil, err
			}
			pending = frames
		}
		for len(pending) != 0 {
			frame := pending[0]
			pending = pending[1:]
			if frame.Type == 7 || frame.Type == 3 && frame.Error != "" {
				return nil, nil, ErrProviderShape
			}
			if frame.Type != 1 || frame.Target != "DeviceListInitialValuesReceived" {
				continue
			}
			if len(frame.Arguments) != 1 {
				return nil, nil, ErrProviderShape
			}
			rows, err := decodeHubInventory(frame.Arguments[0])
			return rows, pending, err
		}
	}
	return nil, nil, ErrProviderShape
}
