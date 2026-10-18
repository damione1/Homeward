package hilo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/damione1/Homeward/internal/integrations"
)

// The loaded python-hilo 2026.9.1 GraphqlHelper is the observation protocol
// authority. The Go SDK's multipart/locationId variant is for operation
// acknowledgements and is not mixed into this SSE profile.
const graphqlSnapshotDeviceFields = `
  deviceType hiloId connectionStatus
  ... on BasicSmartMeter { power { value kind } }
  ... on LowVoltageThermostat { ambientTemperature { value kind } ambientTempSetpoint { value kind } power { value kind } currentState }
  ... on BasicThermostat { ambientTemperature { value kind } ambientTempSetpoint { value kind } power { value kind } heatDemand }
  ... on HeatingFloorThermostat { ambientTemperature { value kind } ambientTempSetpoint { value kind } power { value kind } }
  ... on WaterHeater { probeTemp { value kind } power { value kind } state }
  ... on BasicChargeController { power { value kind } state }
  ... on BasicSwitch { power { value kind } state }
`

const graphqlSubscriptionDeviceFields = `
  ... on Gateway { connectionStatus }
  ... on BasicSmartMeter { hiloId connectionStatus power { value kind } }
  ... on LowVoltageThermostat { hiloId connectionStatus ambientTemperature { value kind } ambientTempSetpoint { value kind } power { value kind } currentState }
  ... on BasicThermostat { hiloId connectionStatus ambientTemperature { value kind } ambientTempSetpoint { value kind } power { value kind } heatDemand }
  ... on HeatingFloorThermostat { hiloId connectionStatus ambientTemperature { value kind } ambientTempSetpoint { value kind } power { value kind } }
  ... on WaterHeater { hiloId connectionStatus probeTemp { value kind } power { value kind } state }
  ... on BasicChargeController { hiloId connectionStatus power { value kind } state }
  ... on BasicSwitch { hiloId connectionStatus power { value kind } state }
`

const graphqlSnapshotQuery = `query getLocation($locationHiloId: String!) { getLocation(id: $locationHiloId) { hiloId devices {` + graphqlSnapshotDeviceFields + `} } }`
const graphqlSubscriptionQuery = `subscription onAnyDeviceUpdated($locationHiloId: String!) { onAnyDeviceUpdated(locationHiloId: $locationHiloId) { deviceType locationHiloId transmissionTime operationId status device {` + graphqlSubscriptionDeviceFields + `} } }`

type graphqlEnvelope struct {
	Data struct {
		Location struct {
			HiloID  string            `json:"hiloId"`
			Devices []json.RawMessage `json:"devices"`
		} `json:"getLocation"`
		Update struct {
			DeviceType       string          `json:"deviceType"`
			LocationHiloID   string          `json:"locationHiloId"`
			TransmissionTime string          `json:"transmissionTime"`
			OperationID      string          `json:"operationId"`
			Status           string          `json:"status"`
			Device           json.RawMessage `json:"device"`
		} `json:"onAnyDeviceUpdated"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func graphqlBody(query, hiloID string, full bool) ([]byte, error) {
	hash := sha256.Sum256([]byte(query))
	value := struct {
		Query     string `json:"query,omitempty"`
		Variables struct {
			LocationHiloID string `json:"locationHiloId"`
		} `json:"variables"`
		Extensions struct {
			Persisted struct {
				Version int    `json:"version"`
				SHA256  string `json:"sha256Hash"`
			} `json:"persistedQuery"`
		} `json:"extensions"`
	}{}
	if full {
		value.Query = query
	}
	value.Variables.LocationHiloID = hiloID
	value.Extensions.Persisted.Version = 1
	value.Extensions.Persisted.SHA256 = hex.EncodeToString(hash[:])
	return json.Marshal(value)
}

func decodeGraphQLEnvelope(raw []byte) (graphqlEnvelope, bool, error) {
	var result graphqlEnvelope
	if json.Unmarshal(raw, &result) != nil {
		return graphqlEnvelope{}, false, ErrProviderShape
	}
	if len(result.Errors) != 0 {
		if len(result.Errors) == 1 && result.Errors[0].Message == "PersistedQueryNotFound" {
			return graphqlEnvelope{}, true, nil
		}
		return graphqlEnvelope{}, false, ErrProviderShape
	}
	return result, false, nil
}

func graphqlSnapshot(ctx context.Context, client *integrations.CloudClient, settings Settings, token credentialJSON) ([]json.RawMessage, integrations.CloudResponse, error) {
	endpoint := settings.PlatformEndpoint("/api/digital-twin/v3/graphql")
	for attempt := 0; attempt < 2; attempt++ {
		body, err := graphqlBody(graphqlSnapshotQuery, settings.locationHiloID, attempt == 1)
		if err != nil {
			return nil, integrations.CloudResponse{}, ErrProviderShape
		}
		headers := bearerHeaders(token, false)
		headers.Set("Content-Type", "application/json")
		request, err := integrations.NewCloudRequest(http.MethodPost, endpoint, headers, body)
		if err != nil {
			return nil, integrations.CloudResponse{}, ErrProviderShape
		}
		response, err := client.Observe(ctx, request)
		if err != nil {
			return nil, integrations.CloudResponse{}, err
		}
		parsed, retryAPQ, err := decodeGraphQLEnvelope(response.Bytes())
		if err != nil {
			return nil, integrations.CloudResponse{}, err
		}
		if retryAPQ {
			if attempt == 0 {
				continue
			}
			return nil, integrations.CloudResponse{}, ErrProviderShape
		}
		if parsed.Data.Location.HiloID != settings.locationHiloID || parsed.Data.Location.Devices == nil || len(parsed.Data.Location.Devices) > 512 {
			return nil, integrations.CloudResponse{}, ErrProviderShape
		}
		return parsed.Data.Location.Devices, response, nil
	}
	return nil, integrations.CloudResponse{}, ErrProviderShape
}

func openGraphQLStream(ctx context.Context, client *integrations.CloudClient, settings Settings, token credentialJSON, full bool) (*integrations.CloudObservationStream, error) {
	body, err := graphqlBody(graphqlSubscriptionQuery, settings.locationHiloID, full)
	if err != nil {
		return nil, ErrProviderShape
	}
	headers := bearerHeaders(token, false)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	request, err := integrations.NewCloudStreamRequest(integrations.CloudStreamSSE, http.MethodPost, settings.PlatformEndpoint("/api/digital-twin/v3/graphql"), headers, body)
	if err != nil {
		return nil, ErrProviderShape
	}
	return client.OpenObservationStream(ctx, request)
}

func decodeGraphQLUpdate(raw []byte, expectedHiloID string) (string, deviceReading, *string, bool, error) {
	parsed, retryAPQ, err := decodeGraphQLEnvelope(raw)
	if err != nil || retryAPQ {
		return "", deviceReading{}, nil, retryAPQ, err
	}
	update := parsed.Data.Update
	if update.LocationHiloID == "" && len(update.Device) == 0 {
		return "", deviceReading{}, nil, false, nil
	}
	if update.LocationHiloID != expectedHiloID {
		return "", deviceReading{}, nil, false, ErrProviderShape
	}
	if len(update.Device) == 0 || strings.EqualFold(string(update.Device), "null") {
		return "", deviceReading{}, nil, false, ErrProviderShape
	}
	if update.OperationID != "" && update.Status != "SUCCEEDED" {
		return "", deviceReading{}, nil, false, nil
	}
	// The pinned Gateway fragment has no HiloID. DeviceHub owns gateway
	// identity/connectivity; this SSE event cannot identify its projection.
	if update.DeviceType == "Gateway" || update.DeviceType == "Hub" {
		return "", deviceReading{}, nil, false, nil
	}
	id, reading, err := decodeGraphQLDevice(update.Device)
	if err != nil {
		return "", deviceReading{}, nil, false, err
	}
	if !reading.hasPayload() {
		return "", deviceReading{}, nil, false, nil
	}
	var sourceTime *string
	if update.TransmissionTime != "" {
		sourceTime = &update.TransmissionTime
	}
	return id, reading, sourceTime, false, nil
}
