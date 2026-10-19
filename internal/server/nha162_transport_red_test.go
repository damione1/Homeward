package server

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The public list API must have one generated service contract linked into the
// server. This test intentionally starts red before NHA-162 adds that contract.
func TestNHA162ListServiceDescriptorRegistered(t *testing.T) {
	const serviceName protoreflect.FullName = "home.lists.v1.ListService"
	found, err := protoregistry.GlobalFiles.FindDescriptorByName(serviceName)
	if err != nil {
		t.Fatalf("generated ListService descriptor is absent from server: %v", err)
	}
	service, ok := found.(protoreflect.ServiceDescriptor)
	if !ok {
		t.Fatalf("%s is %T, want a service descriptor", serviceName, found)
	}
	for _, name := range []protoreflect.Name{
		"ListLists", "CountLists", "GetList", "CreateList", "RenameList", "DeleteList",
		"ListItems", "CountItems", "GetItem", "AddItem", "EditItem", "DeleteItem",
		"SetCompleted", "MoveItem", "WatchLists",
	} {
		if service.Methods().ByName(name) == nil {
			t.Errorf("ListService is missing %s", name)
		}
	}
	watch := service.Methods().ByName("WatchLists")
	if watch != nil && (!watch.IsStreamingServer() || watch.IsStreamingClient()) {
		t.Errorf("WatchLists must be a server stream")
	}
}
