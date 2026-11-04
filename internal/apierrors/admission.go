package apierrors

import (
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidStatus = errors.New("invalid canonical API error")
var admittedPathPart = regexp.MustCompile(`^[a-z][a-z0-9_]*(\[(0|[1-9][0-9]{0,4})\])?$`)

func admittedErrorPath(path string) bool {
	if len(path) > 256 {
		return false
	}
	if path == "" {
		return true
	}
	for _, part := range strings.Split(path, ".") {
		if !admittedPathPart.MatchString(part) {
			return false
		}
		if _, tail, found := strings.Cut(part, "["); found {
			if _, err := strconv.ParseUint(strings.TrimSuffix(tail, "]"), 10, 16); err != nil {
				return false
			}
		}
	}
	return true
}

// AdmitStatus admits the same bounded logical error vocabulary as the Rust
// client. Descriptive text, links and upstream diagnostics are discarded;
// only declared reasons, field rules and mandatory uncertainty are retained.
// It confers no permission and never authorizes replay of a mutation.
func AdmitStatus(raw *statuspb.Status) (Fault, error) {
	invalid := func() (Fault, error) { return Fault{}, ErrInvalidStatus }
	if raw == nil || proto.Size(raw) > MaxDetailsBytes || len(raw.Details) > 10 || len(raw.ProtoReflect().GetUnknown()) != 0 {
		return invalid()
	}
	seen := map[string]bool{}
	var reason api.ApiErrorReason
	var violations []Violation
	truncated := false
	var uncertainty api.UncertaintyPhase
	var operation apitypes.OperationName
	for _, detail := range raw.Details {
		if detail == nil || len(detail.ProtoReflect().GetUnknown()) != 0 {
			return invalid()
		}
		kind := strings.TrimPrefix(detail.TypeUrl, "type.googleapis.com/")
		if seen[kind] {
			return invalid()
		}
		seen[kind] = true
		var message proto.Message
		switch kind {
		case "google.rpc.ErrorInfo":
			message = &errdetails.ErrorInfo{}
		case "google.rpc.BadRequest":
			message = &errdetails.BadRequest{}
		case "home.api.v2.ExecutionUncertainty":
			message = &api.ExecutionUncertainty{}
		case "google.rpc.RequestInfo":
			message = &errdetails.RequestInfo{}
		case "google.rpc.PreconditionFailure":
			message = &errdetails.PreconditionFailure{}
		case "google.rpc.QuotaFailure":
			message = &errdetails.QuotaFailure{}
		case "google.rpc.RetryInfo":
			message = &errdetails.RetryInfo{}
		case "google.rpc.LocalizedMessage":
			message = &errdetails.LocalizedMessage{}
		case "google.rpc.Help":
			message = &errdetails.Help{}
		case "google.rpc.ResourceInfo":
			message = &errdetails.ResourceInfo{}
		default:
			return invalid()
		}
		if proto.Unmarshal(detail.Value, message) != nil {
			return invalid()
		}
		switch value := message.(type) {
		case *errdetails.ErrorInfo:
			if len(value.ProtoReflect().GetUnknown()) != 0 || value.Domain != Domain() {
				return invalid()
			}
			for key, text := range value.Metadata {
				if key != "violationsTruncated" || text != "true" {
					return invalid()
				}
			}
			truncated = len(value.Metadata) != 0
			number, known := api.ApiErrorReason_value["API_ERROR_REASON_"+value.Reason]
			if !known || number == 0 {
				return invalid()
			}
			reason = api.ApiErrorReason(number)
		case *errdetails.BadRequest:
			if len(value.ProtoReflect().GetUnknown()) != 0 || len(value.FieldViolations) > MaxViolations {
				return invalid()
			}
			for _, issue := range value.FieldViolations {
				if issue == nil || len(issue.ProtoReflect().GetUnknown()) != 0 || !admittedErrorPath(issue.Field) {
					return invalid()
				}
				number, known := api.ValidationReason_value["VALIDATION_REASON_"+issue.Reason]
				if !known || number == 0 {
					return invalid()
				}
				admitted := Violation{field: issue.Field, rule: api.ValidationReason(number)}
				if len(violations) != 0 {
					last := violations[len(violations)-1]
					if last.field > admitted.field || last.field == admitted.field && last.rule.String() >= admitted.rule.String() {
						return invalid()
					}
				}
				violations = append(violations, admitted)
			}
		case *api.ExecutionUncertainty:
			if len(value.ProtoReflect().GetUnknown()) != 0 || value.Disposition != api.ReconciliationDisposition_RECONCILIATION_DISPOSITION_RECONCILE_ONLY {
				return invalid()
			}
			if value.Phase != api.UncertaintyPhase_UNCERTAINTY_PHASE_COMMIT && value.Phase != api.UncertaintyPhase_UNCERTAINTY_PHASE_EFFECT {
				return invalid()
			}
			uncertainty = value.Phase
		case *errdetails.ResourceInfo:
			if len(value.ProtoReflect().GetUnknown()) != 0 || value.ResourceType != "longrunning.googleapis.com/Operation" || value.Owner != "" || value.Description != "" {
				return invalid()
			}
			var err error
			operation, err = apitypes.ParseOperationName(value.ResourceName)
			if err != nil {
				return invalid()
			}
		case *errdetails.RequestInfo:
			decoded, err := hex.DecodeString(value.RequestId)
			if len(value.ProtoReflect().GetUnknown()) != 0 || value.ServingData != "" || err != nil || len(decoded) != 16 {
				return invalid()
			}
		}
	}
	if reason == 0 {
		return invalid()
	}
	fault := New(reason)
	if fault.Reason() != reason || int32(fault.Code()) != raw.Code {
		return invalid()
	}
	expected := api.UncertaintyPhase_UNCERTAINTY_PHASE_UNSPECIFIED
	switch reason {
	case api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN:
		expected = api.UncertaintyPhase_UNCERTAINTY_PHASE_COMMIT
	case api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN:
		expected = api.UncertaintyPhase_UNCERTAINTY_PHASE_EFFECT
	}
	if operation.Valid() && expected == 0 || uncertainty != expected || truncated && len(violations) != MaxViolations || len(violations) != 0 && reason != api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED {
		return invalid()
	}
	fault.violations = violations
	fault.truncated = truncated
	fault.operation = operation
	return fault, nil
}
