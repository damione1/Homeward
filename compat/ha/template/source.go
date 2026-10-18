package template

import (
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
)

const (
	maxReadEntities = 4096
	maxReadAliases  = 16384
	maxReadBytes    = 8 << 20
)

// ReadSource is one registered entity's current value, as opposed to its
// last-known display value. Nil text is a known unavailable/unknown entity.
type ReadSource struct {
	entity     domain.EntityID
	aliases    []domain.HAAlias
	state      State
	present    bool
	quality    domain.Quality
	hasQuality bool
}

func NewReadSource(entity domain.EntityID, aliases []domain.HAAlias, text *string, attrs []Binding, quality domain.Quality) (ReadSource, error) {
	if _, err := entity.Value(); err != nil || len(aliases) == 0 || len(aliases) > 256 {
		return ReadSource{}, ErrInvalidInput
	}
	availability, _, _, err := quality.Components()
	if err != nil || (availability == domain.AvailabilityAvailable) != (text != nil) || (text == nil && len(attrs) != 0) {
		return ReadSource{}, ErrInvalidInput
	}
	seen := make(map[domain.HAAlias]struct{}, len(aliases))
	for _, alias := range aliases {
		if _, err := alias.Value(); err != nil {
			return ReadSource{}, ErrInvalidInput
		}
		if _, exists := seen[alias]; exists {
			return ReadSource{}, ErrInvalidInput
		}
		seen[alias] = struct{}{}
	}
	source := ReadSource{entity: entity, aliases: append([]domain.HAAlias(nil), aliases...), quality: quality, hasQuality: true}
	if text != nil {
		state, err := NewState(aliases[0], *text, attrs)
		if err != nil {
			return ReadSource{}, err
		}
		source.state, source.present = state, true
	} else {
		source.state.alias = aliases[0]
		if availability == domain.AvailabilityUnavailable {
			source.state.text = "unavailable"
		} else {
			source.state.text = "unknown"
		}
	}
	return source, nil
}

// NewUnobservedReadSource keeps a registry identity without inventing a
// quality report or a current value before its first Host observation.
func NewUnobservedReadSource(entity domain.EntityID, aliases []domain.HAAlias) (ReadSource, error) {
	if _, err := entity.Value(); err != nil || len(aliases) == 0 || len(aliases) > 256 {
		return ReadSource{}, ErrInvalidInput
	}
	seen := make(map[domain.HAAlias]struct{}, len(aliases))
	for _, alias := range aliases {
		if _, err := alias.Value(); err != nil {
			return ReadSource{}, ErrInvalidInput
		}
		if _, exists := seen[alias]; exists {
			return ReadSource{}, ErrInvalidInput
		}
		seen[alias] = struct{}{}
	}
	return ReadSource{entity: entity, aliases: append([]domain.HAAlias(nil), aliases...), state: State{alias: aliases[0], text: "unknown"}}, nil
}

// ReadGroup has an explicit generation of membership. It carries no inferred
// on/off state; only expand(group.x) consumes it.
type ReadGroup struct {
	alias   domain.HAAlias
	members []domain.HAAlias
}

func NewReadGroup(alias domain.HAAlias, members []domain.HAAlias) (ReadGroup, error) {
	name, err := alias.Value()
	if err != nil || len(name) < 7 || name[:6] != "group." || len(members) > maxItems {
		return ReadGroup{}, ErrInvalidInput
	}
	seen := make(map[domain.HAAlias]struct{}, len(members))
	for _, member := range members {
		if _, err := member.Value(); err != nil || member == alias {
			return ReadGroup{}, ErrInvalidInput
		}
		if _, exists := seen[member]; exists {
			return ReadGroup{}, ErrInvalidInput
		}
		seen[member] = struct{}{}
	}
	return ReadGroup{alias: alias, members: append([]domain.HAAlias(nil), members...)}, nil
}

type readItem struct {
	entity     domain.EntityID
	state      State
	present    bool
	quality    domain.Quality
	hasQuality bool
}

// ReadView owns a bounded, immutable full registry/projection image. It never
// feeds all 4096 identities through NewInputs' deliberate 128-state limit.
type ReadView struct {
	sources  map[string]readItem
	overlays map[string]readItem
	groups   map[string][]string
	valid    bool
}

func (v ReadView) lookup(alias string) (readItem, bool) {
	if item, ok := v.overlays[alias]; ok {
		return item, true
	}
	item, ok := v.sources[alias]
	return item, ok
}

// OverlayCost is the number of alias slots copied or changed by WithCandidate.
// The source image itself is shared and remains immutable.
func (v ReadView) OverlayCost(aliases []domain.HAAlias) int { return len(v.overlays) + len(aliases) }

func NewReadView(sources []ReadSource, groups []ReadGroup) (ReadView, error) {
	if len(sources) > maxReadEntities || len(groups) > maxItems {
		return ReadView{}, ErrBudget
	}
	view := ReadView{sources: make(map[string]readItem), groups: make(map[string][]string), valid: true}
	identities := make(map[domain.EntityID]struct{}, len(sources))
	aliases, bytes := 0, 0
	for _, source := range sources {
		if _, err := source.entity.Value(); err != nil || len(source.aliases) == 0 || len(source.aliases) > 256 {
			return ReadView{}, ErrInvalidInput
		}
		if _, exists := identities[source.entity]; exists {
			return ReadView{}, ErrInvalidInput
		}
		identities[source.entity] = struct{}{}
		if source.hasQuality {
			availability, _, _, err := source.quality.Components()
			if err != nil || (availability == domain.AvailabilityAvailable) != source.present {
				return ReadView{}, ErrInvalidInput
			}
		} else if source.present || source.state.text != "unknown" {
			return ReadView{}, ErrInvalidInput
		}
		if source.present {
			if _, err := NewState(source.aliases[0], source.state.text, stateBindings(source.state)); err != nil {
				return ReadView{}, err
			}
		}
		sourceBytes := len(source.state.text)
		for key, value := range source.state.attrs {
			sourceBytes += len(key) + approximateValueBytes(value)
		}
		for _, alias := range source.aliases {
			name, err := alias.Value()
			if err != nil {
				return ReadView{}, ErrInvalidInput
			}
			if _, exists := view.sources[name]; exists {
				return ReadView{}, ErrInvalidInput
			}
			aliases++
			bytes += len(name) + sourceBytes
			if aliases > maxReadAliases || bytes > maxReadBytes {
				return ReadView{}, ErrBudget
			}
			state := source.state
			state.alias = alias
			state.attrs = cloneAttrs(source.state.attrs)
			view.sources[name] = readItem{entity: source.entity, state: state, present: source.present, quality: source.quality, hasQuality: source.hasQuality}
		}
	}
	for _, group := range groups {
		name, err := group.alias.Value()
		if err != nil || len(name) < 7 || name[:6] != "group." || len(group.members) > maxItems {
			return ReadView{}, ErrInvalidInput
		}
		if _, err := NewReadGroup(group.alias, group.members); err != nil {
			return ReadView{}, err
		}
		if _, exists := view.groups[name]; exists {
			return ReadView{}, ErrInvalidInput
		}
		members := make([]string, 0, len(group.members))
		for _, member := range group.members {
			id, err := member.Value()
			if err != nil || id == name {
				return ReadView{}, ErrInvalidInput
			}
			members = append(members, id)
			bytes += len(id)
		}
		bytes += len(name)
		aliases += 1 + len(members)
		if aliases > maxReadAliases || bytes > maxReadBytes {
			return ReadView{}, ErrBudget
		}
		view.groups[name] = members
	}
	return view, nil
}

func stateBindings(state State) []Binding {
	bindings := make([]Binding, 0, len(state.attrs))
	for key, value := range state.attrs {
		bindings = append(bindings, Binding{Name: key, Value: value})
	}
	return bindings
}

func cloneAttrs(attrs map[string]Value) map[string]Value {
	if attrs == nil {
		return nil
	}
	copyAttrs := make(map[string]Value, len(attrs))
	for key, value := range attrs {
		copyAttrs[key] = cloneValue(value)
	}
	return copyAttrs
}

func approximateValueBytes(value Value) int {
	switch value.kind {
	case valueString:
		return len(value.s)
	case valueList:
		size := 8
		for _, item := range value.list {
			size += approximateValueBytes(item)
		}
		return size
	case valueObject:
		size := 8
		for key, item := range value.obj {
			size += len(key) + approximateValueBytes(item)
		}
		return size
	default:
		return 16
	}
}

func NewInputsFromReadView(view ReadView, variables []Binding, nowUTC time.Time, zone *time.Location) (Inputs, error) {
	if !view.valid {
		return Inputs{}, ErrInvalidInput
	}
	in, err := NewInputs(nil, variables, nowUTC, zone)
	if err != nil {
		return Inputs{}, err
	}
	in.view = &view
	return in, nil
}

// WithCandidate overlays a private, not-yet-published calculated text for all
// aliases of one already registered identity. It never changes the Host image.
func (v ReadView) WithCandidate(aliases []domain.HAAlias, text string, quality domain.Quality) (ReadView, error) {
	availability, _, provenance, err := quality.Components()
	if !v.valid || err != nil || availability != domain.AvailabilityAvailable || provenance != domain.ProvenanceEstimated || len(text) > maxStringBytes || !utf8.ValidString(text) || len(aliases) == 0 {
		return ReadView{}, ErrInvalidInput
	}
	copyOverlays := make(map[string]readItem, len(v.overlays)+len(aliases))
	for key, source := range v.overlays {
		copyOverlays[key] = source
	}
	for _, alias := range aliases {
		name, err := alias.Value()
		if err != nil {
			return ReadView{}, ErrInvalidInput
		}
		source, exists := v.lookup(name)
		if !exists {
			return ReadView{}, ErrInvalidInput
		}
		source.state = State{alias: alias, text: text}
		source.present, source.quality, source.hasQuality = true, quality, true
		copyOverlays[name] = source
	}
	return ReadView{sources: v.sources, overlays: copyOverlays, groups: v.groups, valid: true}, nil
}
