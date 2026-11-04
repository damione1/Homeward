package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
)

type integrationPageKind uint8

const (
	integrationTypesPage integrationPageKind = iota + 1
	integrationInstancesPage
)

type integrationPageToken struct {
	Version uint8               `json:"v"`
	Kind    integrationPageKind `json:"k"`
	Home    string              `json:"h"`
	binding []byte
	Capture [32]byte `json:"c"`
	After   string   `json:"a"`
	Expires int64    `json:"e"`
}

func (h *integrationHandler) pageBinding(ctx context.Context, home apitypes.HomeName, kind integrationPageKind) ([]byte, error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, integrationFault(err)
	}
	current, err := h.authority.Current(principal)
	if err != nil {
		return nil, integrationFault(err)
	}
	method := "ListIntegrationInstances"
	if kind == integrationTypesPage {
		method = "ListIntegrationTypes"
	}
	return homePageBinding(current, home.String(), "", method)
}

func integrationPageCapture(names, etags []string) [32]byte {
	hash := sha256.New()
	for index, name := range names {
		for _, value := range []string{name, etags[index]} {
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			_, _ = hash.Write(size[:])
			_, _ = hash.Write([]byte(value))
		}
	}
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func (h *integrationHandler) decodePage(token string, kind integrationPageKind, home apitypes.HomeName, binding []byte, capture [32]byte) (integrationPageToken, error) {
	invalid := func() (integrationPageToken, error) {
		if kind == integrationTypesPage {
			return integrationPageToken{}, integrationInvalidField(&api.ListIntegrationTypesRequest{}, "page_token", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
		}
		return integrationPageToken{}, integrationInvalidField(&api.ListIntegrationInstancesRequest{}, "page_token", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
	}
	if h.now == nil || h.pages == nil {
		return integrationPageToken{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	if token == "" {
		return integrationPageToken{Version: 1, Kind: kind, Home: home.String(), binding: bytes.Clone(binding), Capture: capture, Expires: h.now().Add(5 * time.Minute).Unix()}, nil
	}
	if len(token) > 2048 {
		return invalid()
	}
	body, err := openAPIPage(h.pages, token, binding)
	if err != nil {
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result integrationPageToken
	if decoder.Decode(&result) != nil {
		return invalid()
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF || result.Version != 1 || result.Kind != kind || result.Home != home.String() || result.After == "" {
		return invalid()
	}
	if result.Expires <= h.now().Unix() || result.Capture != capture {
		return integrationPageToken{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
	}
	result.binding = bytes.Clone(binding)
	return result, nil
}

func (h *integrationHandler) encodePage(token integrationPageToken, after string) (string, error) {
	token.After = after
	body, err := json.Marshal(token)
	if err != nil {
		return "", integrationResourceIntegrity()
	}
	return sealAPIPage(h.pages, body, token.binding)
}
