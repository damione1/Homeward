package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/lists"
)

func dashboardWidgetsImportLists(ctx context.Context, composition *listComposition, path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 16384 || composition == nil {
		return nil, errors.New("bounded real list composition required")
	}
	var sources []struct {
		Slot  string `json:"slot"`
		Alias string `json:"alias"`
		Name  string `json:"name"`
		Items []struct {
			UID       string `json:"uid"`
			Summary   string `json:"summary"`
			Completed bool   `json:"completed,omitempty"`
		} `json:"items"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&sources); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || len(sources) < 2 || len(sources) > 16 {
		return nil, errors.New("two to sixteen explicit list sources required")
	}
	result := make(map[string]string)
	for _, source := range sources {
		if !regexp.MustCompile(`^[A-Z][A-Z_]{0,31}$`).MatchString(source.Slot) || result[source.Slot] != "" || len(source.Items) == 0 || len(source.Items) > 8 {
			return nil, errors.New("invalid source slot or item bound")
		}
		alias, err := domain.ParseHAAlias(source.Alias)
		if err != nil {
			return nil, err
		}
		payload, err := lists.NewLegacyListPayload(source.Name)
		if err != nil {
			return nil, err
		}
		receipt, err := composition.ImportLegacyList(ctx, alias, payload)
		if err != nil {
			return nil, err
		}
		id := receipt.ListID()
		for index, row := range source.Items {
			uid, err := lists.ParseItemUID(row.UID)
			if err != nil {
				return nil, err
			}
			item, err := lists.NewItemPayload(lists.ItemInput{Summary: row.Summary, Completed: row.Completed, Due: lists.NoDue()})
			if err != nil {
				return nil, err
			}
			legacy, err := lists.NewLegacyItemPayload(uid, int64(index), item)
			if err != nil {
				return nil, err
			}
			expected, present := receipt.ListRevision()
			if !present {
				return nil, errors.New("list revision missing")
			}
			receipt, err = composition.ImportLegacyItem(ctx, id, alias, legacy, expected)
			if err != nil {
				return nil, err
			}
		}
		result[source.Slot], err = id.Value()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
