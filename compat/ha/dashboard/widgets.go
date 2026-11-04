package dashboard

// Native widgets are a finite, data-only addition to the existing Lovelace
// forest. Source bindings remain aliases until snapshot-scoped projection.
import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
)

func RestoreConfigured(source Source, files []parser.SourceFile) (Result, error) {
	doc, err := parser.LoadSources(source.entry, files, parser.SyntaxProfileLovelace)
	if err != nil {
		return syntaxFailure(err), nil
	}
	profile := CompilationBasic
	if _, present := doc.Root().Lookup("homeward"); present {
		profile = CompilationDashboard
	}
	return CompileWithProfile(source, doc, profile)
}
func compileDashboardMetadata(node parser.Node, definition *Definition) bool {
	if node.Kind() != parser.KindMapping {
		return false
	}
	owner, profile := "", ""
	hasURL := false
	for _, entry := range node.Entries() {
		text, ok := staticText(entry.Value, 256)
		if !ok {
			return false
		}
		switch entry.Key {
		case "owner":
			owner = text
		case "profile":
			profile = text
		case "url_path":
			hasURL = true
			if !validSegment(text, 128) {
				return false
			}
			definition.source.urlPath = text
		default:
			return false
		}
	}
	id, err := contracts.ParseAccountID(owner)
	if err != nil || profile != DashboardProfile || !hasURL {
		return false
	}
	definition.owner = id
	return true
}
func boundedUint(node parser.Node, min, max uint32) (uint32, bool) {
	scalar, ok := node.Scalar()
	if !ok || scalar.Kind() != parser.ScalarInteger {
		return 0, false
	}
	n, err := strconv.ParseUint(scalar.Text(), 10, 32)
	return uint32(n), err == nil && n >= uint64(min) && n <= uint64(max)
}
func widgetStaticText(node parser.Node, limit int) (string, bool) {
	text, ok := staticText(node, limit)
	return text, ok && strings.IndexFunc(text, unicode.IsControl) < 0
}
func widgetText(node parser.Node, key string, required bool) (string, bool) {
	value, present := node.Lookup(key)
	if !present {
		return "", !required
	}
	text, ok := widgetStaticText(value, 256)
	return text, ok && (!required || text != "")
}
func widgetAlias(node parser.Node, key string, required bool) (string, bool) {
	text, ok := widgetText(node, key, required)
	if !ok || text == "" {
		return text, ok
	}
	_, err := domain.ParseHAAlias(text)
	return text, err == nil
}
func widgetStrings(node parser.Node, key string, required bool, max int, alias bool) ([]string, bool) {
	value, present := node.Lookup(key)
	if !present {
		return nil, !required
	}
	if value.Kind() != parser.KindSequence || len(value.Items()) > max || (required && len(value.Items()) == 0) {
		return nil, false
	}
	result := []string{}
	seen := map[string]bool{}
	for _, item := range value.Items() {
		text, ok := widgetStaticText(item, 256)
		if !ok || text == "" || seen[text] {
			return nil, false
		}
		if alias {
			if _, err := domain.ParseHAAlias(text); err != nil {
				return nil, false
			}
		}
		seen[text] = true
		result = append(result, text)
	}
	return result, true
}
func widgetField(node parser.Node, key string, required bool) (*sourceField, bool) {
	value, present := node.Lookup(key)
	if !present {
		return nil, !required
	}
	if value.Kind() != parser.KindMapping {
		return nil, false
	}
	for _, entry := range value.Entries() {
		if entry.Key != "entity" && entry.Key != "field" {
			return nil, false
		}
	}
	entity, a := widgetAlias(value, "entity", true)
	field, b := widgetText(value, "field", true)
	allowed := field == "state" || field == "value" || field == "current_temperature_celsius" || field == "setpoint_celsius" || field == "measurement"
	return &sourceField{EntityId: entity, FieldKey: field}, a && b && allowed
}
func timezone(node parser.Node) (string, bool) {
	text, ok := widgetText(node, "timezone", true)
	if !ok || text == "Local" {
		return "", false
	}
	_, err := time.LoadLocation(text)
	return text, err == nil
}
func widgetNumber(node parser.Node, key string, min, max uint32) (uint32, bool) {
	value, present := node.Lookup(key)
	if !present {
		return 0, false
	}
	return boundedUint(value, min, max)
}
func (c *compiler) widgetCard(node parser.Node, variant string) Card {
	card := Card{kind: ElementWidgetCard, variant: variant, status: StatusSupported, location: locate(node), widget: &widgetIR{}}
	allowed := map[string]bool{"type": true, "title": true, "grid_options": true, "visibility": true, "action": true, "action_label": true, "empty_label": true, "tap_action": true, "icon": true}
	allow := func(keys ...string) {
		for _, key := range keys {
			allowed[key] = true
		}
	}
	valid := true
	w := card.widget
	switch strings.TrimPrefix(variant, "custom:homeward-") {
	case "clock-weather":
		allow("timezone", "weather_entity")
		zone, a := timezone(node)
		entity, b := widgetAlias(node, "weather_entity", false)
		valid = a && b
		w.Content = &sourceClockWeather{Timezone: zone, WeatherEntityId: entity}
	case "room":
		allow("room_id", "temperature", "activity_entities", "active_states")
		id, a := widgetText(node, "room_id", true)
		field, b := widgetField(node, "temperature", false)
		entities, d := widgetStrings(node, "activity_entities", false, 4096, true)
		states, e := widgetStrings(node, "active_states", false, 64, false)
		valid = a && validSegment(id, 128) && b && d && e && (len(entities) == 0 || len(states) > 0) && (field == nil || field.FieldKey == "measurement" || field.FieldKey == "current_temperature_celsius")
		w.Content = &sourceRoom{RoomId: id, Temperature: field, ActivityEntityIds: entities, ActiveStates: states}
	case "thermostat":
		allow("entity", "current_temperature", "target_temperature", "presentation")
		entity, a := widgetAlias(node, "entity", true)
		current, b := widgetField(node, "current_temperature", true)
		target, d := widgetField(node, "target_temperature", true)
		valid = a && b && d && target != nil && current != nil && target.EntityId == entity && current.EntityId == entity && target.FieldKey == "setpoint_celsius" && current.FieldKey == "current_temperature_celsius"
		presentation, ok := widgetText(node, "presentation", false)
		valid = valid && ok && (presentation == "" || presentation == "compact" || presentation == "detail" || presentation == "summary")
		style := thermostatCompact
		if presentation == "detail" {
			style = thermostatDetail
		}
		if presentation == "summary" {
			style = thermostatSummary
		}
		w.Content = &sourceThermostat{EntityId: entity, CurrentTemperature: current, TargetTemperature: target, Presentation: style}
	case "agenda":
		allow("calendar_id", "timezone", "window_seconds", "limit")
		id, a := widgetText(node, "calendar_id", true)
		zone, b := timezone(node)
		window, d := widgetNumber(node, "window_seconds", 1, 366*86400)
		limit, e := widgetNumber(node, "limit", 1, 1000)
		_, idErr := domain.ParseEntityID(id)
		valid = a && b && d && e && idErr == nil
		w.Content = &sourceAgenda{CalendarId: id, Timezone: zone, WindowSeconds: window, Limit: limit}
	case "tasks":
		allow("list_id", "limit", "presentation")
		id, a := widgetText(node, "list_id", true)
		_, err := domain.ParseListID(id)
		limit, b := widgetNumber(node, "limit", 1, 1000)
		presentation, ok := widgetText(node, "presentation", false)
		valid = a && b && err == nil && ok && (presentation == "" || presentation == "preview" || presentation == "shopping")
		style := tasksPreview
		if presentation == "shopping" {
			style = tasksShopping
		}
		w.Content = &sourceTasks{ListId: id, Limit: limit, Presentation: style}
	case "security":
		allow("entities")
		entities, a := widgetStrings(node, "entities", true, 64, true)
		valid = a
		w.Content = &sourceSecurity{EntityIds: entities}
	case "observations":
		allow("items")
		items, present := node.Lookup("items")
		valid = present && items.Kind() == parser.KindSequence && len(items.Items()) <= 64
		observations := &sourceObservations{}
		if valid {
			for _, item := range items.Items() {
				if item.Kind() != parser.KindMapping {
					valid = false
					break
				}
				for _, entry := range item.Entries() {
					if entry.Key != "field" && entry.Key != "label" && entry.Key != "active_states" && entry.Key != "icon" {
						valid = false
					}
				}
				field, a := widgetField(item, "field", true)
				label, b := widgetText(item, "label", true)
				states, d := widgetStrings(item, "active_states", false, 64, false)
				valid = valid && a && b && d
				icon, iconOK := widgetText(item, "icon", false)
				valid = valid && iconOK
				observations.Items = append(observations.Items, &sourceObservation{Field: field, Label: label, ActiveStates: states, Icon: icon})
			}
		}
		w.Content = observations
	case "navigation":
		allow("destination")
		path, a := widgetText(node, "destination", true)
		valid = a && validNavigation(path)
		card.widgetNavigation = path
		w.Content = &sourceNavigation{}
	default:
		valid = false
	}
	for _, entry := range node.Entries() {
		if !allowed[entry.Key] {
			c.unknown(entry, "widget property outside native profile")
			valid = false
			continue
		}
		switch entry.Key {
		case "icon":
			var ok bool
			card.icon, ok = widgetStaticText(entry.Value, 256)
			valid = valid && ok
		case "title":
			card.title, card.hasTitle = staticText(entry.Value, 256)
			valid = valid && card.hasTitle
		case "grid_options":
			var invalid bool
			card.grid, invalid = c.gridOptions(entry.Value)
			valid = valid && !invalid
		case "visibility":
			var invalid bool
			card.visibility, invalid = c.visibility(entry.Value)
			valid = valid && !invalid
		case "action":
			text, ok := widgetStaticText(entry.Value, 256)
			valid = valid && ok && validNavigation(text)
			card.widgetAction = text
		case "action_label":
			var ok bool
			w.ActionLabel, ok = widgetStaticText(entry.Value, 256)
			valid = valid && ok
		case "empty_label":
			var ok bool
			w.EmptyLabel, ok = widgetStaticText(entry.Value, 256)
			valid = valid && ok
		case "tap_action":
			card.actions = append(card.actions, c.action(GestureTap, entry.Value, true, node, domain.HAAlias{}, false))
		}
	}
	// A navigation leaf has no other content: derive its button label only
	// from the configured title, never from a client-side literal.
	if _, navigation := w.Content.(*sourceNavigation); navigation {
		if w.ActionLabel == "" {
			w.ActionLabel = card.title
		}
		valid = valid && w.ActionLabel != "" && strings.IndexFunc(w.ActionLabel, unicode.IsControl) < 0
		if card.widgetAction != "" && card.widgetAction != card.widgetNavigation {
			valid = false
		}
	}
	expectedAction := card.widgetAction
	if card.widgetNavigation != "" {
		expectedAction = card.widgetNavigation
	}
	if expectedAction != "" {
		for _, action := range card.actions {
			path, navigate := action.Navigation()
			_, confirmed := action.Confirmation()
			if !navigate || path != expectedAction || confirmed {
				valid = false
			}
		}
	}
	if valid {
		c.widgetBindings += len(card.WidgetEntities())
		switch card.widget.Content.(type) {
		case *sourceAgenda, *sourceTasks:
			c.resourceWidgets++
			c.widgetBindings++
		}
		if c.widgetBindings > 4096 || c.resourceWidgets > 32 {
			valid = false
			c.overflow = true
		}
	}
	if !valid {
		c.unsupported(CodeInvalidValue, node, "invalid or incomplete native widget configuration")
		card.status = StatusUnsupported
		card.widget = nil
	}
	return card
}
func (c Card) WidgetEntities() []domain.HAAlias {
	if c.widget == nil {
		return nil
	}
	var values []string
	field := func(f *sourceField) {
		if f != nil {
			values = append(values, f.EntityId)
		}
	}
	switch value := c.widget.Content.(type) {
	case *sourceClockWeather:
		if value.WeatherEntityId != "" {
			values = append(values, value.WeatherEntityId)
		}
	case *sourceRoom:
		field(value.Temperature)
		values = append(values, value.ActivityEntityIds...)
	case *sourceThermostat:
		values = append(values, value.EntityId)
		field(value.CurrentTemperature)
		field(value.TargetTemperature)
	case *sourceSecurity:
		values = append(values, value.EntityIds...)
	case *sourceObservations:
		for _, item := range value.Items {
			field(item.Field)
		}
	}
	result := make([]domain.HAAlias, 0, len(values))
	for _, text := range values {
		alias, err := domain.ParseHAAlias(text)
		if err == nil {
			result = append(result, alias)
		}
	}
	return result
}
