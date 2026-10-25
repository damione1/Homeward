package server

// The development DashboardService projects the existing importer IR. This
// test-only adapter does not introduce a production persistence service.
import (
	"fmt"
	"strings"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func nhf007Location(location imported.Location, variant string) *dashboardv1.LayoutSource {
	at := location.Position()
	return &dashboardv1.LayoutSource{File: at.File, Line: uint32(max(0, at.Line)),
		Column: uint32(max(0, at.Column)), Path: location.Path(), Variant: variant}
}

func nhf007Dimension(value imported.Dimension, present bool) *dashboardv1.LayoutDimension {
	if !present {
		return nil
	}
	switch value.Kind() {
	case imported.DimensionCount:
		count, _ := value.Count()
		return &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_COUNT, Count: uint32(count)}
	case imported.DimensionFull:
		return &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_FULL}
	case imported.DimensionAuto:
		return &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_AUTO}
	case imported.DimensionInvalid:
		return nil
	}
	return nil
}

func nhf007Kind(value imported.ElementKind) (dashboardv1.LayoutKind, string) {
	switch value {
	case imported.ElementSectionsView:
		return dashboardv1.LayoutKind_LAYOUT_KIND_VIEW, "sections"
	case imported.ElementGridSection:
		return dashboardv1.LayoutKind_LAYOUT_KIND_SECTION, "grid"
	case imported.ElementGridCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_GRID, "grid"
	case imported.ElementHorizontalStackCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_HORIZONTAL_STACK, "horizontal-stack"
	case imported.ElementHeadingCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_HEADING, "heading"
	case imported.ElementTileCard:
		return dashboardv1.LayoutKind_LAYOUT_KIND_TILE, "tile"
	case imported.ElementInvalid:
		return dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED, "unknown"
	}
	return dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED, "unknown"
}

func projectDashboardLayout(result imported.Result, support *dashboardv1.ClientLayoutSupport,
	resolve func(domain.HAAlias) (domain.EntityID, bool)) (*dashboardv1.NativeLayout, error) {
	definition, present := result.Definition()
	if !present || result.Status() == imported.StatusFatal || resolve == nil {
		return nil, ErrInvalidPublicPlan
	}
	layout := &dashboardv1.NativeLayout{Version: 1}
	allowed := map[dashboardv1.LayoutKind]bool{}
	// Old clients did not negotiate a tree. Keep their overview readable; their
	// generated decoder ignores this additive projection. New clients negotiate.
	if support == nil {
		for _, kind := range []dashboardv1.LayoutKind{dashboardv1.LayoutKind_LAYOUT_KIND_VIEW, dashboardv1.LayoutKind_LAYOUT_KIND_SECTION,
			dashboardv1.LayoutKind_LAYOUT_KIND_GRID, dashboardv1.LayoutKind_LAYOUT_KIND_HORIZONTAL_STACK,
			dashboardv1.LayoutKind_LAYOUT_KIND_HEADING, dashboardv1.LayoutKind_LAYOUT_KIND_TILE} {
			allowed[kind] = true
		}
	} else if support.Profile == imported.Profile && support.Version == 1 {
		for _, kind := range support.Kinds {
			allowed[kind] = true
		}
	}
	unsupported := result.Unsupported()
	diagnostics := result.Diagnostics()
	makeNode := func(id string, kind imported.ElementKind, status imported.Status, location imported.Location,
		visibility []imported.Visibility, blocked bool) *dashboardv1.LayoutNode {
		wireKind, variant := nhf007Kind(kind)
		node := &dashboardv1.LayoutNode{Id: id, Kind: wireKind, Source: nhf007Location(location, variant)}
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
		if blocked || status != imported.StatusSupported || len(visibility) != 0 || !allowed[wireKind] {
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
	var cards func([]imported.Card, *dashboardv1.LayoutNode, int) error
	cards = func(values []imported.Card, parent *dashboardv1.LayoutNode, depth int) error {
		if depth > 16 {
			return fmt.Errorf("dashboard card depth exceeds importer profile")
		}
		for _, card := range values {
			node := makeNode(card.Location().Path(), card.Kind(), card.Status(), card.Location(), card.Visibility(), parent.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED)
			parent.Children = append(parent.Children, node.Id)
			node.Title, _ = card.Name()
			if card.Kind() == imported.ElementHeadingCard {
				node.Title = card.Heading()
				switch card.HeadingStyle() {
				case imported.HeadingStyleDefault:
					node.HeadingStyle = dashboardv1.NativeHeadingStyle_NATIVE_HEADING_STYLE_DEFAULT
				case imported.HeadingStyleTitle:
					node.HeadingStyle = dashboardv1.NativeHeadingStyle_NATIVE_HEADING_STYLE_TITLE
				case imported.HeadingStyleSubtitle:
					node.HeadingStyle = dashboardv1.NativeHeadingStyle_NATIVE_HEADING_STYLE_SUBTITLE
				case imported.HeadingStyleInvalid:
				}
			}
			if title, present := card.Title(); present {
				node.Title = title
			}
			if alias, present := card.Entity(); present && node.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_TILE {
				if id, known := resolve(alias); known {
					node.EntityId, _ = id.Value()
				}
				if node.EntityId == "" {
					node.Kind = dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED
					node.Diagnostic = "Entité non disponible dans la vue autorisée."
				}
			}
			if count, present := card.Columns(); present {
				node.Columns = uint32(count)
			}
			node.ColumnSpan = nhf007Dimension(card.GridOptions().Columns())
			node.RowSpan = nhf007Dimension(card.GridOptions().Rows())
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
				if err := cards(card.Children(), node, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, view := range definition.Views() {
		node := makeNode(view.Location().Path(), view.Kind(), view.Status(), view.Location(), view.Visibility(), false)
		node.Title = view.Title()
		node.Route = view.Path()
		if columns, present := view.MaxColumns(); present {
			node.Columns = uint32(columns)
		}
		layout.Views = append(layout.Views, node.Id)
		for _, section := range view.Sections() {
			child := makeNode(section.Location().Path(), section.Kind(), section.Status(), section.Location(), section.Visibility(), node.Kind == dashboardv1.LayoutKind_LAYOUT_KIND_UNSUPPORTED)
			node.Children = append(node.Children, child.Id)
			if count, present := section.ColumnSpan(); present {
				child.ColumnSpan = &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_COUNT, Count: uint32(count)}
			}
			if count, present := section.RowSpan(); present {
				child.RowSpan = &dashboardv1.LayoutDimension{Kind: dashboardv1.LayoutDimensionKind_LAYOUT_DIMENSION_KIND_COUNT, Count: uint32(count)}
			}
			if err := cards(section.Cards(), child, 0); err != nil {
				return nil, err
			}
		}
	}
	// Runtime strategies have no views in the importer. Retain a sourced inert
	// diagnostic as the only view instead of inventing an executable layout.
	if len(layout.Views) == 0 {
		for _, item := range unsupported {
			if strings.Contains(item.Location().Path(), "strategy") {
				node := makeNode(item.Location().Path(), imported.ElementInvalid, imported.StatusUnsupported, item.Location(), nil, false)
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
		return nil, ErrInvalidPublicPlan
	}
	return layout, nil
}
