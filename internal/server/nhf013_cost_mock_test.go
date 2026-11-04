package server

// An explicitly enabled monetary fixture; no production tariff or accounting.
import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	history "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
)

const nhf013MockCostLast = "123456789012345678901234567890.123456789"

type nhf013MonetaryHistory struct {
	apiv2connect.HistoryServiceClient
	first, last time.Time
}

func (m nhf013MonetaryHistory) QueryObservations(ctx context.Context, request *connect.Request[history.QueryObservationsRequest]) (*connect.Response[history.QueryObservationsResponse], error) {
	// Forward credentials unchanged. The real service must first admit this
	// exact entity, period and cursor; denied/stale/budget replies stay intact.
	response, err := m.HistoryServiceClient.QueryObservations(ctx, request)
	if err != nil {
		return nil, err
	}
	nhf013MonetaryPage(response.Msg, m.first, m.last)
	return response, nil
}

func (m nhf013MonetaryHistory) QueryAggregates(ctx context.Context, request *connect.Request[history.QueryAggregatesRequest]) (*connect.Response[history.QueryAggregatesResponse], error) {
	if _, err := m.HistoryServiceClient.QueryAggregates(ctx, request); err != nil {
		return nil, err
	}
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("synthetic monetary aggregates are not implemented"))
}

func nhf013MonetaryPage(response *history.QueryObservationsResponse, first, last time.Time) {
	page := response
	if page == nil {
		return
	}
	// This deliberately sparse monetary surface cannot attest a complete
	// production total. No temporal location is inferred for missing values.
	if page.Coverage != nil {
		page.Coverage.Verdict = history.HistoryCoverageVerdict_HISTORY_COVERAGE_VERDICT_NOT_PROVEN
	}
	points := make([]*history.HistoryObservation, 0, 2)
	for _, point := range page.Observations {
		if point.GetState().GetReceiveTime() == nil {
			continue
		}
		at := point.State.ReceiveTime.AsTime()
		var decimal string
		switch {
		case at.Equal(first):
			decimal = "0.00"
		case at.Equal(last):
			decimal = nhf013MockCostLast
		default:
			continue // Empty pages retain their actual progress token.
		}
		point.State.Value = &history.HistoricalState_Measurement{Measurement: &history.HistoricalMeasurement{Unit: "CAD", DecimalValue: &decimal}}
		point.State.Quality = &history.ObservationQuality{Availability: history.Availability_AVAILABILITY_AVAILABLE, Freshness: history.Freshness_FRESHNESS_CURRENT, Provenance: history.Provenance_PROVENANCE_ESTIMATED}
		// Provisional existing-contract representation: a count of CAD units,
		// never energy or a convertible USD amount. Production monetary domain
		// semantics and exact aggregate storage remain a server follow-up.
		point.Descriptor_ = &history.HistoryNumericDescriptor{Unit: "CAD", CanonicalUnit: "CAD", Dimension: history.HistoryDimension_HISTORY_DIMENSION_COUNT, Scale: 1, Precision: 9, Semantic: history.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL, Aggregation: history.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM}
		points = append(points, point)
	}
	page.Observations = points
}

func nhf013CostMock(t *testing.T, upstream string, client *http.Client, data string, first, last time.Time) string {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil || target.Scheme != "https" || target.Hostname() != "127.0.0.1" {
		t.Fatal("synthetic loopback upstream required", err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	forward.Transport = client.Transport
	mux := http.NewServeMux()
	path, handler := apiv2connect.NewHistoryServiceHandler(nhf013MonetaryHistory{HistoryServiceClient: apiv2connect.NewHistoryServiceClient(client, upstream), first: first, last: last}, connect.WithReadMaxBytes(256<<10))
	mux.Handle(path, handler)
	mux.Handle("/", forward)
	proxy := httptest.NewUnstartedServer(mux)
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	proxy.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	proxy.EnableHTTP2 = true
	proxy.StartTLS()
	t.Cleanup(proxy.Close)
	return proxy.URL
}
