package entityfilter_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/entityfilter"
)

type image struct {
	home         apitypes.HomeName
	metadata     apitypes.EntityMetadata
	device       apitypes.DeviceName
	capabilities []apiresources.Capability
	candidate    entityfilter.Candidate
	name         apitypes.EntityName
}

func pointer[T any](value T) *T { return &value }
func fixture(t *testing.T, label string, availability *domain.Availability) image {
	t.Helper()
	home, err := apitypes.ParseHomeName("homes/filter-fixture")
	if err != nil {
		t.Fatal(err)
	}
	entity, err := domain.ParseEntityID("ent_filter_synthetic")
	if err != nil {
		t.Fatal(err)
	}
	device, err := domain.ParseDeviceID("dev_filter_synthetic")
	if err != nil {
		t.Fatal(err)
	}
	deviceName, err := apitypes.DeviceNameForIdentity(home, device)
	if err != nil {
		t.Fatal(err)
	}
	displayName, err := apitypes.NewDisplayName(label)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := apitypes.NewEntityMetadata(entity, displayName, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var observed *domain.ObservedState
	if availability != nil {
		light, err := domain.NewLightValue(false, pointer(0.0), nil)
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewLightStateValue(light)
		if err != nil {
			t.Fatal(err)
		}
		quality, err := domain.NewQuality(*availability, domain.FreshnessStale, domain.ProvenanceRestored)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Unix(1800000000, 0).UTC()
		stamp, err := domain.NewSourceStamp("filter-fixture", pointer(uint64(1)))
		if err != nil {
			t.Fatal(err)
		}
		var current *domain.StateValue
		if *availability == domain.AvailabilityAvailable {
			current = &value
		}
		state, err := domain.NewObservedStateSnapshot(entity, quality, current, domain.StateTimes{ObservedAt: &now, ReceivedAt: now, LastChanged: now, LastUpdated: now, LastReported: now}, stamp, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		observed = &state
	}
	descriptions := []*registry.CapabilityDescription{{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "lamp", DisplayName: "Synthetic source", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100}}}}}
	capabilities, err := apiresources.ProjectCapabilities(home, entity, descriptions, observed)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := entityfilter.NewCandidate(home, metadata, &deviceName, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	name, err := apitypes.EntityNameForIdentity(home, entity)
	if err != nil {
		t.Fatal(err)
	}
	return image{home, metadata, deviceName, capabilities, candidate, name}
}
func match(t *testing.T, input string, candidate entityfilter.Candidate) bool {
	t.Helper()
	filter, err := entityfilter.Parse(input)
	if err != nil {
		t.Fatalf("%q: %v", input, err)
	}
	if !filter.Valid() {
		t.Fatal("successful parse returned invalid filter")
	}
	return filter.Match(candidate)
}
func rejection(t *testing.T, input string, kind entityfilter.ErrorKind) {
	t.Helper()
	filter, err := entityfilter.Parse(input)
	var violation *entityfilter.Error
	if !errors.As(err, &violation) || violation.Kind() != kind || filter.Valid() {
		t.Fatalf("%q: want kind %d, got %v", input, kind, err)
	}
	if strings.Contains(violation.Error(), "PRIVATE-FILTER-CANARY") {
		t.Fatal("error echoed literal")
	}
	if violation.Offset() < 0 || violation.Offset() > len(input) {
		t.Fatal("invalid source byte offset")
	}
}

func TestFiveEntitySelectionsUseOnlyDocumentedFieldsAndFunctions(t *testing.T) {
	f := fixture(t, "North lamp", pointer(domain.AvailabilityAvailable))
	for _, input := range []string{
		`name="` + f.name.String() + `"`,
		`display_name="North lamp"`,
		`device("` + f.device.String() + `")`,
		`trait("power")`,
		`availability(AVAILABLE)`,
		`name:* AND display_name:lamp AND trait("light_control") AND trait("level")`,
	} {
		if !match(t, input, f.candidate) {
			t.Fatalf("did not select %q", input)
		}
	}
	for _, input := range []string{
		`device("homes/filter-fixture/devices/other")`,
		`trait("lock")`,
		`availability(UNAVAILABLE)`,
		`display_name=South`,
		`name="homes/filter-fixture/entities/other"`,
	} {
		if match(t, input, f.candidate) {
			t.Fatalf("unexpected selection %q", input)
		}
	}
}

func TestAIPPrecedenceImplicitConjunctionAndNegation(t *testing.T) {
	f := fixture(t, "B C", nil)
	cases := map[string]bool{
		`A AND B OR C`:   false, // A AND (B OR C), not (A AND B) OR C.
		`A B OR C`:       false,
		`A OR B AND C`:   true,
		`(A AND B) OR C`: true,
		`(A OR B) C`:     true,
		`B C`:            true,
		`B AND C`:        true,
		`NOT A`:          true,
		`-A`:             true,
		`-(A OR B)`:      false,
		`NOT (A AND B)`:  true,
		`NOT (-B)`:       true,
		`NOT (NOT B)`:    true,
		`B OR A AND -A`:  true,
	}
	for input, want := range cases {
		if got := match(t, input, f.candidate); got != want {
			t.Fatalf("%s = %t, want %t", input, got, want)
		}
	}
	rejection(t, `NOT(B)`, entityfilter.Syntax)
	rejection(t, `B AND(C)`, entityfilter.Syntax)
	rejection(t, `(B)(C)`, entityfilter.Syntax)
	rejection(t, `NOT NOT B`, entityfilter.Syntax)
}

func TestStringsUseExactLexicalComparisonAndBoundedWildcards(t *testing.T) {
	f := fixture(t, "North lamp", nil)
	cases := map[string]bool{
		`display_name>"M"`:           true,
		`display_name>="North lamp"`: true,
		`display_name<"Z"`:           true,
		`display_name<="North lamp"`: true,
		`display_name<"North lamp"`:  false,
		`display_name="North*"`:      true,
		`display_name="*lamp"`:       true,
		`display_name="*th la*"`:     true,
		`display_name!="*lamp"`:      false,
		`display_name:"th la"`:       true,
		`display_name:"North*"`:      true,
		`display_name:"*lamp"`:       true,
		`display_name:"north"`:       false,
		`"North lamp"`:               true,
		`North lamp`:                 true,
	}
	for input, want := range cases {
		if got := match(t, input, f.candidate); got != want {
			t.Fatalf("%s = %t, want %t", input, got, want)
		}
	}
	for _, input := range []string{`display_name="No*rth"`, `display_name="***"`} {
		rejection(t, input, entityfilter.InvalidValue)
	}
	rejection(t, `display_name>"North*"`, entityfilter.UnsupportedOperator)
	unicode := fixture(t, "Étage", nil)
	if !match(t, `display_name>"Z"`, unicode.candidate) || match(t, `display_name="Etage"`, unicode.candidate) {
		t.Fatal("comparison normalized or folded Unicode")
	}
}

func TestEmptyLabelPresenceUnknownAvailabilityAndZeroValuesStayIndependent(t *testing.T) {
	empty := fixture(t, "", nil)
	if !match(t, `display_name=""`, empty.candidate) || match(t, `display_name:*`, empty.candidate) || !match(t, `name:*`, empty.candidate) {
		t.Fatal("empty scalar/default presence lost")
	}
	if !match(t, `display_name="*"`, empty.candidate) || match(t, `display_name:*`, empty.candidate) {
		t.Fatal("wildcard equality confused with non-default presence")
	}
	if match(t, `availability(UNKNOWN)`, empty.candidate) || !match(t, `NOT availability(AVAILABLE)`, empty.candidate) {
		t.Fatal("absence fabricated reachability")
	}
	if !match(t, `trait(power) AND trait(level)`, empty.candidate) {
		t.Fatal("unobserved declaration lost its supported traits")
	}
	for _, test := range []struct {
		source domain.Availability
		name   string
	}{
		{domain.AvailabilityAvailable, "AVAILABLE"},
		{domain.AvailabilityUnavailable, "UNAVAILABLE"},
		{domain.AvailabilityUnknown, "UNKNOWN"},
	} {
		f := fixture(t, "Sensor", &test.source)
		if !match(t, `availability(`+test.name+`)`, f.candidate) {
			t.Fatal("OFF/zero/stale/restored changed availability", test.name)
		}
		if match(t, `false`, f.candidate) {
			t.Fatal("bare literal searched observed OFF as a string")
		}
	}
}

func TestRejectsUndeclaredFieldsFunctionsTraversalAndUnknownEnumValues(t *testing.T) {
	for _, input := range []string{`device="x"`, `trait=power`, `availability=AVAILABLE`, `name.foo="x"`, `capabilities.power=true`, `name.0=x`, `displayName=x`, `PRIVATE-FILTER-CANARY=x`} {
		rejection(t, input, entityfilter.UnknownField)
	}
	for _, input := range []string{`unknown("PRIVATE-FILTER-CANARY")`, `Device("x")`} {
		rejection(t, input, entityfilter.UnknownFunction)
	}
	for _, input := range []string{`trait("Power")`, `trait("thermal")`, `trait("PRIVATE-FILTER-CANARY")`} {
		rejection(t, input, entityfilter.UnknownTrait)
	}
	for _, input := range []string{`availability(available)`, `availability(UNSPECIFIED)`, `availability(999)`, `device("homes/demo/entities/e")`, `device("homes/demo/devices/*")`} {
		rejection(t, input, entityfilter.InvalidValue)
	}
	for _, input := range []string{`availability(AVAILABLE)>UNKNOWN`, `trait(power)=true`, `device("homes/demo/devices/d"):*`} {
		rejection(t, input, entityfilter.UnsupportedOperator)
	}
	for _, input := range []string{`trait()`, `availability(AVAILABLE,UNAVAILABLE)`, `name=`, `(`, `"unterminated`, `name==x`, `name=x OR`, `()`, `name[x]=y`} {
		if filter, err := entityfilter.Parse(input); err == nil || filter.Valid() {
			t.Fatalf("invalid syntax accepted %q", input)
		}
	}
}

func TestQuotedEscapesAndCanonicalDigestAreStable(t *testing.T) {
	f := fixture(t, "Étage", nil)
	for _, input := range []string{`display_name="\u00c9tage"`, `display_name='Étage'`, `display_name="Étage"`} {
		if !match(t, input, f.candidate) {
			t.Fatal(input)
		}
	}
	pairs := [][2]string{
		{`display_name="\u00c9tage"`, `display_name='Étage'`},
		{`trait("power") AND name:*`, `name:* trait(power)`},
		{`A AND (B AND C)`, `(C A) AND B`},
		{`A OR (B OR C)`, `C OR A OR B`},
		{`NOT trait(power)`, `-trait("power")`},
		{` `, ``},
	}
	for _, pair := range pairs {
		a, err := entityfilter.Parse(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		b, err := entityfilter.Parse(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		first, _ := a.Digest()
		second, _ := b.Digest()
		if first != second || first == ([32]byte{}) {
			t.Fatalf("equivalent syntax has different digest: %q / %q", pair[0], pair[1])
		}
	}
	a, _ := entityfilter.Parse(`A AND B`)
	b, _ := entityfilter.Parse(`A OR B`)
	x, _ := a.Digest()
	y, _ := b.Digest()
	if x == y {
		t.Fatal("distinct filters share digest")
	}
	for _, input := range []string{`"\uD800"`, `"\uDC00"`, `"\uD800\u0041"`, `"\x41"`, `"\*"`, string([]byte{0xff})} {
		rejection(t, input, entityfilter.InvalidValue)
	}
	emoji := fixture(t, "Sensor 😀", nil)
	if !match(t, `display_name:"\uD83D\uDE00"`, emoji.candidate) {
		t.Fatal("surrogate pair not decoded")
	}
}

func TestBudgetsAndZeroValuesFailClosed(t *testing.T) {
	f := fixture(t, "Sensor", nil)
	rejection(t, strings.Repeat(" ", entityfilter.MaxTextBytes+1), entityfilter.TextLimit)
	if filter, err := entityfilter.Parse(strings.Repeat(" ", entityfilter.MaxTextBytes)); err != nil || !filter.Match(f.candidate) {
		t.Fatal("exact text budget rejected", err)
	}
	rejection(t, strings.Repeat("( ", entityfilter.MaxTokens)+")", entityfilter.TokenLimit)
	allowed := strings.TrimSpace(strings.Repeat("A ", 32))
	if _, err := entityfilter.Parse(allowed); err != nil {
		t.Fatal("63-node expression rejected", err)
	}
	rejection(t, strings.TrimSpace(strings.Repeat("A ", 33)), entityfilter.NodeLimit)
	depth := strings.Repeat("(", entityfilter.MaxDepth) + "A" + strings.Repeat(")", entityfilter.MaxDepth)
	if _, err := entityfilter.Parse(depth); err != nil {
		t.Fatal("exact depth rejected", err)
	}
	rejection(t, "("+depth+")", entityfilter.DepthLimit)
	rejection(t, strings.Repeat("NOT (", entityfilter.MaxDepth)+"A"+strings.Repeat(")", entityfilter.MaxDepth), entityfilter.DepthLimit)
	var zero entityfilter.Filter
	if zero.Valid() || zero.Match(f.candidate) {
		t.Fatal("zero filter matches")
	}
	if _, err := zero.Digest(); err == nil {
		t.Fatal("zero filter has admitted digest")
	}
	all, err := entityfilter.Parse("")
	if err != nil || all.Match(entityfilter.Candidate{}) {
		t.Fatal("invalid candidate matches all filter", err)
	}
}

func TestCandidateOwnsOnlyItsAuthorizedMaterializedImage(t *testing.T) {
	f := fixture(t, "Sensor", pointer(domain.AvailabilityAvailable))
	snapshot := f.candidate
	f.capabilities[0] = apiresources.Capability{}
	f.device, _ = apitypes.ParseDeviceName("homes/filter-fixture/devices/other")
	if !match(t, `trait(power) AND availability(AVAILABLE)`, snapshot) {
		t.Fatal("candidate retained mutable source slice")
	}
	if _, err := entityfilter.NewCandidate(f.home, apitypes.EntityMetadata{}, nil, nil); err == nil {
		t.Fatal("invalid metadata admitted")
	}
	wrong, _ := apitypes.ParseDeviceName("homes/foreign/devices/d")
	if _, err := entityfilter.NewCandidate(f.home, f.metadata, &wrong, nil); err == nil {
		t.Fatal("foreign device admitted")
	}
	if _, err := entityfilter.NewCandidate(f.home, f.metadata, nil, f.capabilities); err == nil {
		t.Fatal("zero capability admitted")
	}
	withoutDevice, err := entityfilter.NewCandidate(f.home, f.metadata, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if match(t, `device("`+f.device.String()+`")`, withoutDevice) {
		t.Fatal("missing association invented a device")
	}
	if match(t, `trait("lock")`, snapshot) {
		t.Fatal("matcher invented a capability absent from authorized image")
	}
}

func TestGeneratedTraitsAndAvailabilityHaveExplicitAdmissionCoverage(t *testing.T) {
	fields := (&api.Capability{}).ProtoReflect().Descriptor().Oneofs().ByName("trait").Fields()
	for i := 0; i < fields.Len(); i++ {
		if _, err := entityfilter.Parse(`trait("` + string(fields.Get(i).Name()) + `")`); err != nil {
			t.Fatal("generated trait lost", err)
		}
	}
	values := api.Availability(0).Descriptor().Values()
	for i := 0; i < values.Len(); i++ {
		value := values.Get(i)
		literal := strings.TrimPrefix(string(value.Name()), "AVAILABILITY_")
		_, err := entityfilter.Parse(`availability(` + literal + `)`)
		if (value.Number() == 0) != (err != nil) {
			t.Fatalf("declared enum requires conscious policy: %s %v", value.Name(), err)
		}
	}
}

func TestDeviceNamesPreserveNativeIdentityAndGrammar(t *testing.T) {
	home, err := apitypes.ParseHomeName("homes/filter-fixture")
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseDeviceID("dev_fixture_identity")
	if err != nil {
		t.Fatal(err)
	}
	name, err := apitypes.DeviceNameForIdentity(home, id)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("dev_fixture_identity"))
	expected := home.String() + "/devices/device-" + hex.EncodeToString(digest[:24])
	if name.String() != expected || name.Home() != home || !name.Valid() {
		t.Fatal("device identity hash/parent differs from SQL")
	}
	for _, input := range []string{"", "/homes/demo/devices/d", "homes/demo/entities/d", "homes/demo/devices/UPPER", "homes/demo/devices/a%2fb", "homes/demo/devices/-a", "homes/demo/devices/" + strings.Repeat("a", 64)} {
		if _, err := apitypes.ParseDeviceName(input); err == nil {
			t.Fatal("invalid device name admitted", input)
		}
	}
	if _, err := apitypes.DeviceNameForIdentity(home, domain.DeviceID{}); err == nil {
		t.Fatal("zero native identity admitted")
	}
	if _, err := apitypes.DeviceNameForIdentity(apitypes.HomeName{}, id); err == nil {
		t.Fatal("zero home admitted")
	}
}

func TestIndependentAuthorizedObservationQualitiesRemainExistential(t *testing.T) {
	f := fixture(t, "Sensor", pointer(domain.AvailabilityAvailable))
	q, err := domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceRestored)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0).UTC()
	source, err := domain.NewSourceStamp("independent-fixture", pointer(uint64(1)))
	if err != nil {
		t.Fatal(err)
	}
	observed, err := domain.NewObservedStateSnapshot(f.metadata.Entity(), q, nil, domain.StateTimes{ObservedAt: &at, ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at}, source, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptions := []*registry.CapabilityDescription{{SchemaId: contracts.BinarySchema, Major: 1, Endpoint: "independent-fact", Constraints: &registry.CapabilityDescription_Binary{Binary: &registry.BinaryConstraints{}}}}
	additional, err := apiresources.ProjectCapabilities(f.home, f.metadata.Entity(), descriptions, &observed)
	if err != nil {
		t.Fatal(err)
	}
	combined := append(append([]apiresources.Capability(nil), f.capabilities...), additional...)
	candidate, err := entityfilter.NewCandidate(f.home, f.metadata, &f.device, combined)
	if err != nil {
		t.Fatal(err)
	}
	if !match(t, `availability(AVAILABLE) AND availability(UNAVAILABLE) AND trait(binary_observation)`, candidate) {
		t.Fatal("independent qualities collapsed into an entity status")
	}
	if match(t, `availability(UNKNOWN)`, candidate) {
		t.Fatal("unavailable/off became unknown")
	}
	// The first image was deliberately readable without the additional source.
	if match(t, `trait(binary_observation) OR availability(UNAVAILABLE)`, f.candidate) {
		t.Fatal("selection consulted a source outside the authorized candidate")
	}
	if _, err := entityfilter.NewCandidate(f.home, f.metadata, nil, append(combined, additional...)); err == nil {
		t.Fatal("duplicate contradictory source image admitted")
	}
	otherID, err := domain.ParseEntityID("ent_other_synthetic")
	if err != nil {
		t.Fatal(err)
	}
	other, err := apitypes.NewEntityMetadata(otherID, f.metadata.Label(), 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entityfilter.NewCandidate(f.home, other, nil, f.capabilities); err == nil {
		t.Fatal("capability from another entity admitted")
	}
}

func FuzzBoundedFilterParse(f *testing.F) {
	for _, input := range []string{"", `trait("power") AND availability(AVAILABLE)`, `NOT (display_name:"sensor" OR name:*)`, `availability(999)`, `name.private="x"`, `"\uD800"`, strings.Repeat("(", 9), string([]byte{0xff})} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		parsed, err := entityfilter.Parse(input)
		if err != nil {
			if parsed.Valid() {
				t.Fatal("invalid syntax admitted")
			}
			return
		}
		if !parsed.Valid() || parsed.Match(entityfilter.Candidate{}) {
			t.Fatal("parse created an unsafe default")
		}
		first, err := parsed.Digest()
		if err != nil {
			t.Fatal(err)
		}
		again, err := entityfilter.Parse(strings.Clone(input))
		if err != nil {
			t.Fatal(err)
		}
		second, err := again.Digest()
		if err != nil || first != second {
			t.Fatal("digest depends on mutable parse state", err)
		}
	})
}
