package meridian_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var priceKey = regexp.MustCompile(`(?i)cost|price|per_million`)

type jsonField struct {
	typ       reflect.Type
	omitempty bool
}

func jsonFields(t reflect.Type) map[string]jsonField {
	out := map[string]jsonField{}

	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}

		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}

		out[name] = jsonField{typ: f.Type, omitempty: strings.Contains(opts, "omitempty")}
	}

	return out
}

func baseKind(t reflect.Type) reflect.Kind {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflect.Int
	case reflect.Float32, reflect.Float64:
		return reflect.Float64
	default:
		return t.Kind()
	}
}

func nilable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	default:
		return false
	}
}

var rawMessage = reflect.TypeOf(json.RawMessage(nil))

// assertConforms holds a hand-written SDK type to the type oapi-codegen generated from the
// pinned engine document: same JSON names, nothing missing but price/cost and listed omissions,
// matching kinds, and a nilable Go type wherever the wire can carry null.
func assertConforms(t *testing.T, schema string, hand, gen any, omit ...string) {
	t.Helper()

	h, g := jsonFields(reflect.TypeOf(hand)), jsonFields(reflect.TypeOf(gen))
	skip := map[string]bool{}

	for _, o := range omit {
		skip[o] = true
	}

	for name := range h {
		if _, ok := g[name]; !ok {
			t.Errorf("%s: SDK field %q is not in the engine schema", schema, name)
		}

		if priceKey.MatchString(name) {
			t.Errorf("%s: SDK type exports the price/cost field %q (D-27)", schema, name)
		}
	}

	for name, gf := range g {
		hf, ok := h[name]
		if !ok {
			if !skip[name] && !priceKey.MatchString(name) {
				t.Errorf("%s: engine field %q is missing from the SDK type", schema, name)
			}

			continue
		}

		if gf.typ.Kind() == reflect.Pointer && !gf.omitempty && !nilable(hf.typ) {
			t.Errorf("%s.%s can be null on the wire; the SDK type must be a pointer, slice, map or interface", schema, name)
		}

		gk, hk := baseKind(gf.typ), baseKind(hf.typ)
		if gk != hk && gk != reflect.Interface && hk != reflect.Interface && hf.typ != rawMessage {
			t.Errorf("%s.%s: kind %s in the engine schema, %s in the SDK", schema, name, gk, hk)
		}
	}
}
