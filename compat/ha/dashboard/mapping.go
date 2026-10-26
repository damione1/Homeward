package dashboard

// This is a source inventory and routing plan, not a compatibility declaration.
// A family routed to a native recipe still needs compilation, negotiation and
// runtime qualification. In particular, installed HACS resources are not cards.
import (
	"fmt"

	"github.com/damione1/Homeward/compat/ha/parser"
)

type OccurrenceKind uint8

const (
	OccurrenceInvalid OccurrenceKind = iota
	OccurrenceView
	OccurrenceSection
	OccurrenceCard
	OccurrenceBadge
	OccurrenceChip
	OccurrenceFeature
	OccurrenceStrategy
)

type Family uint8

const (
	FamilyUnknown Family = iota
	FamilyLayout
	FamilyCommon
	FamilyClimate
	FamilyLight
	FamilyFan
	FamilyHelper
	FamilyCover
	FamilyAlarm
	FamilyMushroomTemplate
	FamilyMushroomSelect
	FamilyMushroomChips
	FamilyBubble
	FamilyStreamline
	FamilyScheduler
	FamilyText
	FamilyLists
	FamilyHistory
	FamilyCalendar
	FamilyMedia
	FamilyCamera
	FamilyWeather
	FamilyRuntimeStrategy
)

type MappingTreatment uint8

const (
	TreatmentInvalid MappingTreatment = iota
	TreatmentNativeLayout
	TreatmentNativeRecipe
	TreatmentSpecializedTicket
	TreatmentRefused
)

type MappingTicket uint8

const (
	MappingLayout   MappingTicket = 7
	MappingControls MappingTicket = 8
	MappingRecipes  MappingTicket = 9
	MappingWeather  MappingTicket = 10
	MappingLists    MappingTicket = 11
	MappingHistory  MappingTicket = 13
	MappingCalendar MappingTicket = 15
	MappingMedia    MappingTicket = 16
	MappingCamera   MappingTicket = 17
	MappingLogic    MappingTicket = 20
)

func (t MappingTicket) String() string {
	switch t {
	case MappingLayout, MappingControls, MappingRecipes, MappingWeather, MappingLists, MappingHistory,
		MappingCalendar, MappingMedia, MappingCamera, MappingLogic:
		return fmt.Sprintf("NHF-%03d", t)
	}
	return ""
}

type MappingDecision struct {
	Family    Family
	Treatment MappingTreatment
	Ticket    MappingTicket
	Reason    string
}

type Occurrence struct {
	Kind     OccurrenceKind
	Variant  string // exact open source label, including future/unknown cards
	Location Location
	Decision MappingDecision
	Source   parser.Node
}

// MapOccurrences inspects only structural Lovelace positions. A `type` property
// inside weather settings, service data or a downloaded resource is not a card.
// Original immutable subtrees and positions are retained, including refusals.
func MapOccurrences(doc *parser.Document) ([]Occurrence, error) {
	if doc == nil || doc.Profile() != parser.LovelaceProfile {
		return nil, ErrInvalidInput
	}
	var result []Occurrence
	var walk func(parser.Node, OccurrenceKind, int) error
	walk = func(node parser.Node, context OccurrenceKind, depth int) error {
		if depth > 32 {
			return fmt.Errorf("dashboard occurrence depth exceeds budget: %w", ErrInvalidInput)
		}
		if node.Kind() == parser.KindMapping && context != OccurrenceInvalid {
			typeNode, present := node.Lookup("type")
			variant, valid := scalarText(typeNode)
			// HA infers sections for a view containing sections. Keep the exact
			// subtree: this inventory does not silently rewrite the source file.
			if context == OccurrenceView && !present {
				if _, sections := node.Lookup("sections"); sections {
					variant, valid = "sections", true
				}
			}
			if !valid {
				variant = ""
			}
			decision := mapVariant(variant, context)
			result = append(result, Occurrence{Kind: context, Variant: variant, Location: locate(node), Decision: decision, Source: node})
			if len(result) > 4096 {
				return fmt.Errorf("dashboard occurrence inventory exceeds budget: %w", ErrInvalidInput)
			}
		}
		for _, entry := range node.Entries() {
			childContext := OccurrenceInvalid
			switch entry.Key {
			case "views":
				if context == OccurrenceInvalid {
					childContext = OccurrenceView
				}
			case "sections":
				if context == OccurrenceView {
					childContext = OccurrenceSection
				}
			case "cards", "card":
				if context == OccurrenceView || context == OccurrenceSection || context == OccurrenceCard {
					childContext = OccurrenceCard
				}
			case "badges":
				if context == OccurrenceView {
					childContext = OccurrenceBadge
				}
			case "chips":
				if context == OccurrenceCard || context == OccurrenceBadge {
					childContext = OccurrenceChip
				}
			case "features":
				if context == OccurrenceCard {
					childContext = OccurrenceFeature
				}
			case "strategy":
				if context == OccurrenceInvalid {
					childContext = OccurrenceStrategy
				}
			case "header", "footer":
				if context == OccurrenceView {
					if card, present := entry.Value.Lookup("card"); present {
						if err := walk(card, OccurrenceCard, depth+1); err != nil {
							return err
						}
					}
				}
			}
			if childContext == OccurrenceInvalid {
				continue
			}
			if err := walk(entry.Value, childContext, depth+1); err != nil {
				return err
			}
		}
		for _, item := range node.Items() {
			if err := walk(item, context, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc.Root(), OccurrenceInvalid, 0); err != nil {
		return nil, err
	}
	return result, nil
}

func mapVariant(variant string, context OccurrenceKind) MappingDecision {
	if context == OccurrenceStrategy {
		return MappingDecision{FamilyRuntimeStrategy, TreatmentRefused, MappingRecipes, "Runtime-generated strategies have no captured view definition; no default support is inferred."}
	}
	switch variant {
	case "sections", "grid", "horizontal-stack", "heading":
		return MappingDecision{FamilyLayout, TreatmentNativeLayout, MappingLayout, "Bounded native forest; individual properties and ancestor support still require admission."}
	case "vertical-stack":
		return MappingDecision{FamilyLayout, TreatmentNativeRecipe, MappingRecipes, "Native vertical grouping, including explicitly delimited Bubble popup bodies."}
	case "tile", "button", "entity", "entities", "toggle":
		return MappingDecision{FamilyCommon, TreatmentNativeRecipe, MappingRecipes, "Native entity/action recipe; instance capabilities and current authorization remain mandatory."}
	case "target-temperature", "climate-hvac-modes":
		return MappingDecision{FamilyClimate, TreatmentNativeRecipe, MappingControls, "Climate controls bind only to the advertised instance bounds and options."}
	case "light-brightness", "light-color-temp":
		return MappingDecision{FamilyLight, TreatmentNativeRecipe, MappingControls, "Native light controls with exact instance constraints and supported coordinates."}
	case "fan-speed":
		return MappingDecision{FamilyFan, TreatmentNativeRecipe, MappingControls, "Native fan control; power and speed remain independent observations."}
	case "numeric-input":
		return MappingDecision{FamilyHelper, TreatmentNativeRecipe, MappingControls, "Native typed helper editor with exact SQL precondition; no fabricated revision."}
	case "cover-open-close", "cover-position":
		return MappingDecision{FamilyCover, TreatmentNativeRecipe, MappingRecipes, "Typed cover capability/state/action contract and composed real or isolated mock handler required."}
	case "alarm-modes", "custom:mushroom-alarm-control-panel-card":
		return MappingDecision{FamilyAlarm, TreatmentNativeRecipe, MappingRecipes, "Typed alarm contract and permitted modes required; no inferred authority or automatic command."}
	case "custom:mushroom-template-card", "custom:mushroom-template-badge", "template":
		return MappingDecision{FamilyMushroomTemplate, TreatmentNativeRecipe, MappingRecipes, "Native static/server-projected recipe; out-of-profile templates and styling retain diagnostics, never execute in the client."}
	case "custom:mushroom-select-card":
		return MappingDecision{FamilyMushroomSelect, TreatmentNativeRecipe, MappingRecipes, "Native controlled helper selector; observed value and capability options are authoritative."}
	case "custom:mushroom-chips-card":
		return MappingDecision{FamilyMushroomChips, TreatmentNativeRecipe, MappingRecipes, "Bounded native chip group; each chip and gesture is admitted independently."}
	case "custom:bubble-card":
		return MappingDecision{FamilyBubble, TreatmentNativeRecipe, MappingRecipes, "Native navigation/dialogue with stable server-resolved destinations; CSS/JS hooks refused with source context."}
	case "custom:streamline-card":
		return MappingDecision{FamilyStreamline, TreatmentRefused, MappingRecipes, "No implicit JavaScript or client template expansion; requires an explicit bounded server profile."}
	case "custom:scheduler-card":
		return MappingDecision{FamilyScheduler, TreatmentSpecializedTicket, MappingLogic, "Server-owned schedules and logic; no client scheduler engine."}
	case "markdown":
		return MappingDecision{FamilyText, TreatmentNativeRecipe, MappingRecipes, "Safe bounded native text profile; HTML, JavaScript and unprojected templates are refused."}
	case "todo-list":
		return MappingDecision{FamilyLists, TreatmentSpecializedTicket, MappingLists, "ListService-backed native list with revisions and conflict handling."}
	case "history-graph":
		return MappingDecision{FamilyHistory, TreatmentSpecializedTicket, MappingHistory, "Authorized paged native history with explicit gaps and units."}
	case "custom:calendar-card-pro":
		return MappingDecision{FamilyCalendar, TreatmentSpecializedTicket, MappingCalendar, "Native calendar over its typed server facade; no downloaded custom card."}
	case "custom:mini-media-player", "custom:sonos-card", "custom:mushroom-media-player-card", "media-player-volume-slider":
		return MappingDecision{FamilyMedia, TreatmentSpecializedTicket, MappingMedia, "Typed native media/group control with per-instance advertised commands."}
	case "custom:frigate-card", "picture-entity":
		return MappingDecision{FamilyCamera, TreatmentSpecializedTicket, MappingCamera, "Authorized snapshot/media session; decoding and device/provider qualification remain separate."}
	case "custom:clock-weather-card", "custom:horizon-card", "weather":
		return MappingDecision{FamilyWeather, TreatmentSpecializedTicket, MappingWeather, "Native clock/weather/solar projection; timezone and freshness explicit."}
	}
	return MappingDecision{FamilyUnknown, TreatmentRefused, MappingRecipes, "Unknown structural variant retained with its source; no executable fallback."}
}
