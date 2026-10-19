package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/authorization"
)

func nha059Source(t *testing.T, id, url, entry string) Source {
	t.Helper()
	dashboardID, err := authorization.ParseDashboardID(id)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(dashboardID, url, entry)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func nha059ImportBytes(t *testing.T, data []byte) Result {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dashboard.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Import(root, nha059Source(t, "dashboard_test", "test-dashboard", "dashboard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

const nha059Supported = `# source comments survive
views:
- title: 'Main'
  type: sections
  path: main
  max_columns: 3
  visible: false
  sections:
  - type: grid
    column_span: 1
    row_span: 2
    visibility:
    - condition: state
      entity: binary_sensor.occupied
      state: 'on'
    cards:
    - type: heading
      heading: Lights
      heading_style: subtitle
    - type: horizontal-stack
      cards:
      - type: tile
        entity: light.kitchen
        name: 'Kitchen'
        vertical: false
        hide_state: false
        grid_options: {columns: full, rows: auto}
        tap_action: {action: toggle}
        hold_action: {action: more-info}
        double_tap_action: {action: navigate, navigation_path: /lovelace/main}
`

func TestNHA059SupportedProfileAndCapabilities(t *testing.T) {
	r := nha059ImportBytes(t, []byte(nha059Supported))
	if r.Status() != StatusSupported || len(r.Diagnostics()) != 0 {
		t.Fatalf("status=%v diagnostics=%v", r.Status(), r.Diagnostics())
	}
	d, ok := r.Definition()
	if !ok || d.Profile() != Profile || len(d.Views()) != 1 {
		t.Fatalf("definition=%v ok=%v", d, ok)
	}
	v := d.Views()[0]
	maxColumns, maxColumnsPresent := v.MaxColumns()
	if v.Kind() != ElementSectionsView || v.Status() != StatusSupported || v.Path() != "main" || v.Title() != "Main" || !maxColumnsPresent || maxColumns != 3 {
		t.Fatalf("view=%+v", v)
	}
	flag, present := v.Visibility()[0].Flag()
	if !present || flag {
		t.Fatal("explicit visible: false was lost")
	}
	s := v.Sections()[0]
	columnSpan, hasColumnSpan := s.ColumnSpan()
	rowSpan, hasRowSpan := s.RowSpan()
	if s.Kind() != ElementGridSection || !hasColumnSpan || columnSpan != 1 || !hasRowSpan || rowSpan != 2 || s.Status() != StatusSupported {
		t.Fatalf("section=%+v", s)
	}
	if s.Visibility()[0].Kind() != VisibilityState || s.Visibility()[0].Status() != StatusSupported {
		t.Fatal("state visibility missing")
	}
	heading, stack := s.Cards()[0], s.Cards()[1]
	if heading.Heading() != "Lights" || heading.HeadingStyle() != HeadingStyleSubtitle {
		t.Fatalf("heading=%+v", heading)
	}
	if stack.Kind() != ElementHorizontalStackCard || len(stack.Children()) != 1 {
		t.Fatalf("stack=%+v", stack)
	}
	tile := stack.Children()[0]
	if tile.Kind() != ElementTileCard || tile.Status() != StatusSupported {
		t.Fatalf("tile=%+v", tile)
	}
	if value, present := tile.Vertical(); !present || value {
		t.Fatal("explicit vertical: false was lost")
	}
	if value, present := tile.HideState(); !present || value {
		t.Fatal("explicit hide_state: false was lost")
	}
	columns, present := tile.GridOptions().Columns()
	if !present || columns.Kind() != DimensionFull {
		t.Fatal("full columns missing")
	}
	rows, present := tile.GridOptions().Rows()
	if !present || rows.Kind() != DimensionAuto {
		t.Fatal("auto rows missing")
	}
	actions := tile.Actions()
	if len(actions) != 3 || actions[0].Kind() != ActionToggle || actions[1].Kind() != ActionMoreInfo || actions[2].Kind() != ActionNavigate {
		t.Fatalf("actions=%v", actions)
	}
	if path, present := actions[2].Navigation(); !present || path != "/lovelace/main" {
		t.Fatal("navigation path changed")
	}
	req := d.Requirements()
	if req.Version() != 1 || !contains(req.Elements(), ElementSectionsView) || !contains(req.Elements(), ElementTileCard) || !contains(req.Actions(), ActionToggle) || !contains(req.Visibility(), VisibilityState) {
		t.Fatalf("requirements=%+v", req)
	}
	client, err := NewClientSupport(1, req.Elements(), req.Actions(), req.Visibility())
	if err != nil || !req.SupportedBy(client) {
		t.Fatalf("client support: %v", err)
	}
}

func TestNHA059InventoryProvenanceAndUnsupportedNodes(t *testing.T) {
	root := "testdata/nha059"
	alarmBytes, err := os.ReadFile(filepath.Join(root, "alarm.inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	alarmSum := sha256.Sum256(alarmBytes)
	if len(alarmBytes) != 4309 || hex.EncodeToString(alarmSum[:]) != "2c8198a5b91b6ef1646f0a54b3f09414a86969cf11db36a5d700bc0df5fc5fe6" {
		t.Fatal("alarm inventory fixture changed")
	}
	alarmSource := nha059Source(t, "dashboard_alarm", "dashboard-alarm", "alarm.inventory.yaml")
	alarm, err := Import(root, alarmSource)
	if err != nil || alarm.Status() != StatusUnsupported {
		t.Fatalf("alarm status=%v err=%v diagnostics=%v", alarm.Status(), err, alarm.Diagnostics())
	}
	definition, ok := alarm.Definition()
	if !ok || len(definition.Views()) != 1 || definition.Views()[0].Path() != "" || definition.Views()[0].Status() != StatusUnsupported {
		t.Fatalf("alarm view=%+v", definition.Views())
	}
	if !bytes.Equal(alarmBytes, mustEntryBytes(t, alarm)) {
		t.Fatal("alarm source bytes changed")
	}
	var kiosk, custom, missingPath bool
	for _, diagnostic := range alarm.Diagnostics() {
		loc := diagnostic.Location()
		if loc.Position().File != "alarm.inventory.yaml" || loc.Position().Line < 1 || loc.Position().Column < 1 {
			t.Fatalf("unsourced diagnostic: %+v", loc)
		}
		if loc.Path() == "$.kiosk_mode" {
			kiosk = true
		}
		if diagnostic.Code() == CodeUnsupportedVariant && strings.Contains(loc.Path(), ".cards[") {
			custom = true
		}
		if diagnostic.Code() == CodeMissingField && loc.Path() == "$.views[0]" {
			missingPath = true
		}
	}
	if !kiosk || !custom || !missingPath || len(alarm.Unsupported()) == 0 {
		t.Fatalf("kiosk=%v custom=%v missingPath=%v unsupported=%d", kiosk, custom, missingPath, len(alarm.Unsupported()))
	}
	restoredAlarm, err := Restore(alarmSource, alarm.Sources())
	if err != nil || restoredAlarm.Status() != alarm.Status() || !reflect.DeepEqual(restoredAlarm.Diagnostics(), alarm.Diagnostics()) {
		t.Fatalf("alarm restore drift: err=%v diagnostics=%v", err, restoredAlarm.Diagnostics())
	}
	restoredAlarmDef, ok := restoredAlarm.Definition()
	if !ok || restoredAlarmDef.SHA256() != definition.SHA256() {
		t.Fatal("alarm bundle hash changed after restore")
	}
	bathroomBytes, err := os.ReadFile(filepath.Join(root, "overview-bathroom.inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bathroomSum := sha256.Sum256(bathroomBytes)
	if hex.EncodeToString(bathroomSum[:]) != "d37b082a85999451494f291a4c30d3c2fffe4e74f11ccf9f617eb5def9ddab00" {
		t.Fatal("derived Overview fixture changed")
	}
	bathroom, err := Import(root, nha059Source(t, "lovelace", "lovelace", "overview-bathroom.inventory.yaml"))
	if err != nil || bathroom.Status() != StatusUnsupported {
		t.Fatalf("bathroom status=%v err=%v", bathroom.Status(), err)
	}
	bathroomDef, ok := bathroom.Definition()
	if !ok || len(bathroomDef.Views()) != 1 || bathroomDef.Views()[0].Path() != "bathroom" {
		t.Fatalf("stable path not projected: %+v", bathroomDef.Views())
	}
	bathroomView := bathroomDef.Views()[0]
	if bathroomView.Kind() != ElementSectionsView || bathroomView.Title() != "Bathroom" || len(bathroomView.Sections()) != 2 || bathroomView.Sections()[0].Cards()[0].Heading() != "Bathroom" {
		t.Fatalf("known Overview fields were not projected: %+v", bathroomView)
	}
	if bathroomView.Status() != StatusUnsupported || len(bathroomDef.Requirements().Elements()) != 0 {
		t.Fatal("unknown Overview view modifiers left descendants renderable")
	}
	if !bytes.Equal(bathroomBytes, mustEntryBytes(t, bathroom)) {
		t.Fatal("derived Overview bytes changed")
	}
}

func mustEntryBytes(t *testing.T, result Result) []byte {
	t.Helper()
	data, ok := result.EntryBytes()
	if !ok {
		t.Fatal("entry bytes unavailable")
	}
	return data
}

func TestNHA059IncludesSnapshotsAndDefensiveCopies(t *testing.T) {
	root := t.TempDir()
	main := []byte("# quote and order\nviews: !include views.yaml\n")
	views := []byte("- title: 'Main'\n  path: main\n  type: sections\n  sections:\n  - type: grid\n    cards:\n    - type: tile\n      entity: light.kitchen\n")
	if err := os.WriteFile(filepath.Join(root, "dashboard.yaml"), main, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "views.yaml"), views, 0600); err != nil {
		t.Fatal(err)
	}
	source := nha059Source(t, "main", "main", "dashboard.yaml")
	first, err := Import(root, source)
	if err != nil || first.Status() != StatusSupported {
		t.Fatalf("first status=%v err=%v diagnostics=%v", first.Status(), err, first.Diagnostics())
	}
	files := first.Sources()
	if len(files) != 2 || files[0].Path() != "dashboard.yaml" || files[1].Path() != "views.yaml" {
		t.Fatalf("source order=%v", files)
	}
	if !bytes.Equal(files[0].Bytes(), main) || !bytes.Equal(files[1].Bytes(), views) {
		t.Fatal("snapshots changed")
	}
	second, err := Restore(source, files)
	if err != nil || second.Status() != StatusSupported {
		t.Fatalf("restore status=%v err=%v diagnostics=%v", second.Status(), err, second.Diagnostics())
	}
	firstDef, _ := first.Definition()
	secondDef, _ := second.Definition()
	if firstDef.SHA256() != secondDef.SHA256() || firstDef.Profile() != secondDef.Profile() || len(second.Diagnostics()) != len(first.Diagnostics()) {
		t.Fatal("detached restore drifted")
	}
	files[0] = parser.SourceFile{}
	entry := mustEntryBytes(t, first)
	entry[0] = 'X'
	copyBytes := first.Sources()[1].Bytes()
	copyBytes[0] = 'X'
	if !bytes.Equal(mustEntryBytes(t, first), main) || !bytes.Equal(first.Sources()[1].Bytes(), views) {
		t.Fatal("caller mutation reached retained source")
	}
	mutableDef, _ := first.Definition()
	mutableDef.views[0].sections[0].cards[0].kind = ElementInvalid
	mutableDef.requirements.elements[0] = ElementInvalid
	stableDef, _ := first.Definition()
	if stableDef.Views()[0].Sections()[0].Cards()[0].Kind() != ElementTileCard || stableDef.Requirements().Elements()[0] == ElementInvalid {
		t.Fatal("caller mutation reached retained IR")
	}
	if stableDef.Views()[0].Sections()[0].Cards()[0].Location().References()[0].Kind != parser.ReferenceInclude {
		t.Fatal("include reference chain was lost")
	}
	extra, err := parser.NewSourceFile("unused.yaml", []byte("views: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	unused, err := Restore(source, append(first.Sources(), extra))
	if err != nil || unused.Status() != StatusFatal || unused.Diagnostics()[0].SyntaxCode() != parser.CodeUnsupportedForm {
		t.Fatalf("unused source status=%v err=%v diagnostics=%v", unused.Status(), err, unused.Diagnostics())
	}
}

func TestNHA059UnknownAncestorAndInertAction(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	data := fmt.Sprintf(`views:
- type: sections
  title: Main
  path: main
  visibility: []
  sections:
  - type: grid
    cards:
    - type: tile
      entity: light.kitchen
      tap_action:
        action: perform-action
        perform_action: light.turn_on
        target: {entity_id: light.kitchen}
      script_url: %s/custom.js
    - type: custom:javascript-card
      source: "fetch('%s/run')"
`, server.URL, server.URL)
	r := nha059ImportBytes(t, []byte(data))
	if r.Status() != StatusUnsupported || requests.Load() != 0 {
		t.Fatalf("status=%v requests=%d", r.Status(), requests.Load())
	}
	def, ok := r.Definition()
	if !ok {
		t.Fatal("partial definition missing")
	}
	view := def.Views()[0]
	if view.Status() != StatusUnsupported || view.Sections()[0].Status() != StatusUnsupported {
		t.Fatal("unknown view modifier did not block descendants")
	}
	tile := view.Sections()[0].Cards()[0]
	if tile.Status() != StatusUnsupported || tile.Actions()[0].Status() != StatusUnsupported {
		t.Fatal("unknown ancestor left action usable")
	}
	if len(def.Requirements().Actions()) != 0 || len(def.Requirements().Elements()) != 0 {
		t.Fatal("blocked branch advertised capabilities")
	}
	if requests.Load() != 0 {
		t.Fatal("Lovelace import fetched a script")
	}
}

func TestNHA059PartialSiblingsStrategyAndUnboundUser(t *testing.T) {
	data := `views:
- type: sections
  title: Main
  path: main
  sections:
  - type: grid
    cards:
    - type: tile
      entity: light.kitchen
      tap_action: {action: perform-action, perform_action: light.turn_on, target: {entity_id: light.kitchen}}
    - type: tile
      entity: light.hall
      unknown_property: keep-this
    - type: custom:untrusted-card
      script: "fetch('https://example.invalid/script.js')"
`
	r := nha059ImportBytes(t, []byte(data))
	if r.Status() != StatusUnsupported {
		t.Fatalf("partial status=%v", r.Status())
	}
	d, ok := r.Definition()
	if !ok {
		t.Fatal("partial definition missing")
	}
	cards := d.Views()[0].Sections()[0].Cards()
	if len(cards) != 3 || cards[0].Status() != StatusSupported || cards[1].Status() != StatusUnsupported || cards[2].Status() != StatusUnsupported {
		t.Fatalf("card statuses=%v", cards)
	}
	if cards[0].Actions()[0].Kind() != ActionPerform || cards[0].Actions()[0].Status() != StatusSupported {
		t.Fatal("inert service description lost")
	}
	serviceID, present := cards[0].Actions()[0].Service()
	if !present {
		t.Fatal("service description unavailable")
	}
	if service, err := serviceID.Value(); err != nil || service != "light.turn_on" {
		t.Fatalf("service=%q err=%v", service, err)
	}
	if !contains(d.Requirements().Actions(), ActionPerform) {
		t.Fatal("supported sibling capability lost")
	}
	var unknownProperty, custom bool
	for _, item := range r.Unsupported() {
		loc := item.Location()
		if loc.Position().Line < 1 || loc.Position().Column < 1 || loc.Position().File != "dashboard.yaml" {
			t.Fatalf("missing source: %+v", loc)
		}
		if strings.HasSuffix(loc.Path(), ".unknown_property") {
			unknownProperty = true
		}
		if strings.HasSuffix(loc.Path(), ".cards[2]") {
			custom = true
		}
	}
	if !unknownProperty || !custom {
		t.Fatalf("unknownProperty=%v custom=%v", unknownProperty, custom)
	}
	strategy := nha059ImportBytes(t, []byte("strategy:\n  type: map\n"))
	strategyDef, hasDef := strategy.Definition()
	if strategy.Status() != StatusUnsupported || !hasDef || len(strategyDef.Views()) != 0 || len(strategy.Unsupported()) != 1 {
		t.Fatalf("strategy=%+v", strategy)
	}
	user := nha059ImportBytes(t, []byte(`views:
- type: sections
  title: Main
  path: main
  sections:
  - type: grid
    visibility:
    - condition: user
      users: [ha-user-id]
    cards:
    - type: tile
      entity: light.kitchen
`))
	userDef, _ := user.Definition()
	if user.Status() != StatusUnsupported || userDef.Views()[0].Sections()[0].Status() != StatusUnsupported || userDef.Views()[0].Sections()[0].Cards()[0].Status() != StatusUnsupported {
		t.Fatal("HA user condition was treated as Homeward authorization")
	}
}

func TestNHA059FatalSyntaxAndBoundaries(t *testing.T) {
	tests := []struct {
		name, main, include string
		code                parser.Code
	}{
		{"malformed", "views: [\n", "", parser.CodeYAML},
		{"duplicate", "views: []\nviews: []\n", "", parser.CodeDuplicateKey},
		{"escape", "views: !include ../escape.yaml\n", "", parser.CodeIncludeOutside},
		{"cycle", "views: !include child.yaml\n", "views: !include dashboard.yaml\n", parser.CodeIncludeCycle},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "dashboard.yaml"), []byte(test.main), 0600); err != nil {
				t.Fatal(err)
			}
			if test.include != "" {
				if err := os.WriteFile(filepath.Join(root, "child.yaml"), []byte(test.include), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r, err := Import(root, nha059Source(t, "main", "main", "dashboard.yaml"))
			if err != nil || r.Status() != StatusFatal {
				t.Fatalf("status=%v err=%v", r.Status(), err)
			}
			if _, ok := r.Definition(); ok {
				t.Fatal("fatal syntax produced a definition")
			}
			if len(r.Diagnostics()) != 1 || r.Diagnostics()[0].Code() != CodeSyntax || r.Diagnostics()[0].SyntaxCode() != test.code {
				t.Fatalf("diagnostics=%v", r.Diagnostics())
			}
		})
	}
	large := append([]byte("views: []\n#"), bytes.Repeat([]byte{'x'}, 1<<20)...)
	if r := nha059ImportBytes(t, large); r.Status() != StatusFatal || r.Diagnostics()[0].SyntaxCode() != parser.CodeSizeLimit {
		t.Fatalf("large input=%v", r.Diagnostics())
	}
	if _, err := Import(t.TempDir(), Source{}); err == nil {
		t.Fatal("zero source admitted")
	}
	if _, err := NewClientSupport(0, nil, nil, nil); err == nil {
		t.Fatal("zero client profile admitted")
	}
	if _, err := NewClientSupport(1, []ElementKind{ElementInvalid}, nil, nil); err == nil {
		t.Fatal("unknown element admitted")
	}
	if _, err := NewClientSupport(1, nil, []ActionKind{ActionKind(255)}, nil); err == nil {
		t.Fatal("unknown action admitted")
	}
	if _, err := NewClientSupport(1, nil, nil, []VisibilityKind{VisibilityInvalid}); err == nil {
		t.Fatal("unknown visibility admitted")
	}
	badAlias := strings.Replace(nha059Supported, "light.kitchen", "not_an_alias", 1)
	if r := nha059ImportBytes(t, []byte(badAlias)); r.Status() != StatusUnsupported {
		t.Fatalf("invalid alias status=%v", r.Status())
	}
	aliasCycle := nha059ImportBytes(t, []byte("views: &cycle [*cycle]\n"))
	if aliasCycle.Status() != StatusFatal || len(aliasCycle.Diagnostics()) == 0 {
		t.Fatalf("alias cycle=%v", aliasCycle.Diagnostics())
	}
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		name := "dashboard.yaml"
		if i > 0 {
			name = fmt.Sprintf("level%d.yaml", i)
		}
		body := "[]\n"
		if i < 9 {
			body = fmt.Sprintf("!include level%d.yaml\n", i+1)
		}
		if i == 0 {
			body = "views: " + strings.TrimSuffix(body, "\n") + "\n"
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tooDeep, err := Import(root, nha059Source(t, "main", "main", "dashboard.yaml"))
	if err != nil || tooDeep.Status() != StatusFatal || tooDeep.Diagnostics()[0].SyntaxCode() != parser.CodeIncludeDepth {
		t.Fatalf("include depth status=%v err=%v diagnostics=%v", tooDeep.Status(), err, tooDeep.Diagnostics())
	}
}

func TestNHA059IndependentGesturesAndLocalNavigation(t *testing.T) {
	r := nha059ImportBytes(t, []byte(`views:
- type: sections
  title: Main
  path: main
  sections:
  - type: grid
    cards:
    - type: tile
      entity: light.kitchen
      tap_action: {action: none}
      double_tap_action:
        action: navigate
        navigation_path: '#main'
        confirmation: {text: Proceed}
    - type: tile
      entity: light.hall
      tap_action: {action: none}
      hold_action: {action: toggle}
    - type: tile
      entity: light.outside
      tap_action: {action: navigate, navigation_path: 'https://example.invalid/run'}
`))
	if r.Status() != StatusUnsupported {
		t.Fatalf("status=%v", r.Status())
	}
	d, ok := r.Definition()
	if !ok {
		t.Fatal("partial definition missing")
	}
	cards := d.Views()[0].Sections()[0].Cards()
	if len(cards) != 3 || cards[0].Status() != StatusSupported || cards[1].Status() != StatusSupported || cards[2].Status() != StatusUnsupported {
		t.Fatalf("card statuses=%v", cards)
	}
	first := cards[0].Actions()
	if len(first) != 3 || first[0].Gesture() != GestureTap || first[0].Kind() != ActionNone || first[1].Gesture() != GestureHold || first[1].Kind() != ActionAbsent || first[2].Gesture() != GestureDoubleTap || first[2].Kind() != ActionNavigate {
		t.Fatalf("first gestures=%v", first)
	}
	if path, present := first[2].Navigation(); !present || path != "#main" {
		t.Fatal("local navigation missing")
	}
	if confirmation, present := first[2].Confirmation(); !present || confirmation != "Proceed" {
		t.Fatal("inert confirmation text missing")
	}
	if _, present := first[0].Confirmation(); present {
		t.Fatal("confirmation leaked across gestures")
	}
	second := cards[1].Actions()
	if second[0].Kind() != ActionNone || second[1].Kind() != ActionToggle || second[2].Kind() != ActionAbsent {
		t.Fatalf("second gestures=%v", second)
	}
	third := cards[2].Actions()
	if third[0].Status() != StatusUnsupported || third[1].Status() != StatusUnsupported || third[2].Status() != StatusUnsupported {
		t.Fatalf("unsafe navigation left gestures usable: %v", third)
	}
	if len(r.Unsupported()) == 0 {
		t.Fatal("external navigation not diagnosed")
	}
}

func TestNHA059OptionalEmptyTextKeepsPresence(t *testing.T) {
	data := []byte(`views:
- type: sections
  title: Main
  path: main
  sections:
  - type: grid
    cards:
    - type: tile
      entity: light.first
    - type: tile
      entity: light.second
      name: ''
    - type: tile
      entity: light.third
      name: Kitchen
    - type: horizontal-stack
      cards: [{type: tile, entity: light.fourth}]
    - type: horizontal-stack
      title: ''
      cards: [{type: tile, entity: light.fifth}]
    - type: horizontal-stack
      title: Music
      cards: [{type: tile, entity: light.sixth}]
`)
	source := nha059Source(t, "dashboard_test", "test-dashboard", "dashboard.yaml")
	result := nha059ImportBytes(t, data)
	if result.Status() != StatusSupported {
		t.Fatalf("status=%v diagnostics=%v", result.Status(), result.Diagnostics())
	}
	check := func(t *testing.T, result Result) {
		t.Helper()
		definition, ok := result.Definition()
		if !ok {
			t.Fatal("definition missing")
		}
		cards := definition.Views()[0].Sections()[0].Cards()
		if len(cards) != 6 {
			t.Fatalf("cards=%d", len(cards))
		}
		for i, want := range []struct {
			text    string
			present bool
		}{{"", false}, {"", true}, {"Kitchen", true}} {
			got, present := cards[i].Name()
			if got != want.text || present != want.present {
				t.Fatalf("name[%d]=%q present=%v", i, got, present)
			}
		}
		for i, want := range []struct {
			text    string
			present bool
		}{{"", false}, {"", true}, {"Music", true}} {
			got, present := cards[i+3].Title()
			if got != want.text || present != want.present {
				t.Fatalf("title[%d]=%q present=%v", i, got, present)
			}
		}
		definition.views[0].sections[0].cards[1].hasName = false
		definition.views[0].sections[0].cards[4].hasTitle = false
	}
	check(t, result)
	check(t, result) // Mutating a returned definition cannot change the retained presence bits.
	restored, err := Restore(source, result.Sources())
	if err != nil || restored.Status() != StatusSupported {
		t.Fatalf("restore status=%v err=%v", restored.Status(), err)
	}
	check(t, restored)
	if !bytes.Equal(mustEntryBytes(t, restored), data) {
		t.Fatal("empty text source changed on restore")
	}
}

func TestNHA059CompilerBudgetsAreFatal(t *testing.T) {
	base := "views:\n- type: sections\n  title: Main\n  path: main\n  sections:\n  - type: grid\n    cards:\n"
	var manyCards strings.Builder
	manyCards.WriteString(base)
	for i := 0; i <= maxCards; i++ {
		manyCards.WriteString("    - type: tile\n      entity: light.kitchen\n")
	}
	var deepCards strings.Builder
	deepCards.WriteString(base)
	for i := 0; i <= maxCardDepth; i++ {
		deepCards.WriteString(strings.Repeat("  ", i))
		deepCards.WriteString("    - type: horizontal-stack\n")
		deepCards.WriteString(strings.Repeat("  ", i))
		deepCards.WriteString("      cards:\n")
	}
	deepCards.WriteString(strings.Repeat("  ", maxCardDepth+1))
	deepCards.WriteString("    - type: tile\n")
	deepCards.WriteString(strings.Repeat("  ", maxCardDepth+1))
	deepCards.WriteString("      entity: light.kitchen\n")
	var manyDiagnostics strings.Builder
	manyDiagnostics.WriteString("views: []\n")
	for i := 0; i <= maxDiagnostics; i++ {
		fmt.Fprintf(&manyDiagnostics, "unknown_%d: value\n", i)
	}
	for _, test := range []struct{ name, data string }{
		{"cards", manyCards.String()},
		{"depth", deepCards.String()},
		{"diagnostics", manyDiagnostics.String()},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := nha059ImportBytes(t, []byte(test.data))
			if r.Status() != StatusFatal {
				t.Fatalf("status=%v diagnostics=%v", r.Status(), r.Diagnostics())
			}
			if _, ok := r.Definition(); ok {
				t.Fatal("budget overflow yielded a definition")
			}
			if len(r.Diagnostics()) == 0 || r.Diagnostics()[len(r.Diagnostics())-1].Code() != CodeInvalidValue {
				t.Fatalf("budget diagnostic missing: %v", r.Diagnostics())
			}
			if len(r.Diagnostics()) > maxDiagnostics || len(r.Unsupported()) > maxDiagnostics {
				t.Fatal("diagnostic retention exceeded budget")
			}
		})
	}
}
