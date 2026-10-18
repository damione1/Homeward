package domain

import (
	"reflect"
	"testing"
)

func TestNHA022CommandAndActorIdentities(t *testing.T) {
	if _, err := (CommandID{}).Value(); err == nil {
		t.Fatal("zero CommandID admitted")
	}
	if _, err := (ActorID{}).Value(); err == nil {
		t.Fatal("zero ActorID admitted")
	}
	if _, err := ParseCommandID(""); err == nil {
		t.Fatal("empty CommandID admitted")
	}
	if _, err := ParseActorID(""); err == nil {
		t.Fatal("empty ActorID admitted")
	}
	command, err := ParseCommandID("intent-1")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := ParseActorID("server-actor")
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := command.Value(); value != "intent-1" {
		t.Fatal("command identity lost")
	}
	if value, _ := actor.Value(); value != "server-actor" {
		t.Fatal("actor identity lost")
	}
	for _, other := range []any{ActionID{}, ActorID{}, EntityID{}, DeviceID{}} {
		if reflect.TypeOf(CommandID{}).ConvertibleTo(reflect.TypeOf(other)) {
			t.Fatalf("CommandID converts to %T", other)
		}
	}
	if reflect.TypeOf(ActorID{}).ConvertibleTo(reflect.TypeOf(ActionID{})) {
		t.Fatal("ActorID converts to ActionID")
	}
}
