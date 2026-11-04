package server

import (
	"encoding/json"
	"fmt"
	"github.com/damione1/Homeward/internal/domain"
	"io"
	"os"
	"path/filepath"
)

const nhf016CameraSchema = "home.cameras.v1.Camera"
const nhf016CameraEndpoint = "camera"
const nhf016AllCameras = `  - scope: {family: media, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`

type nhf016CameraSource struct {
	Entity string   `json:"entity_id"`
	Title  string   `json:"title"`
	Source string   `json:"source"`
	RGB    [3]uint8 `json:"rgb"`
	Events uint32   `json:"events"`
}

func nhf016ReadCameraSources(root string) ([]nhf016CameraSource, error) {
	file, err := os.Open(filepath.Join(root, "cameras.json"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 8193))
	decoder.DisallowUnknownFields()
	var input struct {
		SyntheticOnly bool                 `json:"synthetic_only"`
		Cameras       []nhf016CameraSource `json:"cameras"`
	}
	if err = decoder.Decode(&input); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || !input.SyntheticOnly || len(input.Cameras) != 2 {
		return nil, fmt.Errorf("two bounded synthetic-only camera configurations required")
	}
	seen := make(map[string]bool)
	for _, camera := range input.Cameras {
		if _, err = domain.ParseEntityID(camera.Entity); err != nil || seen[camera.Entity] || camera.Title == "" || len(camera.Title) > 1024 || camera.Source == "" || len(camera.Source) > 1024 || camera.Events > 10000 {
			return nil, fmt.Errorf("invalid synthetic camera source")
		}
		seen[camera.Entity] = true
	}
	return input.Cameras, nil
}
