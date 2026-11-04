package server

import (
	"context"
	"errors"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apimutations"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The transport supplies pure result encoding while the generic owner hook
// keeps its actual result type all the way into the existing transaction.
func businessMutationContext[T any](ctx context.Context, method, id string, normalized proto.Message, encode func(T) (proto.Message, error)) (context.Context, error) {
	if id == "" {
		return ctx, nil
	}
	bytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(normalized)
	if err != nil {
		return nil, taskInvalid()
	}
	request, err := apimutations.New(ctx, method, id, bytes)
	if err != nil {
		return nil, taskInvalid()
	}
	return apimutations.WithResultEncoder(ctx, request, func(value T) ([]byte, error) {
		message, err := encode(value)
		if err != nil {
			return nil, err
		}
		return proto.MarshalOptions{Deterministic: true}.Marshal(message)
	})
}
func businessMutationFault(err error) error {
	var outcome *apimutations.Error
	if !errors.As(err, &outcome) || outcome == nil {
		return err
	}
	var reason api.ApiErrorReason
	switch outcome.Code() {
	case apimutations.RequestIDReused:
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_ID_REUSED
	case apimutations.Capacity:
		reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
	case apimutations.Replayed, apimutations.StoredResultCorrupt:
		reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
	default:
		reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	}
	return apierrors.New(reason)
}

// A replay is returned only after the caller's current object admission. It is
// not a new mutation, never reacquires media and never emits another change.
func businessReplay[T proto.Message](err error, output T) (T, bool, error) {
	var zero T
	var outcome *apimutations.Error
	if !errors.As(err, &outcome) || outcome == nil {
		return zero, false, err
	}
	raw, replayed := outcome.ReplayResult()
	if !replayed {
		return zero, false, businessMutationFault(err)
	}
	if err := proto.Unmarshal(raw, output); err != nil || !calendarKnown(output) {
		return zero, false, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	return output, true, nil
}

// Normalize only the generated transport representation: request_id is the
// dedup key rather than intention content, and OUTPUT_ONLY fields cannot alter
// an intention. Domain constructors and explicit mask selection normalize the
// remaining editable values before this function is called.
func businessNormalize(message proto.Message) proto.Message {
	copy := proto.Clone(message)
	root := copy.ProtoReflect()
	if field := root.Descriptor().Fields().ByName("request_id"); field != nil {
		root.Clear(field)
	}
	businessClearOutputs(root)
	return copy
}
func businessClearOutputs(message protoreflect.Message) {
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if options := field.Options(); options != nil && proto.HasExtension(options, annotations.E_FieldBehavior) {
			for _, behavior := range proto.GetExtension(options, annotations.E_FieldBehavior).([]annotations.FieldBehavior) {
				if behavior == annotations.FieldBehavior_OUTPUT_ONLY {
					message.Clear(field)
					return true
				}
			}
		}
		if field.Kind() == protoreflect.MessageKind {
			if field.IsList() {
				list := value.List()
				for i := 0; i < list.Len(); i++ {
					businessClearOutputs(list.Get(i).Message())
				}
			} else {
				businessClearOutputs(value.Message())
			}
		}
		return true
	})
}
