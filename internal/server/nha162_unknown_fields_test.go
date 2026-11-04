package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	datepb "google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha162UnknownBinaryTransport struct {
	base  http.RoundTripper
	calls atomic.Int32
}

func (t *nha162UnknownBinaryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path == apiv2connect.TaskServiceAppendTaskProcedure || request.URL.Path == apiv2connect.TaskServiceUpdateTaskProcedure {
		if contentType := request.Header.Get("Content-Type"); contentType != "application/proto" {
			return nil, fmt.Errorf("expected binary Connect protobuf, got %q", contentType)
		}
		t.calls.Add(1)
	}
	return t.base.RoundTrip(request)
}

func nha162FutureBytes(message proto.Message, field protowire.Number) {
	unknown := protowire.AppendTag(nil, field, protowire.BytesType)
	unknown = protowire.AppendString(unknown, "synthetic future value")
	message.ProtoReflect().SetUnknown(unknown)
}

func nha162FutureTimestamp(message *timestamppb.Timestamp) {
	unknown := protowire.AppendTag(nil, 3, protowire.VarintType)
	message.ProtoReflect().SetUnknown(protowire.AppendVarint(unknown, 1))
}

func nha162ValidDueInstant() *api.TaskDueInstant {
	return &api.TaskDueInstant{
		DueTime: timestamppb.New(time.Date(2028, 2, 29, 12, 34, 56, 123456000, time.UTC)), TimeZone: "UTC",
	}
}

func nha162AssertInvalidItemAdmission(t *testing.T, err error) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown item payload returned %v, want InvalidArgument: %v", connect.CodeOf(err), err)
		return
	}
	structured, ok := err.(*connect.Error)
	if !ok || len(structured.Details()) < 1 || len(structured.Details()) > 2 {
		t.Error("unknown payload lost its structured validation error:", err)
		return
	}
	if nha200Info(t, err).Reason != "VALIDATION_FAILED" {
		t.Error("canonical validation reason missing")
	}

}

func TestNHA162BinaryUnknownItemPayloadRejectsAddAndEditWithoutMutation(t *testing.T) {
	r := nha162NewRPCRig(t)
	httpClient := r.server.Client()
	transport := &nha162UnknownBinaryTransport{base: httpClient.Transport}
	client := apiv2connect.NewTaskServiceClient(&http.Client{Transport: transport, Timeout: 40 * time.Second}, r.server.URL)
	for _, variant := range []struct {
		name    string
		payload func() *api.Task
	}{
		{"future-due-without-known-due", func() *api.Task {
			payload := &api.Task{Summary: "Future due", Completed: false}
			nha162FutureBytes(payload, 100)
			return payload
		}},
		{"known-date-with-future-due", func() *api.Task {
			payload := &api.Task{Summary: "Future due beside date", Completed: false, Due: &api.Task_DueDate{DueDate: &datepb.Date{Year: 2028, Month: 2, Day: 29}}}
			nha162FutureBytes(payload, 100)
			return payload
		}},
		{"known-instant-with-future-due", func() *api.Task {
			payload := &api.Task{Summary: "Future due beside instant", Completed: false, Due: &api.Task_DueInstant{DueInstant: nha162ValidDueInstant()}}
			nha162FutureBytes(payload, 100)
			return payload
		}},
		{"unknown-due-instant-field", func() *api.Task {
			instant := nha162ValidDueInstant()
			nha162FutureBytes(instant, 3)
			return &api.Task{Summary: "Future instant field", Completed: false, Due: &api.Task_DueInstant{DueInstant: instant}}
		}},
		{"unknown-due-timestamp-field", func() *api.Task {
			instant := nha162ValidDueInstant()
			nha162FutureTimestamp(instant.DueTime)
			return &api.Task{Summary: "Future due timestamp", Completed: false, Due: &api.Task_DueInstant{DueInstant: instant}}
		}},
		{"unknown-civil-date-field", func() *api.Task {
			date := &datepb.Date{Year: 2028, Month: 2, Day: 29}
			nha162FutureBytes(date, 100)
			return &api.Task{Summary: "Future civil date", Due: &api.Task_DueDate{DueDate: date}}
		}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			for _, operation := range []string{"Add", "Edit"} {
				t.Run(operation, func(t *testing.T) {
					created, err := r.tasks.CreateTaskList(r.ctx, nha162Bearer(&api.CreateTaskListRequest{Parent: r.taskHome(t), TaskList: &api.TaskList{DisplayName: "Synthetic unknown fields"}}, r.adminToken))
					if err != nil {
						t.Fatal(err)
					}
					id := created.Msg.Name
					seed, err := client.AppendTask(r.ctx, nha162Bearer(&api.AppendTaskRequest{Name: id, Etag: created.Msg.Etag, Task: &api.TaskContent{Summary: "Keep existing due", Due: &api.TaskContent_DueDate{DueDate: &datepb.Date{Year: 2028, Month: 6, Day: 30}}}}, r.adminToken))
					if err != nil {
						t.Fatal(err)
					}
					beforeList, beforeTask := r.taskList(t, id), r.task(t, seed.Msg.Task.Name)
					unchanged := func() {
						t.Helper()
						if !proto.Equal(r.taskList(t, id), beforeList) {
							t.Error("unknown payload changed list/etag")
						}
						page, e := r.tasks.ListTasks(r.ctx, nha162Bearer(&api.ListTasksRequest{Parent: id}, r.adminToken))
						if e != nil {
							t.Fatal(e)
						}
						if page.Msg.TotalSize != 1 || len(page.Msg.Tasks) != 1 || !proto.Equal(page.Msg.Tasks[0], beforeTask) {
							t.Error("unknown payload changed task/due/etag/count")
						}
					}
					switch operation {
					case "Add":
						raw := variant.payload()
						content := &api.TaskContent{Summary: raw.Summary, Completed: raw.Completed, Description: raw.Description}
						content.ProtoReflect().SetUnknown(raw.ProtoReflect().GetUnknown())
						switch due := raw.Due.(type) {
						case *api.Task_DueDate:
							content.Due = &api.TaskContent_DueDate{DueDate: due.DueDate}
						case *api.Task_DueInstant:
							content.Due = &api.TaskContent_DueInstant{DueInstant: due.DueInstant}
						}
						request := &api.AppendTaskRequest{Name: id, Etag: beforeList.Etag, Task: content}
						encoded, e := proto.Marshal(request)
						if e != nil {
							t.Fatal(e)
						}
						decoded := new(api.AppendTaskRequest)
						if e = proto.Unmarshal(encoded, decoded); e != nil || !proto.Equal(request, decoded) {
							t.Fatal("future fields not preserved", e)
						}
						added, e := client.AppendTask(r.ctx, nha162Bearer(decoded, r.adminToken))
						if added != nil {
							t.Error("unknown payload returned success")
						}
						nha162AssertInvalidItemAdmission(t, e)
					case "Edit":
						raw := variant.payload()
						raw.Name = beforeTask.Name
						raw.Etag = beforeTask.Etag
						request := &api.UpdateTaskRequest{Task: raw, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}}}
						encoded, e := proto.Marshal(request)
						if e != nil {
							t.Fatal(e)
						}
						decoded := new(api.UpdateTaskRequest)
						if e = proto.Unmarshal(encoded, decoded); e != nil || !proto.Equal(request, decoded) {
							t.Fatal("future fields not preserved", e)
						}
						edited, e := client.UpdateTask(r.ctx, nha162Bearer(decoded, r.adminToken))
						if edited != nil {
							t.Error("unknown payload returned success")
						}
						nha162AssertInvalidItemAdmission(t, e)
					}
					unchanged()
				})
			}
		})
	}
	if got := transport.calls.Load(); got != 24 {
		t.Fatalf("binary Connect Add/Edit requests=%d, want 24", got)
	}
}
