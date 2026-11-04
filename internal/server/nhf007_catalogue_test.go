package server

import (
	"bytes"
	"encoding/json"
	"errors"
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/internal/authorization"
	"io"
	"os"
)

type nhf007Catalogue struct{ file string }
type nhf007Entry struct {
	ID         string `json:"id"`
	URLPath    string `json:"url_path"`
	File       string `json:"file"`
	Projection string `json:"projection,omitempty"`
}
type nhf007Config struct {
	Version    uint32        `json:"version"`
	Dashboards []nhf007Entry `json:"dashboards"`
}

func (c *nhf007Catalogue) load() (nhf007Config, error) {
	raw, err := os.ReadFile(c.file)
	if err != nil || len(raw) > 1<<20 {
		return nhf007Config{}, errors.New("synthetic catalogue unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result nhf007Config
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF || result.Version != 1 || len(result.Dashboards) > 128 {
		return result, errors.New("invalid synthetic catalogue")
	}
	seen := map[string]bool{}
	for _, entry := range result.Dashboards {
		if entry.Projection != "" && entry.Projection != "normal" {
			return result, errors.New("retired v1 fault projection is not a native configuration")
		}
		id, err := authorization.ParseDashboardID(entry.ID)
		if err != nil || seen[entry.ID] {
			return result, errors.New("duplicate or invalid synthetic identity")
		}
		seen[entry.ID] = true
		if _, err := imported.NewSource(id, entry.URLPath, entry.File); err != nil {
			return result, err
		}
	}
	return result, nil
}
