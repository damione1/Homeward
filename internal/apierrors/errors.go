// Package apierrors owns the canonical, redacted API error representation.
// It never retains an upstream error or user value. Domain adapters classify
// errors by type/identity before using these constructors.
package apierrors

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

const MaxViolations = 32
const MaxDetailsBytes = 16 * 1024

type specification struct {
	code    code.Code
	message string
}

// Explicit conversion from the generated reason vocabulary. The descriptor
// coverage test rejects a new reason lacking a deliberate code/message mapping.
var specifications = map[wire.ApiErrorReason]specification{
	wire.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED:   {code.Code_UNAUTHENTICATED, "Authenticate with a valid credential."},
	wire.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED:             {code.Code_PERMISSION_DENIED, "Permission denied on the requested resource, or it might not exist."},
	wire.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED:         {code.Code_INVALID_ARGUMENT, "Correct the documented request constraints."},
	wire.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND:        {code.Code_NOT_FOUND, "The authorized resource does not exist."},
	wire.ApiErrorReason_API_ERROR_REASON_RESOURCE_ALREADY_EXISTS:   {code.Code_ALREADY_EXISTS, "The resource identity already exists."},
	wire.ApiErrorReason_API_ERROR_REASON_REQUEST_ID_REUSED:         {code.Code_ALREADY_EXISTS, "Use a new request ID for a different intention."},
	wire.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH:             {code.Code_ABORTED, "Read the current revision and resolve the conflict before submitting a new intention."},
	wire.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED:            {code.Code_FAILED_PRECONDITION, "Read the current configuration before submitting a new intention."},
	wire.ApiErrorReason_API_ERROR_REASON_TOPOLOGY_CHANGED:          {code.Code_FAILED_PRECONDITION, "Read the current topology before submitting a new intention."},
	wire.ApiErrorReason_API_ERROR_REASON_CAPABILITY_UNAVAILABLE:    {code.Code_FAILED_PRECONDITION, "The current capability does not support this intention."},
	wire.ApiErrorReason_API_ERROR_REASON_SOURCE_READ_ONLY:          {code.Code_FAILED_PRECONDITION, "The source does not support modification."},
	wire.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED:           {code.Code_FAILED_PRECONDITION, "Capture a new complete view before continuing reads."},
	wire.ApiErrorReason_API_ERROR_REASON_RATE_LIMITED:              {code.Code_RESOURCE_EXHAUSTED, "The request exceeds the current admission rate."},
	wire.ApiErrorReason_API_ERROR_REASON_REQUEST_TOO_LARGE:         {code.Code_RESOURCE_EXHAUSTED, "Reduce the request to the documented size limit."},
	wire.ApiErrorReason_API_ERROR_REASON_STREAM_CAPACITY:           {code.Code_RESOURCE_EXHAUSTED, "The bounded stream capacity is exhausted."},
	wire.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED:     {code.Code_RESOURCE_EXHAUSTED, "Reduce the query window or complexity."},
	wire.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE:       {code.Code_UNAVAILABLE, "The service is temporarily unavailable; an attempted mutation may still have had an effect."},
	wire.ApiErrorReason_API_ERROR_REASON_REQUEST_CANCELLED:         {code.Code_CANCELLED, "The request was cancelled; reconcile any possibly submitted mutation."},
	wire.ApiErrorReason_API_ERROR_REASON_REQUEST_DEADLINE_EXCEEDED: {code.Code_DEADLINE_EXCEEDED, "The deadline expired; reconcile any possibly submitted mutation."},
	wire.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN:    {code.Code_UNKNOWN, "The commit result is unknown; reconcile the existing request identity without redispatch."},
	wire.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN:    {code.Code_UNKNOWN, "The physical outcome is unknown; reconcile the existing operation without redispatch."},
	wire.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE:          {code.Code_INTERNAL, "The request could not complete; retain the support reference."},
	wire.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY:     {code.Code_DATA_LOSS, "Stored data violates its documented invariants."},
	wire.ApiErrorReason_API_ERROR_REASON_METHOD_NOT_SUPPORTED:      {code.Code_UNIMPLEMENTED, "This implementation does not support the requested method."},
}

// Domain is derived from the actual resource annotation, not a second namespace.
func Domain() string {
	options := (&wire.Entity{}).ProtoReflect().Descriptor().Options()
	resource := proto.GetExtension(options, annotations.E_Resource).(*annotations.ResourceDescriptor)
	domain, _, _ := strings.Cut(resource.Type, "/")
	return domain
}

// Violation proves a real protobuf field path and a declared nonzero rule.
// No rejected value, raw upstream text or arbitrary metadata can be attached.
type Violation struct {
	field string
	rule  wire.ValidationReason
}

func (v Violation) Field() string               { return v.field }
func (v Violation) Rule() wire.ValidationReason { return v.rule }

var segment = regexp.MustCompile(`^([a-z][a-z0-9_]*)(?:\[([0-9]{1,5})\])?$`)
var ErrViolation = errors.New("invalid violation descriptor or field path")

func NewViolation(root protoreflect.MessageDescriptor, field string, rule wire.ValidationReason) (Violation, error) {
	if root == nil || len(field) > 256 || rule == 0 {
		return Violation{}, ErrViolation
	}
	if _, known := wire.ValidationReason_name[int32(rule)]; !known {
		return Violation{}, ErrViolation
	}
	if field == "" {
		return Violation{field, rule}, nil
	}
	parts := strings.Split(field, ".")
	for index, part := range parts {
		match := segment.FindStringSubmatch(part)
		if match == nil || root == nil {
			return Violation{}, ErrViolation
		}
		fd := root.Fields().ByName(protoreflect.Name(match[1]))
		if fd == nil || fd.IsMap() {
			return Violation{}, ErrViolation
		}
		if match[2] != "" {
			value, err := strconv.Atoi(match[2])
			if err != nil || value > 65535 || !fd.IsList() || strconv.Itoa(value) != match[2] {
				return Violation{}, ErrViolation
			}
		} else if fd.IsList() && index != len(parts)-1 {
			return Violation{}, ErrViolation
		}
		root = fd.Message()
	}
	return Violation{field, rule}, nil
}

// Fault is immutable after construction. A Go zero value becomes a safe
// INTERNAL error, never success. An unknown reason follows the same rule.
type Fault struct {
	reason     wire.ApiErrorReason
	violations []Violation
	truncated  bool
}

func New(reason wire.ApiErrorReason) Fault {
	if _, known := specifications[reason]; !known {
		reason = wire.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	}
	return Fault{reason: reason}
}
func (f Fault) Reason() wire.ApiErrorReason {
	if _, known := specifications[f.reason]; !known {
		return wire.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	}
	return f.reason
}
func (f Fault) Code() code.Code         { return specifications[f.Reason()].code }
func (f Fault) Error() string           { return specifications[f.Reason()].message }
func (f Fault) Violations() []Violation { return append([]Violation(nil), f.violations...) }

func Validation(violations ...Violation) Fault {
	f := New(wire.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	// Retain only the sorted prefix; memory stays bounded even when many
	// independent input elements violate their constraints.
	for _, value := range violations {
		if value.rule == 0 {
			return New(wire.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
		}
		i := sort.Search(len(f.violations), func(i int) bool {
			current := f.violations[i]
			return current.field > value.field || current.field == value.field && current.rule.String() >= value.rule.String()
		})
		if i < len(f.violations) && f.violations[i] == value {
			continue
		}
		if len(f.violations) == MaxViolations {
			f.truncated = true
			if i == MaxViolations {
				continue
			}
		} else {
			f.violations = append(f.violations, Violation{})
		}
		copy(f.violations[i+1:], f.violations[i:len(f.violations)-1])
		f.violations[i] = value
	}
	return f
}

func pack(message proto.Message) *anypb.Any {
	// All messages contain only static text or constructor-admitted ASCII paths.
	// No input can introduce invalid UTF-8 or an arbitrary message implementation.
	value, err := anypb.New(message)
	if err != nil {
		panic("apierrors: invalid internally constructed detail")
	}
	return value
}

// Status returns fresh Google RPC objects. It is a logical error representation,
// not a successful RPC reply. Native protocol adapters carry its typed details.
func (f Fault) Status() *statuspb.Status {
	reason := f.Reason()
	info := &errdetails.ErrorInfo{Domain: Domain(), Reason: strings.TrimPrefix(reason.String(), "API_ERROR_REASON_")}
	if f.truncated {
		info.Metadata = map[string]string{"violationsTruncated": "true"}
	}
	result := &statuspb.Status{Code: int32(f.Code()), Message: f.Error(), Details: []*anypb.Any{pack(info)}}
	if len(f.violations) != 0 {
		bad := &errdetails.BadRequest{}
		for _, value := range f.violations {
			bad.FieldViolations = append(bad.FieldViolations, &errdetails.BadRequest_FieldViolation{
				Field: value.field, Reason: strings.TrimPrefix(value.rule.String(), "VALIDATION_REASON_"), Description: "Value does not meet the documented constraint.",
			})
		}
		result.Details = append(result.Details, pack(bad))
	}
	if f.Code() == code.Code_FAILED_PRECONDITION || reason == wire.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH {
		result.Details = append(result.Details, pack(&errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{{
			Type: info.Reason, Description: f.Error(),
		}}}))
	}
	if f.Code() == code.Code_RESOURCE_EXHAUSTED {
		result.Details = append(result.Details, pack(&errdetails.QuotaFailure{Violations: []*errdetails.QuotaFailure_Violation{{
			Subject: "request", Description: f.Error(),
		}}}))
	}
	var phase wire.UncertaintyPhase
	switch reason {
	case wire.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN:
		phase = wire.UncertaintyPhase_UNCERTAINTY_PHASE_COMMIT
	case wire.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN:
		phase = wire.UncertaintyPhase_UNCERTAINTY_PHASE_EFFECT
	}
	if phase != 0 {
		result.Details = append(result.Details, pack(&wire.ExecutionUncertainty{
			Phase: phase, Disposition: wire.ReconciliationDisposition_RECONCILIATION_DISPOSITION_RECONCILE_ONLY,
		}))
	}
	if reason == wire.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE {
		var id [16]byte
		if _, err := rand.Read(id[:]); err == nil {
			result.Details = append(result.Details, pack(&errdetails.RequestInfo{RequestId: hex.EncodeToString(id[:])}))
		}
	}
	return result
}

func (f Fault) Connect() *connect.Error {
	status := f.Status()
	result := connect.NewError(connect.Code(status.Code), errors.New(status.Message))
	for _, value := range status.Details {
		detail, _ := connect.NewErrorDetail(value)
		result.AddDetail(detail)
	}
	return result
}
