package template

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

func nha044Alias(t *testing.T, name string) domain.HAAlias {
	t.Helper()
	a, err := domain.ParseHAAlias(name)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func nha044Source(t *testing.T, n int, text *string, quality domain.Quality) ReadSource {
	t.Helper()
	id, err := domain.ParseEntityID(fmt.Sprintf("entity-%d", n))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewReadSource(id, []domain.HAAlias{nha044Alias(t, fmt.Sprintf("sensor.item_%d", n))}, text, nil, quality)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func nha044Quality(t *testing.T, availability domain.Availability) domain.Quality {
	t.Helper()
	q, err := domain.NewQuality(availability, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestNHA044ReadViewBeyondLegacyInputAndCopies(t *testing.T) {
	zero := "0"
	sources := make([]ReadSource, 129)
	for i := range sources {
		sources[i] = nha044Source(t, i, &zero, nha044Quality(t, domain.AvailabilityAvailable))
	}
	view, err := NewReadView(sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewInputs(make([]State, 129), nil, time.Now(), time.UTC); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("legacy input cap: %v", err)
	}
	zero = "changed"
	sources[128].aliases[0] = nha044Alias(t, "sensor.corrupt")
	in, err := NewInputsFromReadView(view, nil, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile("{{ states('sensor.item_128') }}")
	if err != nil {
		t.Fatal(err)
	}
	r, err := EvaluateTracked(context.Background(), p, in, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	result, ok := r.Result()
	if !ok || result.Text() != "0" || len(r.Reads()) != 1 || !r.Reads()[0].Present() {
		t.Fatalf("read view lost present zero: result=%q ok=%v reads=%+v", result.Text(), ok, r.Reads())
	}
}

func TestNHA044ExecutedTraceAndPartialError(t *testing.T) {
	on := "on"
	sources := []ReadSource{nha044Source(t, 1, &on, nha044Quality(t, domain.AvailabilityAvailable))}
	view, err := NewReadView(sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	in, err := NewInputsFromReadView(view, nil, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile("{{ states('sensor.item_1') or states('sensor.item_2') }}")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := EvaluateTracked(context.Background(), p, in, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(tracked.Dependencies()) != 1 || len(p.StaticDependencies()) != 2 || tracked.Steps() == 0 {
		t.Fatalf("executed/static trace mismatch: active=%v static=%v steps=%d", tracked.Dependencies(), p.StaticDependencies(), tracked.Steps())
	}
	p, err = Compile("{{ states('sensor.item_1') }}{{ 1 - 2 }}")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err = EvaluateTracked(context.Background(), p, in, DefaultLimits())
	if !errors.Is(err, ErrEvaluation) || len(tracked.Dependencies()) != 1 {
		t.Fatalf("partial trace lost on evaluation error: %v %+v", err, tracked.Dependencies())
	}
	if _, ok := tracked.Result(); ok {
		t.Fatal("partial render admitted as result")
	}
}

func TestNHA044UnknownUnavailableAndGroupMembership(t *testing.T) {
	unknown := nha044Source(t, 1, nil, nha044Quality(t, domain.AvailabilityUnknown))
	unavailable := nha044Source(t, 2, nil, nha044Quality(t, domain.AvailabilityUnavailable))
	group, err := NewReadGroup(nha044Alias(t, "group.test"), []domain.HAAlias{nha044Alias(t, "sensor.item_1"), nha044Alias(t, "sensor.missing")})
	if err != nil {
		t.Fatal(err)
	}
	view, err := NewReadView([]ReadSource{unknown, unavailable}, []ReadGroup{group})
	if err != nil {
		t.Fatal(err)
	}
	in, err := NewInputsFromReadView(view, nil, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile("{{ states('sensor.item_1') }}|{{ states('sensor.item_2') }}|{{ expand('group.test') | map(attribute='entity_id') | list }}")
	if err != nil {
		t.Fatal(err)
	}
	r, err := EvaluateTracked(context.Background(), p, in, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	result, ok := r.Result()
	if !ok || result.Text() == "" || len(r.Dependencies()) < 4 {
		t.Fatalf("incomplete read: %q %v", result.Text(), r.Dependencies())
	}
	if len(r.Reads()) < 4 || r.Reads()[0].Present() || r.Reads()[1].Present() {
		t.Fatalf("unavailable became present: %+v", r.Reads())
	}
}

func TestNHA044MinuteOnlyWhenExecuted(t *testing.T) {
	in, err := NewInputs(nil, nil, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile("{{ 'on' or now() }}")
	if err != nil {
		t.Fatal(err)
	}
	r, err := EvaluateTracked(context.Background(), p, in, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Dependencies()) != 0 || len(p.StaticDependencies()) != 1 {
		t.Fatalf("inactive minute timer: %v %v", r.Dependencies(), p.StaticDependencies())
	}
}

func TestNHA044TypedDependencyDomainAndAliasAdmission(t *testing.T) {
	if (Dependency{}).Valid() {
		t.Fatal("zero dependency admitted")
	}
	for _, raw := range []string{"", "Bad", "sensor.bad", "switch.*"} {
		if _, err := ParseDomainName(raw); err == nil {
			t.Fatalf("invalid domain admitted: %q", raw)
		}
	}
	domainName, err := ParseDomainName("custom_domain_2")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewDomainCatalogDependency(domainName)
	if err != nil || !catalog.Valid() || catalog.Kind() != DependencyDomainCatalog {
		t.Fatalf("catalogue boundary: %+v %v", catalog, err)
	}
	if name, ok := catalog.Domain(); !ok || name != domainName {
		t.Fatalf("catalogue identity: %+v %v", name, ok)
	}
	if _, ok := catalog.Alias(); ok {
		t.Fatal("domain catalogue fabricated alias")
	}
	if _, err := newAliasDependency(DependencyState, "sensor.bad-name", ""); err == nil {
		t.Fatal("malformed alias entered trace")
	}
	state, err := newAliasDependency(DependencyState, "sensor.good", "")
	if err != nil || !state.Valid() {
		t.Fatalf("alias admission: %+v %v", state, err)
	}
	if alias, ok := state.Alias(); !ok || alias != nha044Alias(t, "sensor.good") {
		t.Fatalf("typed alias: %+v %v", alias, ok)
	}
	if _, err := newAliasDependency(DependencyAttribute, "sensor.good", ""); err == nil {
		t.Fatal("empty attribute entered trace")
	}
}
