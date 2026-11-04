package server

import (
	"errors"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/integrationadmin"
	"google.golang.org/protobuf/proto"
)

func integrationInvalidField(message proto.Message, field string, reason api.ValidationReason) apierrors.Fault {
	violation, err := apierrors.NewViolation(message.ProtoReflect().Descriptor(), field, reason)
	if err != nil {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	return apierrors.Validation(violation)
}

func integrationFault(err error) apierrors.Fault {
	switch {
	case errors.Is(err, integrationadmin.ErrInvalid):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	case errors.Is(err, integrationadmin.ErrNotFound), errors.Is(err, integrationadmin.ErrUnknownType):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	case errors.Is(err, integrationadmin.ErrAlreadyExists):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_ALREADY_EXISTS)
	case errors.Is(err, integrationadmin.ErrWorkspaceInvalid):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED)
	case errors.Is(err, integrationadmin.ErrUnavailable):
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	default:
		return administrationFault(err)
	}
}
