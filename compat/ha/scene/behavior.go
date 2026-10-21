package scene

import (
	"github.com/damione1/Homeward/compat/ha/parser"
	"reflect"
)

func (d Definition) SameBehavior(other Definition) bool {
	if _, err := d.ref.ID(); err != nil {
		return false
	}
	if _, err := other.ref.ID(); err != nil || d.ID() != other.ID() {
		return false
	}
	left, right := d.Members(), other.Members()
	for _, members := range [][]Member{left, right} {
		for n := range members {
			members[n].source = parser.Position{}
			for j := range members[n].diagnostics {
				members[n].diagnostics[j].at = parser.Position{}
				members[n].diagnostics[j].path = ""
			}
		}
	}
	return reflect.DeepEqual(left, right)
}
