package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
)

func TestNHA168PackageHealthKeepsReadinessAndDurabilityDistinct(t *testing.T) {
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cell, err := observability.NewHealthCell(epoch)
	if err != nil {
		t.Fatal(err)
	}
	handler := packageOperatorHealth(http.NotFoundHandler(), cell)
	probe := func(ready, degraded bool) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
		var got struct {
			Live     bool `json:"live"`
			Ready    bool `json:"ready"`
			Degraded bool `json:"degraded"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil || !got.Live || got.Ready != ready || got.Degraded != degraded {
			t.Fatalf("health does not reflect owned facts: %d %s", response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("health response can be cached")
		}
	}
	probe(false, true)
	revision, err := observability.NewActiveRevision(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cell.PublishReady(revision); err != nil {
		t.Fatal(err)
	}
	if err := cell.PublishDurableFact(obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE, time.Now()); err != nil {
		t.Fatal(err)
	}
	probe(true, false)
	if err := cell.PublishDurableFact(obs.DurableAvailability_DURABLE_AVAILABILITY_DEGRADED, time.Now()); err != nil {
		t.Fatal(err)
	}
	probe(true, true)
	if err := cell.PublishDraining(); err != nil {
		t.Fatal(err)
	}
	probe(false, true)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/health", nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatal("health accepted a mutation method")
		}
	}
}

func TestNHA168PackageHealthIsAbsentWithoutExplicitCell(t *testing.T) {
	response := httptest.NewRecorder()
	packageOperatorHealth(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusNotFound {
		t.Fatal("implicit package health authority")
	}
}
