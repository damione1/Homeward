package dashboard

import (
	"errors"
	"math"
	"strings"
	"unicode"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/domain"
)

var ErrUnprojectableRecipe = errors.New("dashboard recipe cannot be projected by the admitted native profile")

type projectedRecipes struct {
	Interactions []*wire.DashboardInteraction
	Visibility   *wire.DashboardPredicate
}

func nativeSource(location Location, variant string) *wire.DashboardLayoutSource {
	at := location.Position()
	if at.Line < 0 || at.Column < 0 || int64(at.Line) > math.MaxInt32 || int64(at.Column) > math.MaxInt32 {
		return nil
	}
	return &wire.DashboardLayoutSource{File: at.File, Line: int32(at.Line), Column: int32(at.Column), Path: location.Path(), Variant: variant}
}

func nativeEntity(alias domain.HAAlias, resolver *apiProjector) (string, error) {
	if resolver.options.Entity == nil {
		return "", ErrUnprojectableRecipe
	}
	name, ok := resolver.options.Entity(alias)
	if !ok || !name.Valid() || name.Home() != resolver.options.Home {
		return "", ErrUnprojectableRecipe
	}
	return name.String(), nil
}

// Project admitted source semantics directly into the closed v2 alternatives.
// Failure makes the complete node inert, never a more permissive default.
func projectRecipes(owner Location, actions []Action, visibility []Visibility, resolver *apiProjector, view Location) (*projectedRecipes, error) {
	if len(actions) > 3 || len(visibility) > 63 {
		return nil, ErrUnprojectableRecipe
	}
	recipes := &projectedRecipes{}
	seen := map[Gesture]bool{}
	for _, action := range actions {
		if action.Status() != StatusSupported {
			return nil, ErrUnprojectableRecipe
		}
		if seen[action.Gesture()] {
			return nil, ErrUnprojectableRecipe
		}
		seen[action.Gesture()] = true
		var gesture wire.DashboardGesture
		switch action.Gesture() {
		case GestureTap:
			gesture = wire.DashboardGesture_DASHBOARD_GESTURE_TAP
		case GestureHold:
			gesture = wire.DashboardGesture_DASHBOARD_GESTURE_HOLD
		case GestureDoubleTap:
			gesture = wire.DashboardGesture_DASHBOARD_GESTURE_DOUBLE_TAP
		case GestureInvalid:
			return nil, ErrUnprojectableRecipe
		}
		if gesture == wire.DashboardGesture_DASHBOARD_GESTURE_UNSPECIFIED {
			return nil, ErrUnprojectableRecipe
		}
		if action.Kind() == ActionAbsent {
			continue
		}
		interaction := &wire.DashboardInteraction{Gesture: gesture, Source: nativeSource(action.Location(), "action")}
		if interaction.Source == nil {
			return nil, ErrUnprojectableRecipe
		}
		if text, present := action.Confirmation(); present {
			interaction.Confirmation = &wire.DashboardConfirmation{Text: text}
		}
		switch action.Kind() {
		case ActionNone:
			if interaction.Confirmation != nil {
				return nil, ErrUnprojectableRecipe
			}
			interaction.Intention = &wire.DashboardInteraction_None{None: &wire.DashboardNoAction{}}
		case ActionNavigate:
			path, present := action.Navigation()
			if !present {
				return nil, ErrUnprojectableRecipe
			}
			target, ok := resolver.destination(view, path)
			if !ok {
				return nil, ErrUnprojectableRecipe
			}
			interaction.Intention = &wire.DashboardInteraction_Navigate{Navigate: target}
		case ActionMoreInfo, ActionToggle, ActionPerform:
			alias, present := action.Entity()
			if !present {
				return nil, ErrUnprojectableRecipe
			}
			entity, err := nativeEntity(alias, resolver)
			if err != nil {
				return nil, err
			}
			if action.Kind() == ActionMoreInfo {
				interaction.Intention = &wire.DashboardInteraction_MoreInfo{MoreInfo: &wire.DashboardEntityTarget{Entity: entity}}
				break
			}
			kind := wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_TOGGLE
			if action.Kind() == ActionPerform {
				service, present := action.Service()
				if !present {
					return nil, ErrUnprojectableRecipe
				}
				value, err := service.Value()
				if err != nil {
					return nil, ErrUnprojectableRecipe
				}
				aliasValue, err := alias.Value()
				if err != nil {
					return nil, ErrUnprojectableRecipe
				}
				serviceDomain, _, _ := strings.Cut(value, ".")
				entityDomain, _, _ := strings.Cut(aliasValue, ".")
				if serviceDomain != entityDomain {
					return nil, ErrUnprojectableRecipe
				}
				kind = nativeService(value)
				if kind == wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_UNSPECIFIED {
					return nil, ErrUnprojectableRecipe
				}
			}
			capability, err := resolver.command(alias, kind)
			if err != nil {
				return nil, err
			}
			interaction.Intention = &wire.DashboardInteraction_Command{Command: &wire.DashboardCommand{Capability: capability, Kind: kind}}
		case ActionInvalid, ActionAbsent:
			return nil, ErrUnprojectableRecipe
		}
		if interaction.Intention == nil {
			return nil, ErrUnprojectableRecipe
		}
		recipes.Interactions = append(recipes.Interactions, interaction)
	}
	if len(visibility) != 0 {
		list := &wire.DashboardPredicateList{}
		remaining := 63 // the enclosing conjunction consumes the first node
		for _, value := range visibility {
			if value.Kind() == VisibilityScreen {
				if _, _, valid := value.Viewport(); !valid {
					return nil, ErrUnprojectableRecipe
				}
				continue // handled exclusively by LayoutNode.screen_visibility
			}
			predicate, err := projectNativePredicate(value, resolver, 1, &remaining)
			if err != nil {
				return nil, err
			}
			list.Conditions = append(list.Conditions, predicate)
		}
		if len(list.Conditions) != 0 {
			recipes.Visibility = &wire.DashboardPredicate{Source: nativeSource(owner, "visibility"), Condition: &wire.DashboardPredicate_All{All: list}}
			if recipes.Visibility.Source == nil {
				return nil, ErrUnprojectableRecipe
			}
		}
	}
	// Successful projection always owns a value, including an empty recipe.
	// Both layout consumers read its fields after checking only the error.
	return recipes, nil
}

func projectNativePredicate(value Visibility, resolver *apiProjector, depth int, remaining *int) (*wire.DashboardPredicate, error) {
	if value.Status() != StatusSupported || depth > 8 || *remaining <= 0 {
		return nil, ErrUnprojectableRecipe
	}
	*remaining--
	predicate := &wire.DashboardPredicate{Source: nativeSource(value.Location(), "visibility")}
	if predicate.Source == nil {
		return nil, ErrUnprojectableRecipe
	}
	switch value.Kind() {
	case VisibilityFlag:
		flag, present := value.Flag()
		if !present {
			return nil, ErrUnprojectableRecipe
		}
		predicate.Condition = &wire.DashboardPredicate_Constant{Constant: flag}
	case VisibilityState:
		alias, present := value.Entity()
		if !present {
			return nil, ErrUnprojectableRecipe
		}

		state, not, present := value.State()
		if !present || strings.ContainsRune(state, '\x00') {
			return nil, ErrUnprojectableRecipe
		}
		comparison := wire.DashboardComparison_DASHBOARD_COMPARISON_EQUAL
		if not {
			comparison = wire.DashboardComparison_DASHBOARD_COMPARISON_NOT_EQUAL
		}
		field, literal, err := resolver.literalField(alias, "state", state)
		if err != nil {
			return nil, err
		}
		predicate.Condition = &wire.DashboardPredicate_State{State: &wire.DashboardStatePredicate{Field: field, Expected: literal, Comparison: comparison}}
	case VisibilityAll, VisibilityAny:
		children := value.Children()
		if len(children) == 0 || len(children) > *remaining {
			return nil, ErrUnprojectableRecipe
		}
		list := &wire.DashboardPredicateList{}
		for _, child := range children {
			nested, err := projectNativePredicate(child, resolver, depth+1, remaining)
			if err != nil {
				return nil, err
			}
			list.Conditions = append(list.Conditions, nested)
		}
		if value.Kind() == VisibilityAll {
			predicate.Condition = &wire.DashboardPredicate_All{All: list}
		} else {
			predicate.Condition = &wire.DashboardPredicate_Any{Any: list}
		}
	case VisibilityInvalid, VisibilityHAUser:
		return nil, ErrUnprojectableRecipe
	}
	if predicate.Condition == nil {
		return nil, ErrUnprojectableRecipe
	}
	return predicate, nil
}

func nativeIdentity(value string) bool {
	return value != "" && len(value) <= 4096 && strings.IndexFunc(value, unicode.IsControl) < 0
}

// Parse the open HA service name once into the closed native command vocabulary.
// Unlisted services/data never become an arbitrary physical invocation.
func nativeService(value string) wire.DashboardCommandKind {
	switch value {
	case "switch.toggle", "light.toggle", "fan.toggle", "input_boolean.toggle":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_TOGGLE
	case "switch.turn_on", "light.turn_on", "fan.turn_on", "input_boolean.turn_on":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_TURN_ON
	case "switch.turn_off", "light.turn_off", "fan.turn_off", "input_boolean.turn_off":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_TURN_OFF
	case "button.press", "input_button.press", "scene.turn_on", "script.turn_on":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ACTIVATE
	case "cover.open_cover":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_COVER_OPEN
	case "cover.close_cover":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_COVER_CLOSE
	case "cover.stop_cover":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_COVER_STOP
	case "lock.lock":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_LOCK
	case "lock.unlock":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_UNLOCK
	case "alarm_control_panel.alarm_arm_home":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ALARM_ARM_HOME
	case "alarm_control_panel.alarm_arm_away":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ALARM_ARM_AWAY
	case "alarm_control_panel.alarm_disarm":
		return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ALARM_DISARM
	}
	return wire.DashboardCommandKind_DASHBOARD_COMMAND_KIND_UNSPECIFIED
}
