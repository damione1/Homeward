package config_test

import (
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/workday"
)

func TestNHA102ConfiguredWorkdayAccepted(t *testing.T) {
	input := "listen: '127.0.0.1:0'\n" + testPostgres(t) + "workday: {timezone: America/Toronto}\n"
	parsed, err := config.ParseWithEnv([]byte(input), testLookup)
	if err != nil {
		t.Fatalf("valid Workday profile with explicit zone was rejected: %v", err)
	}
	settings, present := parsed.WorkdaySettings()
	if !present || !settings.Valid() {
		t.Fatal("admitted profile was lost")
	}
}

func TestNHA102OnlyRetainedProfileAccepted(t *testing.T) {
	base := "listen: '127.0.0.1:0'\n" + testPostgres(t)
	without, err := config.ParseWithEnv([]byte(base), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := without.WorkdaySettings(); present {
		t.Fatal("workday appeared without configuration")
	}
	for _, section := range []string{
		"workday: null", "workday: {}", "workday: []", "workday: {timezone: ''}",
		"workday: {timezone: Local}", "workday: {timezone: Unknown/Zone}",
		"workday: {timezone: America/Toronto, subdivision: QC}",
		"workday: {timezone: America/Toronto, country: CA}",
		"workday: {timezone: America/Toronto, excludes: [sat, sun, holiday]}",
		"workday: {timezone: America/Toronto, days_offset: 1}",
		"workday: {timezone: America/Toronto, timezone: UTC}",
	} {
		_, err := config.ParseWithEnv([]byte(base+section+"\n"), testLookup)
		if err == nil {
			t.Fatalf("unsupported or malformed profile admitted: %s", section)
		}
		if section == "workday: {timezone: Local}" {
			var profileError *workday.Error
			if !errors.As(err, &profileError) || profileError.Code() != workday.InvalidProfile {
				t.Fatalf("invalid zone lost typed profile diagnostic: %v", err)
			}
		}
		if section == "workday: {timezone: America/Toronto, subdivision: QC}" ||
			section == "workday: {timezone: America/Toronto, country: CA}" ||
			section == "workday: {timezone: America/Toronto, excludes: [sat, sun, holiday]}" ||
			section == "workday: {timezone: America/Toronto, days_offset: 1}" {
			if !errors.Is(err, config.ErrUnsupportedWorkdayProfile) {
				t.Fatalf("unsupported profile lost typed diagnostic: %v", err)
			}
		}
	}
}
