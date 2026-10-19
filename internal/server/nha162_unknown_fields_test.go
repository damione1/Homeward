package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	listsv1 "github.com/damione1/Homeward/gen/home/lists/v1"
	"github.com/damione1/Homeward/gen/home/lists/v1/listsv1connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha162UnknownBinaryTransport struct {
	base  http.RoundTripper
	calls atomic.Int32
}

func (t *nha162UnknownBinaryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path == listsv1connect.ListServiceAddItemProcedure || request.URL.Path == listsv1connect.ListServiceEditItemProcedure {
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

func nha162ValidDueInstant() *listsv1.DueInstant {
	return &listsv1.DueInstant{
		At: timestamppb.New(time.Date(2028, 2, 29, 12, 34, 56, 123456000, time.UTC)), Zone: "UTC",
	}
}

func nha162AssertInvalidItemAdmission(t *testing.T, err error) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown item payload returned %v, want InvalidArgument: %v", connect.CodeOf(err), err)
		return
	}
	structured, ok := err.(*connect.Error)
	if !ok || len(structured.Details()) != 1 {
		t.Error("unknown payload lost its structured Invalid detail:", err)
		return
	}
	value, decodeErr := structured.Details()[0].Value()
	if decodeErr != nil {
		t.Error(decodeErr)
		return
	}
	detail, ok := value.(*listsv1.ListError)
	if !ok || detail.Code != listsv1.ListErrorCode_LIST_ERROR_CODE_INVALID {
		t.Errorf("unknown payload was not rejected at domain admission: %v", value)
	}
}

func TestNHA162BinaryUnknownItemPayloadRejectsAddAndEditWithoutMutation(t *testing.T) {
	r := nha162NewRPCRig(t)
	httpClient := r.server.Client()
	transport := &nha162UnknownBinaryTransport{base: httpClient.Transport}
	client := listsv1connect.NewListServiceClient(&http.Client{Transport: transport, Timeout: 40 * time.Second}, r.server.URL)
	completed := false
	for _, variant := range []struct {
		name    string
		payload func() *listsv1.ItemPayload
	}{
		{"future-due-without-known-due", func() *listsv1.ItemPayload {
			payload := &listsv1.ItemPayload{Summary: "Future due", Completed: &completed}
			nha162FutureBytes(payload, 7)
			return payload
		}},
		{"known-date-with-future-due", func() *listsv1.ItemPayload {
			payload := &listsv1.ItemPayload{Summary: "Future due beside date", Completed: &completed, Due: &listsv1.ItemPayload_DueDate{DueDate: "2028-02-29"}}
			nha162FutureBytes(payload, 7)
			return payload
		}},
		{"known-instant-with-future-due", func() *listsv1.ItemPayload {
			payload := &listsv1.ItemPayload{Summary: "Future due beside instant", Completed: &completed, Due: &listsv1.ItemPayload_DueInstant{DueInstant: nha162ValidDueInstant()}}
			nha162FutureBytes(payload, 7)
			return payload
		}},
		{"unknown-due-instant-field", func() *listsv1.ItemPayload {
			instant := nha162ValidDueInstant()
			nha162FutureBytes(instant, 3)
			return &listsv1.ItemPayload{Summary: "Future instant field", Completed: &completed, Due: &listsv1.ItemPayload_DueInstant{DueInstant: instant}}
		}},
		{"unknown-due-timestamp-field", func() *listsv1.ItemPayload {
			instant := nha162ValidDueInstant()
			nha162FutureTimestamp(instant.At)
			return &listsv1.ItemPayload{Summary: "Future due timestamp", Completed: &completed, Due: &listsv1.ItemPayload_DueInstant{DueInstant: instant}}
		}},
		{"unknown-completed-timestamp-field", func() *listsv1.ItemPayload {
			checked := true
			at := nha162ValidDueInstant().At
			nha162FutureTimestamp(at)
			return &listsv1.ItemPayload{Summary: "Future completion timestamp", Completed: &checked, CompletedAt: at}
		}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			for _, operation := range []string{"Add", "Edit"} {
				t.Run(operation, func(t *testing.T) {
					created, err := r.client.CreateList(r.ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Synthetic unknown fields"}, r.adminToken))
					if err != nil {
						t.Fatal(err)
					}
					id := created.Msg.Receipt.ListId
					seed, err := client.AddItem(r.ctx, nha162Bearer(&listsv1.AddItemRequest{
						ListId: id, ExpectedListRevision: created.Msg.Receipt.ListRevision,
						Item: &listsv1.ItemPayload{Summary: "Keep existing due", Completed: &completed, Due: &listsv1.ItemPayload_DueDate{DueDate: "2028-06-30"}},
					}, r.adminToken))
					if err != nil {
						t.Fatal("valid date payload did not pass binary admission:", err)
					}
					beforeList, err := r.client.GetList(r.ctx, nha162Bearer(&listsv1.GetListRequest{ListId: id}, r.adminToken))
					if err != nil {
						t.Fatal(err)
					}
					beforeItem, err := r.client.GetItem(r.ctx, nha162Bearer(&listsv1.GetItemRequest{ListId: id, ItemUid: seed.Msg.Receipt.ItemUid}, r.adminToken))
					if err != nil {
						t.Fatal(err)
					}
					unchanged := func() {
						t.Helper()
						list, err := r.client.GetList(r.ctx, nha162Bearer(&listsv1.GetListRequest{ListId: id}, r.adminToken))
						if err != nil {
							t.Fatal(err)
						}
						if !proto.Equal(list.Msg.List, beforeList.Msg.List) {
							t.Error("unknown payload changed durable list metadata/revision")
						}
						items, err := r.client.ListItems(r.ctx, nha162Bearer(&listsv1.ListItemsRequest{ListId: id, PageSize: 2}, r.adminToken))
						if err != nil {
							t.Fatal(err)
						}
						if len(items.Msg.Items) != 1 {
							t.Errorf("unknown payload changed durable item rows: got %d, want 1", len(items.Msg.Items))
						} else if !proto.Equal(items.Msg.Items[0], beforeItem.Msg.Item) {
							t.Errorf("unknown payload edited durable item/due/revision: due_date=%q, want %q", items.Msg.Items[0].Value.GetDueDate(), beforeItem.Msg.Item.Value.GetDueDate())
						}
						count, err := r.client.CountItems(r.ctx, nha162Bearer(&listsv1.CountItemsRequest{ListId: id}, r.adminToken))
						if err != nil {
							t.Fatal(err)
						}
						if count.Msg.Count != 1 {
							t.Errorf("unknown payload changed durable item count: got %d, want 1", count.Msg.Count)
						}
					}
					switch operation {
					case "Add":
						add := &listsv1.AddItemRequest{ListId: id, ExpectedListRevision: seed.Msg.Receipt.ListRevision, Item: variant.payload()}
						encoded, err := proto.Marshal(add)
						if err != nil {
							t.Fatal("future fields were not protobuf encodable:", err)
						}
						decodedAdd := new(listsv1.AddItemRequest)
						if err := proto.Unmarshal(encoded, decodedAdd); err != nil || !proto.Equal(add, decodedAdd) {
							t.Fatal("generated Add transport did not preserve decodable future fields:", err)
						}
						added, err := client.AddItem(r.ctx, nha162Bearer(decodedAdd, r.adminToken))
						if added != nil {
							t.Error("unknown payload returned an Add success/receipt")
						}
						nha162AssertInvalidItemAdmission(t, err)
					case "Edit":
						edit := &listsv1.EditItemRequest{
							ListId: id, ItemUid: seed.Msg.Receipt.ItemUid,
							ExpectedListRevision: seed.Msg.Receipt.ListRevision, ExpectedItemRevision: seed.Msg.Receipt.ItemRevision,
							Item: variant.payload(),
						}
						encoded, err := proto.Marshal(edit)
						if err != nil {
							t.Fatal(err)
						}
						decodedEdit := new(listsv1.EditItemRequest)
						if err := proto.Unmarshal(encoded, decodedEdit); err != nil || !proto.Equal(edit, decodedEdit) {
							t.Fatal("generated Edit transport did not preserve decodable future fields:", err)
						}
						edited, err := client.EditItem(r.ctx, nha162Bearer(decodedEdit, r.adminToken))
						if edited != nil {
							t.Error("unknown payload returned an Edit success/receipt")
						}
						nha162AssertInvalidItemAdmission(t, err)
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
