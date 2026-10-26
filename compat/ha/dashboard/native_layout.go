package dashboard

// Native projection is pure data conversion. The caller supplies the authorized
// entity view, negotiated support, and optional snapshot-scoped navigation index.
import (
	"fmt"
	"strings"

	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func nativeDimension(value Dimension, present bool) *dashboardv1.LayoutDimension {
	if !present {
		return nil
	}
	switch value.Kind() {
	case DimensionCount:
		count, _ := value.Count()
		return &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_COUNT, Count: uint32(count)}
	case DimensionFull:
		return &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_FULL}
	case DimensionAuto:
		return &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_AUTO}
	case DimensionInvalid:
		return nil
	}
	return nil
}

func nativeLayoutKind(value ElementKind) (dashboardv1.LayoutKind, string) {
	switch value {
	case ElementSectionsView:
		return dashboardv1.LayoutKind_LAYOUT_KIND_VIEW, "sections"
	case ElementGridSection:
		return dashboardv1.LayoutKind_LAYOUT_KIND_SECTION, "grid"
	case ElementGridCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_GRID, "grid"
	case ElementHorizontalStackCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_HORIZONTAL_STACK, "horizontal-stack"
	case ElementHeadingCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_HEADING, "heading"
	case ElementTileCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_TILE, "tile"
	case ElementRecipeCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_RECIPE, "recipe"
	case ElementVerticalStackCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_VERTICAL_STACK, "vertical-stack"
	case ElementPopupCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_POPUP, "custom:bubble-card"
	case ElementChipGroup:
		return dashboardv1.LayoutKind_LAYOUT_KIND_CHIP_GROUP, "custom:mushroom-chips-card"
	case ElementInvalid:
		return dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED, "unknown"
	}
	return dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED, "unknown"
}

type LayoutProjection struct {
	Support    *dashboardv1.ClientLayoutSupport
	Entity     func(domain.HAAlias) (domain.EntityID, bool)
	Navigation *NavigationIndex
}

func ProjectNativeLayout(result Result, options LayoutProjection) (*dashboardv1.NativeLayout, error) {
	support, resolve := options.Support, options.Entity
	definition, present := result.Definition()
	if !present || result.Status() == StatusFatal || resolve == nil {
		return nil, ErrInvalidInput
	}
	layout := &dashboardv1.NativeLayout{Version: uint32(definition.Requirements().Version())}
	if layout.Version != 1 && layout.Version != 2 {
		return nil, ErrInvalidInput
	}
	navigation := options.Navigation
	if navigation == nil && layout.Version == 2 {
		index, err := NewNavigationIndex([]Definition{definition})
		if err != nil {
			return nil, err
		}
		navigation = &index
	}
	if layout.Version == 2 && !navigation.matches(definition) {
		return nil, ErrInvalidInput
	}
	allowed := map[dashboardv1.LayoutKind]bool{}
	// Old clients did not negotiate a tree. Keep their overview readable; their
	// generated decoder ignores this additive projection. New clients negotiate.
	if support == nil && layout.Version == 1 {
		for _, kind := range []dashboardv1.LayoutKind{dashboardv1.LayoutKind_LAYOUT_KIND_VIEW, dashboardv1.LayoutKind_LAYOUT_KIND_SECTION,
			dashboardv1.LayoutKind_LAYOUT_KIND_GRID, dashboardv1.LayoutKind_LAYOUT_KIND_HORIZONTAL_STACK,
			dashboardv1.LayoutKind_LAYOUT_KIND_HEADING, dashboardv1.LayoutKind_LAYOUT_KIND_TILE} {
			allowed[kind] = true
		}
	} else if support != nil && ((support.Profile == definition.Profile() && support.Version == layout.Version) ||
		// The recipe renderer includes the Basic vocabulary. A v2 client can
		// consume a server's Basic tree without enabling recipes in that tree.
		(definition.Profile() == Profile && layout.Version == 1 && support.Profile == RecipesProfile && support.Version == 2)) {
		for _, kind := range support.Kinds {
			allowed[kind] = true
		}
	}
	unsupported := result.Unsupported()
	diagnostics := result.Diagnostics()
	makeNode := func(id string, kind ElementKind, status Status, location Location,
		visibility []Visibility, blocked bool) *dashboardv1.LayoutNode {
		wireKind, variant := nativeLayoutKind(kind)
		node := &dashboardv1.LayoutNode{Id: id, Kind: wireKind, Source: nativeSource(location, variant)}
		for _, item := range unsupported {
			if item.Location().Path() != location.Path() {
				continue
			}
			if name, present := item.Node().Lookup("type"); present {
				if scalar, present := name.Scalar(); present {
					node.Source.Variant = scalar.Text()
				}
			}
		}
		if blocked || status != StatusSupported || (layout.Version == 1 && len(visibility) != 0) || !allowed[wireKind] {
			node.Kind = dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED
			node.Diagnostic = "Élément non pris en charge par le profil négocié."
			if blocked {
				node.Diagnostic = "Élément inactif : son parent n’est pas pris en charge."
			} else if len(visibility) != 0 {
				node.Diagnostic = "Condition de visibilité non évaluée par ce client."
			}
			for _, diagnostic := range diagnostics {
				if diagnostic.Location().Path() == location.Path() || strings.HasPrefix(diagnostic.Location().Path(), location.Path()+".") {
					node.Diagnostic = diagnostic.Reason()
					break
				}
			}
		}
		layout.Nodes = append(layout.Nodes, node)
		return node
	}
	applyRecipes := func(node *dashboardv1.LayoutNode, owner Location, actions []Action, visibility []Visibility, view Location) {
		if layout.Version != 2 || node.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED {
			return
		}
		resolver := RecipeResolver{Entity: resolve}
		if navigation != nil {
			resolver.Destination = func(path string) (*dashboardv1.NativeDestination, bool) {
				return navigation.Resolve(definition.Source().ID(), view, path)
			}
		}
		recipes, err := ProjectNativeRecipes(owner, actions, visibility, resolver)
		if err != nil {
			node.Kind, node.EntityId, node.Diagnostic = dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED, "", "Recette, cible ou destination non disponible dans la vue autorisée."
			return
		}
		node.Recipes = recipes
	}
	var cards func([]Card, *dashboardv1.LayoutNode, int, Location) error
	cards = func(values []Card, parent *dashboardv1.LayoutNode, depth int, view Location) error {
		if depth > 16 {
			return fmt.Errorf("dashboard card depth exceeds importer profile")
		}
		for _, card := range values {
			node := makeNode(card.Location().Path(), card.Kind(), card.Status(), card.Location(), card.Visibility(), parent.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED)
			if card.Variant() != "" {
				node.Source.Variant = card.Variant()
			}
			applyRecipes(node, card.Location(), card.Actions(), card.Visibility(), view)
			parent.Children = append(parent.Children, node.Id)
			node.Title, _ = card.Name()
			if card.Kind() == ElementHeadingCard {
				node.Title = card.Heading()
				switch card.HeadingStyle() {
				case HeadingStyleDefault:
					node.HeadingStyle = dashboardv1.NativeHeadingStyle_NATIVE_HEADING_STYLE_DEFAULT
				case HeadingStyleTitle:
					node.HeadingStyle = dashboardv1.NativeHeadingStyle_NATIVE_HEADING_STYLE_TITLE
				case HeadingStyleSubtitle:
					node.HeadingStyle = dashboardv1.NativeHeadingStyle_NATIVE_HEADING_STYLE_SUBTITLE
				case HeadingStyleInvalid:
				}
			}
			if title, present := card.Title(); present {
				node.Title = title
			}
			if alias, present := card.Entity(); present && (node.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_TILE || node.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_RECIPE) {
				if id, known := resolve(alias); known {
					node.EntityId, _ = id.Value()
				}
				if node.EntityId == "" {
					node.Kind = dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED
					node.Recipes = nil
					node.Diagnostic = "Entité non disponible dans la vue autorisée."
				}
			}
			if layout.Version == 2 && node.Kind != dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED {
				node.RecipeKind = nativeRecipeKind(card.Recipe())
				node.Secondary = card.Secondary()
				for _, feature := range card.Features() {
					node.Features = append(node.Features, nativeFeatureKind(feature.Kind()))
				}
			}
			if count, present := card.Columns(); present {
				node.Columns = uint32(count)
			}
			node.ColumnSpan = nativeDimension(card.GridOptions().Columns())
			node.RowSpan = nativeDimension(card.GridOptions().Rows())
			if value, present := card.Square(); present {
				node.Square = &value
			}
			if value, present := card.Vertical(); present {
				node.Vertical = &value
			}
			if value, present := card.HideState(); present {
				node.HideState = &value
			}
			if len(card.Children()) != 0 {
				if err := cards(card.Children(), node, depth+1, view); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, view := range definition.Views() {
		node := makeNode(view.Location().Path(), view.Kind(), view.Status(), view.Location(), view.Visibility(), false)
		applyRecipes(node, view.Location(), nil, view.Visibility(), view.Location())
		node.Title = view.Title()
		node.Route = view.NativeRoute()
		if columns, present := view.MaxColumns(); present {
			node.Columns = uint32(columns)
		}
		layout.Views = append(layout.Views, node.Id)
		for _, section := range view.Sections() {
			child := makeNode(section.Location().Path(), section.Kind(), section.Status(), section.Location(), section.Visibility(), node.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED)
			applyRecipes(child, section.Location(), nil, section.Visibility(), view.Location())
			node.Children = append(node.Children, child.Id)
			if count, present := section.ColumnSpan(); present {
				child.ColumnSpan = &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_COUNT, Count: uint32(count)}
			}
			if count, present := section.RowSpan(); present {
				child.RowSpan = &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_COUNT, Count: uint32(count)}
			}
			if err := cards(section.Cards(), child, 0, view.Location()); err != nil {
				return nil, err
			}
		}
	}
	// Runtime strategies have no views in the importer. Retain a sourced inert
	// diagnostic as the only view instead of inventing an executable layout.
	if len(layout.Views) == 0 {
		for _, item := range unsupported {
			if strings.Contains(item.Location().Path(), "strategy") {
				node := makeNode(item.Location().Path(), ElementInvalid, StatusUnsupported, item.Location(), nil, false)
				node.Source.Variant = "strategy"
				if name, present := item.Node().Lookup("type"); present {
					if scalar, present := name.Scalar(); present {
						node.Source.Variant = scalar.Text()
					}
				}
				node.Title = "Stratégie non prise en charge"
				layout.Views = append(layout.Views, node.Id)
			}
		}
	}
	if len(layout.Nodes) > 128+256+2048 || len(layout.Views) == 0 {
		return nil, ErrInvalidInput
	}
	return layout, nil
}

func nativeRecipeKind(kind RecipeKind) dashboardv1.NativeRecipeKind {
	switch kind {
	case RecipeTemplate:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_TEMPLATE
	case RecipeSelect:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_SELECT
	case RecipeAlarm:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_ALARM
	case RecipeBubbleButton:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_BUBBLE_BUTTON
	case RecipeBubblePopup:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_BUBBLE_POPUP
	case RecipeChips:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_CHIPS
	case RecipeTemplateChip:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_TEMPLATE_CHIP
	case RecipeButton:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_BUTTON
	case RecipeEntity:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_ENTITY
	case RecipeText:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_TEXT
	case RecipeInvalid:
		return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_UNSPECIFIED
	}
	return dashboardv1.NativeRecipeKind_NATIVE_RECIPE_KIND_UNSPECIFIED
}

func nativeFeatureKind(kind FeatureKind) dashboardv1.NativeFeatureKind {
	switch kind {
	case FeatureToggle:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_TOGGLE
	case FeatureBrightness:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_BRIGHTNESS
	case FeatureColorTemperature:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_COLOR_TEMPERATURE
	case FeatureTargetTemperature:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_TARGET_TEMPERATURE
	case FeatureClimateModes:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_CLIMATE_MODES
	case FeatureFanSpeed:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_FAN_SPEED
	case FeatureNumericInput:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_NUMERIC_INPUT
	case FeatureCoverOpenClose:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_COVER_OPEN_CLOSE
	case FeatureCoverPosition:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_COVER_POSITION
	case FeatureAlarmModes:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_ALARM_MODES
	case FeatureInvalid:
		return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_UNSPECIFIED
	}
	return dashboardv1.NativeFeatureKind_NATIVE_FEATURE_KIND_UNSPECIFIED
}
