package server

import (
	"fmt"
	"github.com/damione1/Homeward/internal/apitypes"
	"testing"
)

func TestNHA203IntegrationOperationUnionConsumesOnlyCoveredPrefix(t *testing.T) {
	name := func(number int) apitypes.OperationName {
		value, err := apitypes.ParseOperationName(fmt.Sprintf("homes/synthetic/operations/config-%032x", number))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	global, integration := []apitypes.OperationName{name(1), name(3), name(5)}, []apitypes.OperationName{name(2), name(4), name(6)}
	gNext, iNext := name(5), name(6)
	page, next, err := mergeAdministrationOperationNames(global, &gNext, integration, &iNext, 500)
	if err != nil || len(page) != 5 || next == nil || *next != name(5) {
		t.Fatalf("covered frontier %v %v %v", page, next, err)
	}
	for index, value := range page {
		if value != name(index+1) {
			t.Fatal("merge order skipped candidate")
		}
	}
	page, next, err = mergeAdministrationOperationNames(global, &gNext, integration, &iNext, 2)
	if err != nil || len(page) != 2 || next == nil || *next != name(2) {
		t.Fatal("page limit consumed prefetched candidates", err)
	}
	page, next, err = mergeAdministrationOperationNames(global, nil, integration, nil, 500)
	if err != nil || len(page) != 6 || next != nil {
		t.Fatal("finite union did not end", err)
	}
	page, next, err = mergeAdministrationOperationNames(nil, nil, integration, &iNext, 500)
	if err != nil || len(page) != 3 || next == nil || *next != name(6) {
		t.Fatal("inaccessible global family hid integration operations", err)
	}
	if _, _, err := mergeAdministrationOperationNames([]apitypes.OperationName{name(1)}, nil, []apitypes.OperationName{name(1)}, nil, 500); err == nil {
		t.Fatal("one native attempt appeared in both closed families")
	}
}
