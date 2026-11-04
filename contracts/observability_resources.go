package contracts

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
)

// Closed v2 request admission precedes native reads and any SQL diagnostics.
func AdmitObservationRequestShape(value proto.Message) error {
	if value == nil || !value.ProtoReflect().IsValid() || !configurationKnown(value.ProtoReflect()) || proto.Size(value) > 4096 {
		return ErrInvalidObservabilityContract
	}
	switch value.(type) {
	case *api.GetHealthRequest, *api.GetMetricsRequest, *api.GetIntegrationHealthRequest, *api.QueryRuntimeLogsRequest, *api.QueryAuditsRequest, *api.QueryRunTraceRequest, *api.QueryRecorderCoverageRequest:
		return nil
	default:
		return ErrInvalidObservabilityContract
	}
}
