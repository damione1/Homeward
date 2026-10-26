package dashboard

import (
	"crypto/sha256"
	"strconv"
	"strings"

	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

type navigationTarget struct {
	dashboard    authorization.DashboardID
	view         string
	node         string
	presentation wire.NativePresentation
}
type popupKey struct {
	dashboard authorization.DashboardID
	view      string
	anchor    PopupAnchor
}
type navigationContext struct {
	dashboard authorization.DashboardID
	view      string
}
type navigationDefinition struct {
	source  Source
	digest  [sha256.Size]byte
	profile string
}

// NavigationIndex belongs to one authorized configuration snapshot. A server
// rebuilds it on every document/rights change; numeric HA links are aliases in
// that snapshot only, never a client row identity or a persisted room identity.
// Source paths and explicit configured routes/hashes resolve to native nodes;
// no room names, alias fragments, network access or device command is involved.
type NavigationIndex struct {
	pages       map[string]navigationTarget
	popups      map[popupKey]navigationTarget
	contexts    map[navigationContext]bool
	definitions map[authorization.DashboardID]navigationDefinition
}

func NewNavigationIndex(definitions []Definition) (NavigationIndex, error) {
	index := NavigationIndex{pages: map[string]navigationTarget{}, popups: map[popupKey]navigationTarget{}, contexts: map[navigationContext]bool{}, definitions: map[authorization.DashboardID]navigationDefinition{}}
	ids, urls := map[authorization.DashboardID]bool{}, map[string]bool{}
	if len(definitions) > 128 {
		return NavigationIndex{}, ErrInvalidInput
	}
	for _, definition := range definitions {
		if !definition.source.valid() || (definition.profile != Profile && definition.profile != RecipesProfile) || ids[definition.source.id] || urls[definition.source.urlPath] {
			return NavigationIndex{}, ErrInvalidInput
		}
		ids[definition.source.id], urls[definition.source.urlPath] = true, true
		index.definitions[definition.source.id] = navigationDefinition{source: definition.source, digest: definition.digest, profile: definition.profile}
		for position, view := range definition.views {
			if view.status != StatusSupported {
				continue
			}
			index.contexts[navigationContext{dashboard: definition.source.id, view: view.location.path}] = true
			target := navigationTarget{dashboard: definition.source.id, view: view.location.path, node: view.location.path, presentation: wire.NativePresentation_NATIVE_PRESENTATION_PAGE}
			routes := []string{"/" + definition.source.urlPath + "/" + strconv.Itoa(position)}
			if position == 0 {
				routes = append(routes, "/"+definition.source.urlPath)
			}
			if view.path != "" {
				routes = append(routes, "/"+definition.source.urlPath+"/"+view.path)
			}
			for _, path := range routes {
				if previous, exists := index.pages[path]; exists && previous != target {
					return NavigationIndex{}, ErrInvalidInput
				}
				index.pages[path] = target
			}
			var visit func([]Card) error
			visit = func(cards []Card) error {
				for _, card := range cards {
					if card.status != StatusSupported {
						continue
					}
					if anchor, present := card.Anchor(); present {
						key := popupKey{dashboard: definition.source.id, view: view.location.path, anchor: anchor}
						if _, duplicate := index.popups[key]; duplicate {
							return ErrInvalidInput
						}
						index.popups[key] = navigationTarget{dashboard: definition.source.id, view: view.location.path, node: card.location.path, presentation: wire.NativePresentation_NATIVE_PRESENTATION_POPUP}
					}
					if err := visit(card.children); err != nil {
						return err
					}
				}
				return nil
			}
			for _, section := range view.sections {
				if section.status != StatusSupported {
					continue
				}
				if err := visit(section.cards); err != nil {
					return NavigationIndex{}, err
				}
			}
		}
	}
	return index, nil
}

func (i NavigationIndex) matches(definition Definition) bool {
	expected, present := i.definitions[definition.source.id]
	return present && expected.source == definition.source && expected.digest == definition.digest && expected.profile == definition.profile
}

func (t navigationTarget) wire() *wire.NativeDestination {
	value, err := t.dashboard.Value()
	if err != nil {
		return nil
	}
	return &wire.NativeDestination{DashboardId: &wire.DashboardId{Value: value}, NodeId: t.node, Presentation: t.presentation}
}

// Resolve accepts only the exact local routes captured from this snapshot.
// Anchor lookup is scoped to its source view, including cross-dashboard links.
// A missing/unsupported destination never falls back to a row, external browser
// or action. Returned messages are detached from the immutable index.
func (i NavigationIndex) Resolve(from authorization.DashboardID, view Location, path string) (*wire.NativeDestination, bool) {
	if !i.contexts[navigationContext{dashboard: from, view: view.path}] || !validNavigation(path) {
		return nil, false
	}
	page, fragment, hasFragment := strings.Cut(path, "#")
	if page == "" {
		if !hasFragment {
			return nil, false
		}
		anchor, err := ParsePopupAnchor("#" + fragment)
		if err != nil {
			return nil, false
		}
		target, present := i.popups[popupKey{dashboard: from, view: view.path, anchor: anchor}]
		if !present {
			return nil, false
		}
		return target.wire(), true
	}
	target, present := i.pages[page]
	if !present {
		return nil, false
	}
	if hasFragment {
		anchor, err := ParsePopupAnchor("#" + fragment)
		if err != nil {
			return nil, false
		}
		target, present = i.popups[popupKey{dashboard: target.dashboard, view: target.view, anchor: anchor}]
		if !present {
			return nil, false
		}
	}
	return target.wire(), true
}
