package server

import (
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"testing"
)

func TestNHA055SourceResolverPreservesRegistryAliasesAndRefusesInventedDeviceOrCalendar(t *testing.T) {
	_, manifest := nha055SemanticCatalog(t)
	record, entity := nha055SemanticRecord(t, "switch.arc_soundbar_crossfade", false, "switch.nha055_renamed")
	retired, _ := nha055SemanticRecord(t, "switch.arc_soundbar_crossfade", true)
	// Only the configured remote bridge may supply a calendar source.
	calendar, err := domain.ParseHAAlias(remoteCalendarAlias)
	if err != nil {
		t.Fatal(err)
	}
	source, err := automationtriggers.ParseCalendarSourceID("nha055-actual-remote-source")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := sourceRuntimeResolverFromRecords([]integrations.RegistryRecord{record}, manifest, map[domain.HAAlias]automationtriggers.CalendarSourceID{calendar: source})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"switch.arc_soundbar_crossfade", "switch.nha055_renamed"} {
		alias, err := domain.ParseHAAlias(name)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := resolver.ResolveEntity(alias)
		if err != nil || resolved != entity {
			t.Fatal("historical identity not retained", err)
		}
	}
	resolved, err := resolver.ResolveCalendar(calendar)
	if err != nil || resolved != source {
		t.Fatal("composed calendar source lost", err)
	}
	empty, err := sourceRuntimeResolverFromRecords(nil, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.ResolveCalendar(calendar); err == nil {
		t.Fatal("unconfigured calendar gained source authority")
	}
	if _, err := resolver.ResolveDevice(ir.HADeviceRef{}); err == nil {
		t.Fatal("HA device reference synthesized a Homeward UUID")
	}
	retiredResolver, err := sourceRuntimeResolverFromRecords([]integrations.RegistryRecord{retired}, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	original, err := domain.ParseHAAlias("switch.arc_soundbar_crossfade")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retiredResolver.ResolveEntity(original); err == nil {
		t.Fatal("retired registry identity gained executable resolution")
	}
}
