package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func TestNHA137ColdScopeSelectionIsTypedAndStrict(t *testing.T) {
	input := "listen: '127.0.0.1:0'\n" + testPostgres(t)
	for _, tc := range []struct {
		name, selector string
		want           scope.Revision
		entities       int
	}{
		{"omitted", "", scope.BaselineV1(), 850},
		{"baseline", "scope_revision: v1\n", scope.BaselineV1(), 850},
		{"amendment", "scope_revision: ecobee_mvp_v1\n", scope.EcobeeMVPV1(), 851},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := config.ParseWithEnv([]byte(input+tc.selector), testLookup)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := parsed.ScopeRevision()
			if err != nil || revision != tc.want {
				t.Fatalf("revision=%v err=%v", revision, err)
			}
			manifest, err := registry.LoadManifest(revision)
			if err != nil {
				t.Fatal(err)
			}
			counts, ok := manifest.Counts()
			if !ok || counts.Entities != tc.entities {
				t.Fatalf("loaded selection=%+v ok=%t", counts, ok)
			}
		})
	}
	for _, selector := range []string{
		"scope_revision: null\n", "scope_revision:\n", "scope_revision: ''\n",
		"scope_revision: 1\n", "scope_revision: true\n", "scope_revision: []\n",
		"scope_revision: {}\n", "scope_revision: [v1]\n", "scope_revision: V1\n",
		"scope_revision: v2\n", "scope_revision: ' v1'\n", "scope_revision: ecobee\n",
		"scope_revision: !!int 1\n", "scope_revision: v1\nscope_revision: ecobee_mvp_v1\n",
	} {
		t.Run(selector, func(t *testing.T) {
			parsed, err := config.ParseWithEnv([]byte(input+selector), testLookup)
			if err == nil {
				t.Fatal("invalid selector admitted")
			}
			if _, err := parsed.ScopeRevision(); !errors.Is(err, config.ErrUninitialized) {
				t.Fatalf("failed parse granted scope: %v", err)
			}
		})
	}
	aliasInput := strings.Replace(input, "listen: '127.0.0.1:0'", "listen: &endpoint '127.0.0.1:0'", 1) + "scope_revision: *endpoint\n"
	if _, err := config.ParseWithEnv([]byte(aliasInput), testLookup); err == nil {
		t.Fatal("YAML alias selector admitted")
	}
	if _, err := (config.Config{}).ScopeRevision(); !errors.Is(err, config.ErrUninitialized) {
		t.Fatalf("zero config granted scope: %v", err)
	}
}
