package server

import (
	"errors"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/lists"
	"github.com/damione1/Homeward/internal/tasknames"
)

// Called only under withTaskList's retained authorization permit. Independent
// field errors are collected without exposing a forbidden object's diagnostics.
func taskAddition(raw *api.AppendTaskRequest, name tasknames.ListName) (lists.ListPrecondition, lists.ItemPayload, error) {
	var issues []apierrors.Violation
	descriptorInvalid := false
	add := func(path string, rule api.ValidationReason) {
		v, err := apierrors.NewViolation((&api.AppendTaskRequest{}).ProtoReflect().Descriptor(), path, rule)
		if err != nil {
			descriptorInvalid = true
			return
		}
		issues = append(issues, v)
	}
	if !taskKnown(raw) {
		return nil, lists.ItemPayload{}, taskViolation(&api.AppendTaskRequest{}, "", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
	}
	rev, err := taskListRevision(name, raw.Etag)
	if err != nil {
		rule := api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT
		if raw.Etag == "" {
			rule = api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE
		}
		add("etag", rule)
	}
	var payload lists.ItemPayload
	if raw.Task == nil {
		add("task", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE)
	} else if !taskKnown(raw.Task) {
		add("task", api.ValidationReason_VALIDATION_REASON_UNKNOWN_FIELD)
	} else {
		input, inputErr := taskAppendInput(raw.Task)
		if inputErr != nil {
			// Preserve independent text validation when the due representation is bad.
			input = lists.ItemInput{Summary: raw.Task.Summary, Completed: raw.Task.Completed, Description: raw.Task.Description, Due: lists.NoDue()}
			path := "task"
			switch raw.Task.Due.(type) {
			case *api.TaskContent_DueDate:
				path = "task.due_date"
			case *api.TaskContent_DueInstant:
				path = "task.due_instant"
			}
			add(path, api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
		}
		payload, err = lists.NewItemPayload(input)
		if err != nil {
			var invalid *lists.InputValidationError
			if !errors.As(err, &invalid) {
				descriptorInvalid = true
			} else {
				for _, issue := range invalid.Issues() {
					path := "task"
					if issue.Field() != "" {
						path += "." + issue.Field()
					}
					add(path, issue.Rule())
				}
			}
		}
	}
	if descriptorInvalid {
		return nil, lists.ItemPayload{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
	}
	if len(issues) > 0 {
		return nil, lists.ItemPayload{}, apierrors.Validation(issues...).Connect()
	}
	return rev, payload, nil
}
