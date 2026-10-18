package registry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
)

func TestNHA069ConfiguredWireIdentifierSelectionAndColdTopicHistory(t *testing.T) {
	store, ctx, _ := nha015Store(t)
	repo := nha015Repository(t, store)
	instance := nha015Instance(t, "nha069-registry")
	key := nha015Source(t, "zigbee2mqtt-configured-id", "wire-unique-id")
	identity, err := repo.ConfigureBinding(ctx, instance, key, "light.table_lamp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MatchMQTTSource(ctx, instance, []string{"unconfigured-id"}, "wire-unique-id"); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("unconfigured wire id: %v", err)
	}
	chosen, err := repo.MatchMQTTSource(ctx, instance, []string{"unconfigured-id", "zigbee2mqtt-configured-id", "later-id"}, "wire-unique-id")
	if err != nil || chosen != key {
		t.Fatalf("configured wire id not selected: %v %v", chosen, err)
	}
	topic := "homeassistant/light/synthetic-lamp/config"
	if err := repo.RememberMQTTTopics(ctx, instance, topic, []integrations.SourceKey{key}); err != nil {
		t.Fatal(err)
	}
	// A newly constructed repository has no per-session topic map. SQL alone
	// supplies historical provenance, but it still cannot mint an identity.
	cold := nha015Repository(t, store)
	previous, err := cold.MQTTTopicSources(ctx, instance, topic)
	if err != nil || len(previous) != 1 || previous[0] != key {
		t.Fatalf("cold topic provenance: %v %v", previous, err)
	}
	if err := cold.Tombstone(ctx, identity.Entity); err != nil {
		t.Fatal(err)
	}
	if _, err := cold.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrTombstoned) {
		t.Fatalf("retired source resolved: %v", err)
	}
	alias, err := cold.LookupAlias(ctx, "light.table_lamp")
	if err != nil || !alias.Retired || alias.Entity != identity.Entity {
		t.Fatalf("historical alias: %+v %v", alias, err)
	}
	remaining, err := cold.MQTTTopicSources(context.Background(), instance, topic)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("retired source remained active in topic: %v %v", remaining, err)
	}
}
