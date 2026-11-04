package server

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The public list API must have one generated service contract linked into the
// server; the retired v1 descriptor is not a substitute.
func TestNHA162TaskServiceDescriptorRegistered(t *testing.T) {
	const serviceName protoreflect.FullName = "home.api.v2.TaskService"
	found, err := protoregistry.GlobalFiles.FindDescriptorByName(serviceName)
	if err != nil {
		t.Fatalf("generated TaskService descriptor is absent from server: %v", err)
	}
	service, ok := found.(protoreflect.ServiceDescriptor)
	if !ok {
		t.Fatalf("%s is %T, want a service descriptor", serviceName, found)
	}
	for _, name := range []protoreflect.Name{
		"ListTaskLists", "GetTaskList", "CreateTaskList", "UpdateTaskList", "DeleteTaskList",
		"ListTasks", "GetTask", "AppendTask", "UpdateTask", "DeleteTask", "MoveTask", "WatchTaskLists",
	} {
		if service.Methods().ByName(name) == nil {
			t.Errorf("TaskService is missing %s", name)
		}
	}
	watch := service.Methods().ByName("WatchTaskLists")
	if watch != nil && (!watch.IsStreamingServer() || watch.IsStreamingClient()) {
		t.Errorf("WatchTaskLists must be a server stream")
	}
}

// A retired descriptor can expose an obsolete public contract through reflection
// even when no handler is registered. Keep all four replaced families absent.
func TestNHA202RetiredBusinessServicesAbsentFromDescriptors(t *testing.T) {
	for _, name := range []protoreflect.FullName{
		"home.lists.v1.ListService",
		"home.calendars.v1.CalendarService",
		"home.cameras.v1.CameraService",
		"home.media.v1.MediaService",
	} {
		if descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(name); !errors.Is(err, protoregistry.NotFound) {
			t.Errorf("retired service %s remains linked: descriptor=%v error=%v", name, descriptor, err)
		}
	}
}
