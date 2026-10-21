package configdocuments

import (
	"context"
	"reflect"
)

func usableContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return !v.IsNil()
	default:
		return true
	}
}
