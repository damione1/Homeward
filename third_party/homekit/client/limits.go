package client

import (
	"fmt"
	"io"
	"net/http"
)

const maximumResponseBytes = 1 << 20

func readResponseBody(body io.Reader) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(body, maximumResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(value) > maximumResponseBytes {
		return nil, fmt.Errorf("HomeKit response exceeds limit")
	}
	return value, nil
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
