package dashboard

import "sort"

func collectRequirements(views []View) Requirements {
	elements := make(map[ElementKind]bool)
	actions := make(map[ActionKind]bool)
	visibility := make(map[VisibilityKind]bool)
	for _, view := range views {
		if view.status != StatusSupported {
			continue
		}
		elements[view.kind] = true
		collectVisibility(visibility, view.visibility)
		for _, section := range view.sections {
			if section.status != StatusSupported {
				continue
			}
			elements[section.kind] = true
			collectVisibility(visibility, section.visibility)
			for _, card := range section.cards {
				collectCard(elements, actions, visibility, card)
			}
		}
	}
	r := Requirements{version: 1}
	for kind := range elements {
		r.elements = append(r.elements, kind)
	}
	for kind := range actions {
		r.actions = append(r.actions, kind)
	}
	for kind := range visibility {
		r.visibility = append(r.visibility, kind)
	}
	sort.Slice(r.elements, func(i, j int) bool { return r.elements[i] < r.elements[j] })
	sort.Slice(r.actions, func(i, j int) bool { return r.actions[i] < r.actions[j] })
	sort.Slice(r.visibility, func(i, j int) bool { return r.visibility[i] < r.visibility[j] })
	return r
}

func collectVisibility(target map[VisibilityKind]bool, values []Visibility) {
	for _, value := range values {
		if value.status == StatusSupported {
			target[value.kind] = true
		}
	}
}

func collectCard(elements map[ElementKind]bool, actions map[ActionKind]bool, visibility map[VisibilityKind]bool, card Card) {
	if card.status != StatusSupported {
		return
	}
	elements[card.kind] = true
	collectVisibility(visibility, card.visibility)
	for _, action := range card.actions {
		if action.status == StatusSupported && action.kind != ActionAbsent {
			actions[action.kind] = true
		}
	}
	for _, child := range card.children {
		collectCard(elements, actions, visibility, child)
	}
}
