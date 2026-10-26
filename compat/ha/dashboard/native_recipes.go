package dashboard

import (
	"errors"
	"strings"
	"unicode"

	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/domain"
)

var ErrUnprojectableRecipe = errors.New("dashboard recipe cannot be projected by the admitted native profile")

// RecipeResolver binds only identities available in the caller's authorized
// dashboard/entity view. It never resolves aliases by name fragments and never
// performs I/O, authorization, template evaluation or a device command itself.
type RecipeResolver struct {
	Entity      func(domain.HAAlias) (domain.EntityID, bool)
	Destination func(string) (*wire.NativeDestination, bool)
}

func nativeSource(location Location, variant string) *wire.LayoutSource {
	at := location.Position()
	return &wire.LayoutSource{File: at.File, Line: uint32(max(0, at.Line)), Column: uint32(max(0, at.Column)), Path: location.Path(), Variant: variant}
}

func nativeEntity(alias domain.HAAlias, resolver RecipeResolver) (string, error) {
	if resolver.Entity == nil {
		return "", ErrUnprojectableRecipe
	}
	id, ok := resolver.Entity(alias)
	if !ok {
		return "", ErrUnprojectableRecipe
	}
	value, err := id.Value()
	if err != nil || !nativeIdentity(value) {
		return "", ErrUnprojectableRecipe
	}
	return value, nil
}

// ProjectNativeRecipes projects the already admitted importer IR to inert versioned
// wire data. Failure is all-or-nothing: a caller must mark that node inert, never
// omit an unsupported action/condition and expose a more permissive default.
// This function does not claim that a client negotiated or can render recipes.
func ProjectNativeRecipes(owner Location, actions []Action, visibility []Visibility, resolver RecipeResolver) (*wire.NativeRecipes, error) {
	if len(actions) > 3 || len(visibility) > 63 {
		return nil, ErrUnprojectableRecipe
	}
	recipes := &wire.NativeRecipes{Version: 1}
	seen := map[Gesture]bool{}
	for _, action := range actions {
		if action.Status() != StatusSupported {
			return nil, ErrUnprojectableRecipe
		}
		if seen[action.Gesture()] {
			return nil, ErrUnprojectableRecipe
		}
		seen[action.Gesture()] = true
		var gesture wire.NativeGesture
		switch action.Gesture() {
		case GestureTap:
			gesture = wire.NativeGesture_NATIVE_GESTURE_TAP
		case GestureHold:
			gesture = wire.NativeGesture_NATIVE_GESTURE_HOLD
		case GestureDoubleTap:
			gesture = wire.NativeGesture_NATIVE_GESTURE_DOUBLE_TAP
		case GestureInvalid:
			return nil, ErrUnprojectableRecipe
		}
		if gesture == wire.NativeGesture_NATIVE_GESTURE_UNSPECIFIED {
			return nil, ErrUnprojectableRecipe
		}
		if action.Kind() == ActionAbsent {
			continue
		}
		interaction := &wire.NativeInteraction{Gesture: gesture, Source: nativeSource(action.Location(), "action")}
		if text, present := action.Confirmation(); present {
			interaction.Confirmation = &wire.NativeConfirmation{Text: text}
		}
		switch action.Kind() {
		case ActionNone:
			if interaction.Confirmation != nil {
				return nil, ErrUnprojectableRecipe
			}
			interaction.Intention = &wire.NativeInteraction_None{None: &wire.NativeNoAction{}}
		case ActionNavigate:
			path, present := action.Navigation()
			if !present || resolver.Destination == nil {
				return nil, ErrUnprojectableRecipe
			}
			target, ok := resolver.Destination(path)
			if !ok || target == nil || target.DashboardId == nil || target.DashboardId.Value == "" || target.NodeId == "" ||
				(target.Presentation != wire.NativePresentation_NATIVE_PRESENTATION_PAGE && target.Presentation != wire.NativePresentation_NATIVE_PRESENTATION_POPUP) {
				return nil, ErrUnprojectableRecipe
			}
			if len(target.ProtoReflect().GetUnknown()) != 0 || len(target.DashboardId.ProtoReflect().GetUnknown()) != 0 ||
				!nativeIdentity(target.NodeId) || !nativeIdentity(target.DashboardId.Value) {
				return nil, ErrUnprojectableRecipe
			}
			interaction.Intention = &wire.NativeInteraction_Navigate{Navigate: &wire.NativeDestination{
				DashboardId: &wire.DashboardId{Value: target.DashboardId.Value}, NodeId: target.NodeId, Presentation: target.Presentation}}
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
				interaction.Intention = &wire.NativeInteraction_MoreInfo{MoreInfo: &wire.NativeEntityTarget{EntityId: entity}}
				break
			}
			kind := wire.NativeCommandKind_NATIVE_COMMAND_KIND_TOGGLE
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
				if kind == wire.NativeCommandKind_NATIVE_COMMAND_KIND_UNSPECIFIED {
					return nil, ErrUnprojectableRecipe
				}
			}
			interaction.Intention = &wire.NativeInteraction_Command{Command: &wire.NativeCommand{EntityId: entity, Kind: kind}}
		case ActionInvalid, ActionAbsent:
			return nil, ErrUnprojectableRecipe
		}
		if interaction.Intention == nil {
			return nil, ErrUnprojectableRecipe
		}
		recipes.Interactions = append(recipes.Interactions, interaction)
	}
	if len(visibility) != 0 {
		list := &wire.NativePredicateList{}
		remaining := 63 // the enclosing conjunction consumes the first node
		for _, value := range visibility {
			predicate, err := projectNativePredicate(value, resolver, 1, &remaining)
			if err != nil {
				return nil, err
			}
			list.Conditions = append(list.Conditions, predicate)
		}
		recipes.Visibility = &wire.NativePredicate{Source: nativeSource(owner, "visibility"), Condition: &wire.NativePredicate_All{All: list}}
	}
	if len(recipes.Interactions) == 0 && recipes.Visibility == nil {
		return nil, nil
	}
	return recipes, nil
}

func projectNativePredicate(value Visibility, resolver RecipeResolver, depth int, remaining *int) (*wire.NativePredicate, error) {
	if value.Status() != StatusSupported || depth > 8 || *remaining <= 0 {
		return nil, ErrUnprojectableRecipe
	}
	*remaining--
	predicate := &wire.NativePredicate{Source: nativeSource(value.Location(), "visibility")}
	switch value.Kind() {
	case VisibilityFlag:
		flag, present := value.Flag()
		if !present {
			return nil, ErrUnprojectableRecipe
		}
		predicate.Condition = &wire.NativePredicate_Constant{Constant: flag}
	case VisibilityState:
		alias, present := value.Entity()
		if !present {
			return nil, ErrUnprojectableRecipe
		}
		entity, err := nativeEntity(alias, resolver)
		if err != nil {
			return nil, err
		}
		state, not, present := value.State()
		if !present || strings.ContainsRune(state, '\x00') {
			return nil, ErrUnprojectableRecipe
		}
		comparison := wire.NativeComparison_NATIVE_COMPARISON_EQUAL
		if not {
			comparison = wire.NativeComparison_NATIVE_COMPARISON_NOT_EQUAL
		}
		predicate.Condition = &wire.NativePredicate_State{State: &wire.NativeStatePredicate{EntityId: entity, State: state, Comparison: comparison}}
	case VisibilityAll, VisibilityAny:
		children := value.Children()
		if len(children) == 0 || len(children) > *remaining {
			return nil, ErrUnprojectableRecipe
		}
		list := &wire.NativePredicateList{}
		for _, child := range children {
			nested, err := projectNativePredicate(child, resolver, depth+1, remaining)
			if err != nil {
				return nil, err
			}
			list.Conditions = append(list.Conditions, nested)
		}
		if value.Kind() == VisibilityAll {
			predicate.Condition = &wire.NativePredicate_All{All: list}
		} else {
			predicate.Condition = &wire.NativePredicate_Any{Any: list}
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
// Unlisted services/data never become an arbitrary ActionService invocation.
func nativeService(value string) wire.NativeCommandKind {
	switch value {
	case "switch.toggle", "light.toggle", "fan.toggle", "input_boolean.toggle":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_TOGGLE
	case "switch.turn_on", "light.turn_on", "fan.turn_on", "input_boolean.turn_on":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_TURN_ON
	case "switch.turn_off", "light.turn_off", "fan.turn_off", "input_boolean.turn_off":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_TURN_OFF
	case "button.press", "input_button.press", "scene.turn_on", "script.turn_on":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_ACTIVATE
	case "cover.open_cover":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_COVER_OPEN
	case "cover.close_cover":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_COVER_CLOSE
	case "cover.stop_cover":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_COVER_STOP
	case "lock.lock":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_LOCK
	case "lock.unlock":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_UNLOCK
	case "alarm_control_panel.alarm_arm_home":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_ALARM_ARM_HOME
	case "alarm_control_panel.alarm_arm_away":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_ALARM_ARM_AWAY
	case "alarm_control_panel.alarm_disarm":
		return wire.NativeCommandKind_NATIVE_COMMAND_KIND_ALARM_DISARM
	}
	return wire.NativeCommandKind_NATIVE_COMMAND_KIND_UNSPECIFIED
}
