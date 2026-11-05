package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

// TestNoSDKTypesInPublicAPI enforces the boundary DL-029 draws: the Modal
// SDK must not appear in any type the rest of spov touches, so that an SDK
// upgrade or a second provider changes only this package.
//
// @decision(DL-029)
func TestNoSDKTypesInPublicAPI(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(Spec{}),
		reflect.TypeOf(Info{}),
		reflect.TypeOf(EgressPolicy{}),
		reflect.TypeOf(ExecResult{}),
		reflect.TypeOf(ModalConfig{}),
		reflect.TypeOf((*Provider)(nil)).Elem(),
		reflect.TypeOf((*Handle)(nil)).Elem(),
	}
	for _, typ := range types {
		t.Run(typ.Name(), func(t *testing.T) {
			for _, path := range referencedPackages(typ, map[reflect.Type]bool{}) {
				if strings.Contains(path, "modal-client") {
					t.Errorf("%s exposes a Modal SDK type from %s", typ.Name(), path)
				}
			}
		})
	}
}

// referencedPackages walks a type and reports the import paths it reaches.
func referencedPackages(t reflect.Type, seen map[reflect.Type]bool) []string {
	if t == nil || seen[t] {
		return nil
	}
	seen[t] = true

	var out []string
	if p := t.PkgPath(); p != "" {
		out = append(out, p)
	}

	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		out = append(out, referencedPackages(t.Elem(), seen)...)
	case reflect.Map:
		out = append(out, referencedPackages(t.Key(), seen)...)
		out = append(out, referencedPackages(t.Elem(), seen)...)
	case reflect.Struct:
		for i := range t.NumField() {
			out = append(out, referencedPackages(t.Field(i).Type, seen)...)
		}
	case reflect.Interface:
		for i := range t.NumMethod() {
			out = append(out, referencedPackages(t.Method(i).Type, seen)...)
		}
	case reflect.Func:
		for i := range t.NumIn() {
			out = append(out, referencedPackages(t.In(i), seen)...)
		}
		for i := range t.NumOut() {
			out = append(out, referencedPackages(t.Out(i), seen)...)
		}
	}
	return out
}
