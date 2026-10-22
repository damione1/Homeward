package configplans

import "sort"

func difference(base, next image) []Change {
	keys := map[string]bool{}
	for k := range base.fields {
		keys[k] = true
	}
	for k := range next.fields {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	var added, removed uint32
	for ref := range next.refs {
		if !base.refs[ref] {
			added++
		}
	}
	for ref := range base.refs {
		if !next.refs[ref] {
			removed++
		}
	}
	changes := []Change{}
	for _, key := range ordered {
		left, leftPresent := base.fields[key]
		right, rightPresent := next.fields[key]
		if leftPresent && rightPresent && left == right {
			continue
		}
		operation := Replaced
		if !leftPresent {
			operation = Added
		} else if !rightPresent {
			operation = Removed
		}
		// Removed fields retain their admitted former effect. Replacements use
		// the stronger admitted effect from either image, never a fallback that
		// erases an ownership change when the next field no longer exists.
		effect := fieldEffect(next, key)
		if !rightPresent {
			effect = fieldEffect(base, key)
		} else if leftPresent {
			if previous := fieldEffect(base, key); previous > effect {
				effect = previous
			}
		}
		changes = append(changes, Change{effect: effect, operation: operation, field: key})
	}
	if len(changes) > 0 {
		changes[0].referencesAdded = added
		changes[0].referencesRemoved = removed
	}
	return changes
}

func fieldEffect(value image, key string) Effect {
	if effect, ok := value.fieldEffects[key]; ok {
		return effect
	}
	return value.effect
}
