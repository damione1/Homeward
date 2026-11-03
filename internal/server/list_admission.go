package server

import (
	"errors"
	"math"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	listsv1 "github.com/damione1/Homeward/gen/home/lists/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/lists"
)

type listInputIssues struct {
	values            []apierrors.Violation
	invalidDescriptor bool
}

func (v *listInputIssues) add(field string, rule api.ValidationReason) {
	issue, err := apierrors.NewViolation((&listsv1.AddItemRequest{}).ProtoReflect().Descriptor(), field, rule)
	if err != nil {
		v.invalidDescriptor = true
		return
	}
	v.values = append(v.values, issue)
}
func (v listInputIssues) failure() error {
	if v.invalidDescriptor {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
	}
	if len(v.values) == 0 {
		return nil
	}
	return apierrors.Validation(v.values...).Connect()
}

// parseListAddition is called only inside withListTarget's retained permit.
// Constructors are the owners of constraints; independent field failures are
// collected without manufacturing a descendant error for an absent parent.
func parseListAddition(raw *listsv1.AddItemRequest, id domain.ListID) (listAddition, error) {
	var issues listInputIssues
	if raw == nil || len(raw.ProtoReflect().GetUnknown()) != 0 {
		issues.add("", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
		return listAddition{}, issues.failure()
	}
	if len(raw.ListId.ProtoReflect().GetUnknown()) != 0 {
		issues.add("list_id", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
	}
	var expected lists.ListRevision
	value := raw.ExpectedListRevision
	if value == nil {
		issues.add("expected_list_revision", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	} else {
		before := len(issues.values)
		if len(value.ProtoReflect().GetUnknown()) != 0 {
			issues.add("expected_list_revision", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
		} else {
			if value.Number == 0 || value.Number > math.MaxInt64 {
				issues.add("expected_list_revision.number", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE)
			}
			bound, err := listID(value.ListId)
			if err != nil || bound != id {
				issues.add("expected_list_revision.list_id", api.ValidationReason_VALIDATION_REASON_INVALID_NAME)
			} else if len(value.ListId.ProtoReflect().GetUnknown()) != 0 {
				issues.add("expected_list_revision.list_id", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
			}
		}
		if len(issues.values) == before {
			var err error
			expected, err = lists.NewListRevision(id, int64(value.Number))
			if err != nil {
				issues.invalidDescriptor = true
			}
		}
	}
	payload := admittedItemPayload(raw.Item, &issues)
	if err := issues.failure(); err != nil {
		return listAddition{}, err
	}
	return listAddition{id, expected, payload}, nil
}

func admittedItemPayload(raw *listsv1.ItemPayload, issues *listInputIssues) lists.ItemPayload {
	if raw == nil {
		issues.add("item", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
		return lists.ItemPayload{}
	}
	if len(raw.ProtoReflect().GetUnknown()) != 0 {
		issues.add("item", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
		return lists.ItemPayload{}
	}
	input := lists.ItemInput{Summary: raw.Summary, Description: raw.Description, Due: lists.NoDue()}
	if raw.Completed == nil {
		issues.add("item.completed", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	} else {
		input.Completed = *raw.Completed
	}
	switch due := raw.Due.(type) {
	case nil:
	case *listsv1.ItemPayload_DueDate:
		if due == nil {
			issues.add("item.due_date", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
		} else {
			value, err := lists.NewDueDate(due.DueDate)
			if err != nil {
				issues.add("item.due_date", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
			} else {
				input.Due = value
			}
		}
	case *listsv1.ItemPayload_DueInstant:
		if due == nil || due.DueInstant == nil {
			issues.add("item.due_instant", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
		} else {
			instant := due.DueInstant
			if len(instant.ProtoReflect().GetUnknown()) != 0 {
				issues.add("item.due_instant", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
			} else if instant.At == nil {
				issues.add("item.due_instant.at", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
			} else if len(instant.At.ProtoReflect().GetUnknown()) != 0 {
				issues.add("item.due_instant.at", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
			} else if instant.At.CheckValid() != nil {
				issues.add("item.due_instant.at", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
			} else {
				value, err := lists.NewDueInstant(instant.At.AsTime(), instant.Zone)
				if err != nil {
					issues.add("item.due_instant", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
				} else {
					input.Due = value
				}
			}
		}
	default:
		issues.add("item", api.ValidationReason_VALIDATION_REASON_UNKNOWN_VALUE)
	}
	if raw.CompletedAt != nil {
		if len(raw.CompletedAt.ProtoReflect().GetUnknown()) != 0 {
			issues.add("item.completed_at", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
		} else if raw.CompletedAt.CheckValid() != nil {
			issues.add("item.completed_at", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
		} else if raw.Completed != nil {
			value := raw.CompletedAt.AsTime()
			input.CompletedAt = &value
		}
	}
	payload, err := lists.NewItemPayload(input)
	if err != nil {
		var invalid *lists.InputValidationError
		if !errors.As(err, &invalid) || invalid == nil {
			issues.invalidDescriptor = true
			return lists.ItemPayload{}
		}
		for _, issue := range invalid.Issues() {
			path := "item"
			if issue.Field() != "" {
				path += "." + issue.Field()
			}
			issues.add(path, issue.Rule())
		}
	}
	return payload
}
