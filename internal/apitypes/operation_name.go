package apitypes

import "strings"

// OperationName is an admitted relative operation identity. It grants neither
// knowledge of a result nor permission to redispatch a physical intention.
type OperationName struct{ value string }

func ParseOperationName(text string) (OperationName, error) {
	if err := resourceName(text, "homes", "operations"); err != nil {
		return OperationName{}, err
	}
	return OperationName{value: text}, nil
}
func (n OperationName) String() string { return n.value }
func (n OperationName) Valid() bool    { return n.value != "" }
func (n OperationName) Home() HomeName {
	if !n.Valid() {
		return HomeName{}
	}
	return HomeName{value: n.value[:strings.LastIndex(n.value, "/operations/")]}
}
func (n OperationName) ID() string {
	if !n.Valid() {
		return ""
	}
	return n.value[strings.LastIndex(n.value, "/")+1:]
}
