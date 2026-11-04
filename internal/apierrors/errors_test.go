package apierrors

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
)

func TestCanonicalReasonsDescriptorCoverage(t *testing.T) {
	values := api.ApiErrorReason(0).Descriptor().Values()
	for i := 1; i < values.Len(); i++ {
		reason := api.ApiErrorReason(values.Get(i).Number())
		t.Run(reason.String(), func(t *testing.T) {
			fault := New(reason)
			if fault.Reason() != reason {
				t.Fatal("declared reason lacks a deliberate code/message mapping")
			}
			status := fault.Status()
			if status.Code == 0 || status.Code > int32(code.Code_UNAUTHENTICATED) {
				t.Fatal("invalid canonical code")
			}
			if status.Message == "" || len(status.Message) > 256 || proto.Size(status) > MaxDetailsBytes {
				t.Fatal("unbounded or absent safe diagnostic")
			}
			seen := make(map[string]bool)
			for _, detail := range status.Details {
				if seen[detail.TypeUrl] {
					t.Fatal("duplicate detail type")
				}
				seen[detail.TypeUrl] = true
				if _, err := detail.UnmarshalNew(); err != nil {
					t.Fatal(err)
				}
			}
			var info errdetails.ErrorInfo
			if err := status.Details[0].UnmarshalTo(&info); err != nil {
				t.Fatal(err)
			}
			if info.Domain != Domain() || info.Reason != strings.TrimPrefix(reason.String(), "API_ERROR_REASON_") {
				t.Fatal("reason/domain drift")
			}
			if reason == api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE {
				var request errdetails.RequestInfo
				if err := status.Details[len(status.Details)-1].UnmarshalTo(&request); err != nil {
					t.Fatal(err)
				}
				if bytes, err := hex.DecodeString(request.RequestId); err != nil || len(bytes) != 16 || request.ServingData != "" {
					t.Fatal("invalid support reference")
				}
			}
			if int32(fault.Connect().Code()) != status.Code {
				t.Fatal("native Connect code differs from logical Status")
			}
		})
	}
	for _, fault := range []Fault{{}, New(api.ApiErrorReason(9001))} {
		if fault.Code() != code.Code_INTERNAL || fault.Reason() != api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE {
			t.Fatal("unknown/zero fault became a successful or retryable response")
		}
	}
}

func TestAdmittedCorpusMatchesServerCodesAndReasons(t *testing.T) {
	data, err := os.ReadFile("../../contracts/api_error_cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 5 {
			t.Fatal("invalid shared case")
		}
		if parts[2] != "true" {
			continue
		} // Malformed remote errors are client admission cases.
		t.Run(parts[0], func(t *testing.T) {
			bytes, err := hex.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var status statuspb.Status
			if err := proto.Unmarshal(bytes, &status); err != nil {
				t.Fatal(err)
			}
			number, known := api.ApiErrorReason_value["API_ERROR_REASON_"+parts[3]]
			if !known {
				t.Fatal("fixture reason outside generated vocabulary")
			}
			expected, err := strconv.Atoi(parts[4])
			if err != nil {
				t.Fatal(err)
			}
			server := New(api.ApiErrorReason(number)).Status()
			if status.Code != int32(expected) || server.Code != status.Code {
				t.Fatal("server canonical code drift")
			}
			var actual, fixture errdetails.ErrorInfo
			if err := server.Details[0].UnmarshalTo(&actual); err != nil {
				t.Fatal(err)
			}
			if err := status.Details[0].UnmarshalTo(&fixture); err != nil {
				t.Fatal(err)
			}
			if actual.Domain != fixture.Domain || actual.Reason != fixture.Reason {
				t.Fatal("server domain/reason drift")
			}
		})
	}
}

func TestViolationsAreAdmittedSortedBoundedAndImmutable(t *testing.T) {
	root := (&api.ListEntitiesResponse{}).ProtoReflect().Descriptor()
	var inputs []Violation
	for i := 100; i >= 0; i-- {
		value, err := NewViolation(root, fmt.Sprintf("entities[%d].display_name", i), api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, value, value)
	}
	fault := Validation(inputs...)
	if len(fault.Violations()) != MaxViolations {
		t.Fatal("violations not capped")
	}
	status := fault.Status()
	var info errdetails.ErrorInfo
	var bad errdetails.BadRequest
	if err := status.Details[0].UnmarshalTo(&info); err != nil {
		t.Fatal(err)
	}
	if err := status.Details[1].UnmarshalTo(&bad); err != nil {
		t.Fatal(err)
	}
	if len(info.Metadata) != 1 || info.Metadata["violationsTruncated"] != "true" || len(bad.FieldViolations) != MaxViolations || proto.Size(status) > MaxDetailsBytes {
		t.Fatal("invalid detail budget/truncation")
	}
	for i, value := range bad.FieldViolations {
		if i > 0 && bad.FieldViolations[i-1].Field >= value.Field {
			t.Fatal("non-canonical sorting or duplicate")
		}
		if value.Description != "Value does not meet the documented constraint." {
			t.Fatal("dynamic description escaped")
		}
	}
	copy := fault.Violations()
	copy[0] = Violation{}
	inputs[0] = Violation{}
	if fault.Violations()[0].rule == 0 {
		t.Fatal("caller mutated admitted violations")
	}
	status.Details[0].Value = nil
	if len(fault.Status().Details[0].Value) == 0 {
		t.Fatal("caller mutated future errors")
	}
}

func TestFieldPathsComeFromDescriptors(t *testing.T) {
	root := (&api.UpdateTaskRequest{}).ProtoReflect().Descriptor()
	for _, path := range []string{"task.completed", "task.due_instant.due_time", "task.etag", ""} {
		if _, err := NewViolation(root, path, api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE); err != nil {
			t.Fatalf("valid field %q: %v", path, err)
		}
	}
	for _, path := range []string{"task[0]", "task.secret", "task.due", "task..summary", "task['private-value']", "task.summary.value", strings.Repeat("a", 257)} {
		if _, err := NewViolation(root, path, api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE); err == nil {
			t.Fatalf("non-field admitted: %q", path)
		}
	}
	if _, err := NewViolation(root, "task", api.ValidationReason(1000)); err == nil {
		t.Fatal("unknown rule admitted")
	}
	if _, err := NewViolation(root, "task", 0); err == nil {
		t.Fatal("unspecified rule admitted")
	}
}
