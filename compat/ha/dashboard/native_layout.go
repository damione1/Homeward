package dashboard

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/types/known/emptypb"
)

func nativeDimension(value Dimension, present bool) *api.DashboardLayoutDimension {
	if !present {
		return nil
	}
	switch value.Kind() {
	case DimensionCount:
		count, _ := value.Count()
		if count < 1 || count > 12 {
			return nil
		}
		return &api.DashboardLayoutDimension{Size: &api.DashboardLayoutDimension_Count{Count: int32(count)}}
	case DimensionFull:
		return &api.DashboardLayoutDimension{Size: &api.DashboardLayoutDimension_Full{Full: &emptypb.Empty{}}}
	case DimensionAuto:
		return &api.DashboardLayoutDimension{Size: &api.DashboardLayoutDimension_Auto{Auto: &emptypb.Empty{}}}
	}
	return nil
}
func inertNode(node *api.DashboardLayoutNode) {
	node.Visibility = nil
	node.Content = &api.DashboardLayoutNode_Unsupported{Unsupported: &api.DashboardUnsupportedNode{Diagnostic: "Élément ou liaison non disponible dans cette vue autorisée."}}
}

// ProjectDashboardDefinition converts the admitted compiler IR directly to v2.
// Unsupported source remains inert; no protocol negotiation or v1 adapter exists.
func ProjectDashboardDefinition(def Definition, options APIProjection) (*api.DashboardLayout, error) {
	if !options.Home.Valid() || options.Entity == nil || options.Capabilities == nil {
		return nil, ErrInvalidInput
	}
	if options.Navigation == nil {
		index, err := NewNavigationIndex([]Definition{def})
		if err != nil {
			return nil, err
		}
		options.Navigation = &index
	}
	if !options.Navigation.matches(def) {
		return nil, ErrInvalidInput
	}
	p := &apiProjector{options: options, definition: def}
	layout := &api.DashboardLayout{}
	add := func(location Location, status Status, visibility []Visibility, blocked bool, view Location) *api.DashboardLayoutNode {
		node := &api.DashboardLayoutNode{Id: location.Path(), Source: nativeSource(location, "")}
		layout.Nodes = append(layout.Nodes, node)
		screen, ok := nativeScreenVisibility(visibility)
		node.ScreenVisibility = screen
		if blocked || status != StatusSupported || !ok {
			inertNode(node)
			return node
		}
		recipes, err := projectRecipes(location, nil, visibility, p, view)
		if err != nil {
			inertNode(node)
		} else {
			node.Visibility = recipes.Visibility
		}
		return node
	}
	var cards func([]Card, bool, int, Location) ([]string, error)
	cards = func(values []Card, blocked bool, depth int, view Location) ([]string, error) {
		if depth > 16 {
			return nil, ErrInvalidInput
		}
		ids := []string{}
		for _, card := range values {
			if card.gap > 256 || card.minimumColumnWidth > 4096 || card.columns > 12 {
				return nil, ErrInvalidInput
			}
			for _, span := range card.grid.responsiveSpans {
				if span.Columns < 1 || span.Columns > 12 || span.Span < 1 || span.Span > span.Columns {
					return nil, ErrInvalidInput
				}
			}
			node := add(card.Location(), card.Status(), card.Visibility(), blocked, view)
			ids = append(ids, node.Id)
			if node.Source == nil {
				return nil, ErrInvalidInput
			}
			node.Source.Variant = card.Variant()
			node.DisplayName, _ = card.Name()
			if title, ok := card.Title(); ok {
				node.DisplayName = title
			}
			node.Icon = nativeCardIcon(card.icon)
			for _, dimension := range []struct {
				value   Dimension
				present bool
			}{{card.grid.columns, card.grid.hasColumns}, {card.grid.rows, card.grid.hasRows}} {
				if count, counted := dimension.value.Count(); dimension.present && counted && (count < 1 || count > 12) {
					return nil, ErrInvalidInput
				}
			}
			node.Placement = &api.DashboardPlacement{ColumnSpan: nativeDimension(card.GridOptions().Columns()), RowSpan: nativeDimension(card.GridOptions().Rows())}
			for _, span := range card.grid.responsiveSpans {
				node.Placement.ResponsiveSpans = append(node.Placement.ResponsiveSpans, &api.DashboardResponsiveSpan{Columns: int32(span.Columns), Span: int32(span.Span)})
			}
			recipes, err := projectRecipes(card.Location(), card.Actions(), card.Visibility(), p, view)
			if err != nil {
				inertNode(node)
			}
			if node.GetUnsupported() == nil {
				interactions := &api.DashboardInteractions{Interactions: recipes.Interactions}
				node.Visibility = recipes.Visibility
				children := &api.DashboardChildren{Gap: int32(card.gap)}
				presentation := api.DashboardControlPresentation_DASHBOARD_CONTROL_PRESENTATION_CONTROLS
				if card.controlPresentation == controlsSummary {
					presentation = api.DashboardControlPresentation_DASHBOARD_CONTROL_PRESENTATION_SUMMARY
				}
				switch card.Kind() {
				case ElementGridCard:
					grid := &api.DashboardGridNode{Children: children, Columns: int32(card.columns), MinimumColumnWidth: int32(card.minimumColumnWidth), Interactions: interactions}
					if value, ok := card.Square(); ok {
						grid.Square = &value
					}
					node.Content = &api.DashboardLayoutNode_Grid{Grid: grid}
				case ElementHorizontalStackCard:
					node.Content = &api.DashboardLayoutNode_HorizontalStack{HorizontalStack: &api.DashboardStackNode{Children: children, Interactions: interactions}}
				case ElementVerticalStackCard:
					node.Content = &api.DashboardLayoutNode_VerticalStack{VerticalStack: &api.DashboardStackNode{Children: children, Interactions: interactions}}
				case ElementHeadingCard:
					node.DisplayName = card.Heading()
					style := api.DashboardHeadingStyle_DASHBOARD_HEADING_STYLE_DEFAULT
					if card.HeadingStyle() == HeadingStyleTitle {
						style = api.DashboardHeadingStyle_DASHBOARD_HEADING_STYLE_TITLE
					} else if card.HeadingStyle() == HeadingStyleSubtitle {
						style = api.DashboardHeadingStyle_DASHBOARD_HEADING_STYLE_SUBTITLE
					}
					node.Content = &api.DashboardLayoutNode_Heading{Heading: &api.DashboardHeadingNode{Style: style, Interactions: interactions}}
				case ElementPopupCard:
					node.Content = &api.DashboardLayoutNode_Popup{Popup: &api.DashboardPopupNode{Children: children, Interactions: interactions}}
				case ElementChipGroup:
					node.Content = &api.DashboardLayoutNode_ChipGroup{ChipGroup: &api.DashboardChipGroupNode{Children: children, Interactions: interactions}}
				case ElementTileCard, ElementRecipeCard:
					capability, features, err := p.cardBinding(card)
					if err != nil {
						inertNode(node)
						break
					}
					if card.Kind() == ElementTileCard {
						tile := &api.DashboardTileNode{Capability: capability, Features: features, Presentation: presentation, Interactions: interactions}
						if value, ok := card.Vertical(); ok {
							tile.Vertical = &value
						}
						if value, ok := card.HideState(); ok {
							tile.HideObservation = &value
						}
						node.Content = &api.DashboardLayoutNode_Tile{Tile: tile}
					} else {
						node.Content = &api.DashboardLayoutNode_Recipe{Recipe: &api.DashboardRecipeNode{Kind: nativeRecipeKind(card.Recipe()), Capability: capability, Features: features, Secondary: card.Secondary(), Presentation: presentation, Interactions: interactions}}
					}
				case ElementWidgetCard:
					widget, err := p.widget(card, view)
					if err != nil {
						inertNode(node)
					} else {
						widget.Interactions = interactions
						node.Content = &api.DashboardLayoutNode_Widget{Widget: widget}
					}
				default:
					inertNode(node)
				}
			}
			descendants, err := cards(card.Children(), node.GetUnsupported() != nil, depth+1, view)
			if err != nil {
				return nil, err
			}
			switch content := node.Content.(type) {
			case *api.DashboardLayoutNode_Grid:
				content.Grid.Children.Children = descendants
			case *api.DashboardLayoutNode_HorizontalStack:
				content.HorizontalStack.Children.Children = descendants
			case *api.DashboardLayoutNode_VerticalStack:
				content.VerticalStack.Children.Children = descendants
			case *api.DashboardLayoutNode_Popup:
				content.Popup.Children.Children = descendants
			case *api.DashboardLayoutNode_ChipGroup:
				content.ChipGroup.Children.Children = descendants
			case *api.DashboardLayoutNode_Unsupported:
				content.Unsupported.Children = descendants
			default:
				if len(descendants) > 0 {
					return nil, ErrInvalidInput
				}
			}
			if len(layout.Nodes) > 2432 {
				return nil, ErrInvalidInput
			}
		}
		return ids, nil
	}
	for _, view := range def.Views() {
		columns, _ := view.MaxColumns()
		if view.gap > 256 || view.minimumColumnWidth > 4096 || columns > 12 {
			return nil, ErrInvalidInput
		}
		node := add(view.Location(), view.Status(), view.Visibility(), false, view.Location())
		node.DisplayName = view.Title()
		layout.Views = append(layout.Views, node.Id)
		children := &api.DashboardChildren{Gap: int32(view.gap)}
		if node.GetUnsupported() == nil {
			columns, _ := view.MaxColumns()
			node.Content = &api.DashboardLayoutNode_View{View: &api.DashboardViewNode{Route: view.NativeRoute(), Children: children, MaximumColumns: int32(columns), MinimumColumnWidth: int32(view.minimumColumnWidth)}}
		}
		for _, section := range view.Sections() {
			if section.gap > 256 || section.minimumColumnWidth > 4096 || section.columnSpan > 12 || section.rowSpan > 12 {
				return nil, ErrInvalidInput
			}
			child := add(section.Location(), section.Status(), section.Visibility(), node.GetUnsupported() != nil, view.Location())
			children.Children = append(children.Children, child.Id)
			child.Placement = &api.DashboardPlacement{}
			if count, ok := section.ColumnSpan(); ok {
				child.Placement.ColumnSpan = &api.DashboardLayoutDimension{Size: &api.DashboardLayoutDimension_Count{Count: int32(count)}}
			}
			if count, ok := section.RowSpan(); ok {
				child.Placement.RowSpan = &api.DashboardLayoutDimension{Size: &api.DashboardLayoutDimension_Count{Count: int32(count)}}
			}
			ids, err := cards(section.Cards(), child.GetUnsupported() != nil, 0, view.Location())
			if err != nil {
				return nil, err
			}
			if child.GetUnsupported() != nil {
				child.GetUnsupported().Children = ids
			} else {
				child.Content = &api.DashboardLayoutNode_Section{Section: &api.DashboardSectionNode{Children: &api.DashboardChildren{Children: ids, Gap: int32(section.gap)}, MinimumColumnWidth: int32(section.minimumColumnWidth)}}
			}
		}
		if node.GetUnsupported() != nil {
			node.GetUnsupported().Children = children.Children
		}
	}
	for _, node := range layout.Nodes {
		if node.Source == nil {
			return nil, ErrInvalidInput
		}
	}
	if len(layout.Views) > 128 || len(layout.Nodes) > 2432 {
		return nil, ErrInvalidInput
	}
	return layout, nil
}

func nativeRecipeKind(kind RecipeKind) api.DashboardRecipeKind {
	switch kind {
	case RecipeTemplate:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_TEMPLATE
	case RecipeSelect:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_SELECT
	case RecipeAlarm:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_ALARM
	case RecipeBubbleButton:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_BUBBLE_BUTTON
	case RecipeBubblePopup:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_BUBBLE_POPUP
	case RecipeChips:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_CHIPS
	case RecipeTemplateChip:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_TEMPLATE_CHIP
	case RecipeButton:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_BUTTON
	case RecipeEntity:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_ENTITY
	case RecipeText:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_TEXT
	case RecipeInvalid:
		return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_UNSPECIFIED
	}
	return api.DashboardRecipeKind_DASHBOARD_RECIPE_KIND_UNSPECIFIED
}

func nativeFeatureKind(kind FeatureKind) api.DashboardFeatureKind {
	switch kind {
	case FeatureToggle:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_TOGGLE
	case FeatureBrightness:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_BRIGHTNESS
	case FeatureColorTemperature:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_COLOR_TEMPERATURE
	case FeatureTargetTemperature:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_TARGET_TEMPERATURE
	case FeatureClimateModes:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_CLIMATE_MODES
	case FeatureFanSpeed:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_FAN_SPEED
	case FeatureNumericInput:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_NUMERIC_INPUT
	case FeatureCoverOpenClose:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_COVER_OPEN_CLOSE
	case FeatureCoverPosition:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_COVER_POSITION
	case FeatureAlarmModes:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_ALARM_MODES
	case FeatureInvalid:
		return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_UNSPECIFIED
	}
	return api.DashboardFeatureKind_DASHBOARD_FEATURE_KIND_UNSPECIFIED
}
