package dashboard

// Private immutable compiler IR. Source spellings are retained only here;
// canonical resource names and typed literals are resolved at projection.
type widgetContent interface{ dashboardWidgetContent() }
type widgetIR struct {
	Content                 widgetContent
	ActionLabel, EmptyLabel string
}
type sourceField struct{ EntityId, FieldKey string }
type sourceClockWeather struct{ Timezone, WeatherEntityId string }
type sourceRoom struct {
	RoomId                          string
	Temperature                     *sourceField
	ActivityEntityIds, ActiveStates []string
}
type sourceThermostat struct {
	EntityId                              string
	CurrentTemperature, TargetTemperature *sourceField
	Presentation                          thermostatPresentation
}
type sourceAgenda struct {
	CalendarId, Timezone string
	WindowSeconds, Limit uint32
}
type sourceTasks struct {
	ListId       string
	Limit        uint32
	Presentation tasksPresentation
}
type sourceSecurity struct{ EntityIds []string }
type sourceObservation struct {
	Field        *sourceField
	Label        string
	ActiveStates []string
	Icon         string
}
type sourceObservations struct{ Items []*sourceObservation }
type sourceNavigation struct{}

func (*sourceClockWeather) dashboardWidgetContent() {}
func (*sourceRoom) dashboardWidgetContent()         {}
func (*sourceThermostat) dashboardWidgetContent()   {}
func (*sourceAgenda) dashboardWidgetContent()       {}
func (*sourceTasks) dashboardWidgetContent()        {}
func (*sourceSecurity) dashboardWidgetContent()     {}
func (*sourceObservations) dashboardWidgetContent() {}
func (*sourceNavigation) dashboardWidgetContent()   {}

type thermostatPresentation uint8

const (
	thermostatCompact thermostatPresentation = iota
	thermostatDetail
	thermostatSummary
)

type tasksPresentation uint8

const (
	tasksPreview tasksPresentation = iota
	tasksShopping
)

type controlPresentation uint8

const (
	controlsVisible controlPresentation = iota
	controlsSummary
)

func cloneField(field *sourceField) *sourceField {
	if field == nil {
		return nil
	}
	value := *field
	return &value
}
func cloneWidget(value *widgetIR) *widgetIR {
	if value == nil {
		return nil
	}
	out := *value
	switch source := value.Content.(type) {
	case *sourceClockWeather:
		copy := *source
		out.Content = &copy
	case *sourceRoom:
		copy := *source
		copy.Temperature = cloneField(source.Temperature)
		copy.ActivityEntityIds = append([]string(nil), source.ActivityEntityIds...)
		copy.ActiveStates = append([]string(nil), source.ActiveStates...)
		out.Content = &copy
	case *sourceThermostat:
		copy := *source
		copy.CurrentTemperature = cloneField(source.CurrentTemperature)
		copy.TargetTemperature = cloneField(source.TargetTemperature)
		out.Content = &copy
	case *sourceAgenda:
		copy := *source
		out.Content = &copy
	case *sourceTasks:
		copy := *source
		out.Content = &copy
	case *sourceSecurity:
		copy := *source
		copy.EntityIds = append([]string(nil), source.EntityIds...)
		out.Content = &copy
	case *sourceObservations:
		copy := &sourceObservations{}
		for _, item := range source.Items {
			cloned := *item
			cloned.Field = cloneField(item.Field)
			cloned.ActiveStates = append([]string(nil), item.ActiveStates...)
			copy.Items = append(copy.Items, &cloned)
		}
		out.Content = copy
	case *sourceNavigation:
		out.Content = &sourceNavigation{}
	}
	return &out
}
