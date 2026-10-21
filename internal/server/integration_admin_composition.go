package server

import (
	"context"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrationadmin"
)

func newIntegrationAdminHandler(documents *configdocuments.Service, sessions *sessionCoordinator, runtime *observationRuntime) (*integrationAdminHandler, error) {
	if documents == nil || sessions == nil || sessions.authority == nil || runtime == nil {
		return nil, ErrInvalidPublicPlan
	}
	health := func(ctx context.Context, id domain.IntegrationInstanceID) (*obs.IntegrationReportPresence, *obs.IntegrationSupervision, error) {
		if ctx == nil || ctx.Err() != nil || runtime.sequence == nil || runtime.supervisor == nil {
			return nil, nil, integrationadmin.ErrUnavailable
		}
		report, err := runtime.sequence.IntegrationHealth(id)
		if err != nil {
			return nil, nil, err
		}
		phase, err := runtime.supervisor.ObservationIntegration(id)
		return report, phase, err
	}
	service, err := integrationadmin.New(documents, sessions.authority, health)
	if err != nil {
		return nil, err
	}
	return &integrationAdminHandler{service: service, documents: documents}, nil
}
