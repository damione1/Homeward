package configplans

import "github.com/damione1/Homeward/internal/config/bundle"

type bundleCandidate struct{ aliases []string }

func compileBundle(source Candidate) (bundleCandidate, error) {
	var candidate bundle.Candidate
	var err error
	switch source.family {
	case Declarative1:
		candidate, err = bundle.CompileSources(source.entry, source.files)
	case Declarative2:
		candidate, err = bundle.CompileSourcesV2(source.entry, source.files)
	default:
		return bundleCandidate{}, ErrInvalid
	}
	if err != nil || !candidate.Valid() {
		return bundleCandidate{}, ErrInvalid
	}
	result := bundleCandidate{}
	add := func(id interface{ Value() (string, error) }) {
		if name, e := id.Value(); e == nil {
			result.aliases = append(result.aliases, name)
		}
	}
	for _, v := range candidate.Booleans() {
		add(v.ID())
	}
	for _, v := range candidate.Numbers() {
		add(v.ID())
	}
	for _, v := range candidate.Counters() {
		add(v.ID())
	}
	for _, v := range candidate.Buttons() {
		add(v.ID())
	}
	for _, v := range candidate.Datetimes() {
		add(v.ID())
	}
	for _, v := range candidate.Selects() {
		add(v.ID())
	}
	for _, v := range candidate.Texts() {
		add(v.ID())
	}
	for _, v := range candidate.Groups() {
		add(v.ID())
		for _, member := range v.Members() {
			add(member)
		}
	}
	return result, nil
}
