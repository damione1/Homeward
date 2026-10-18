package hilo

import "reflect"

// Interfaces at the Host/B2C boundary may contain typed nil pointers.
func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	probe := reflect.ValueOf(value)
	switch probe.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return probe.IsNil()
	default:
		return false
	}
}
