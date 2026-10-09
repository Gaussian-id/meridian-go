package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func run(t *testing.T, in string) map[string]any {
	t.Helper()

	var doc any
	if err := json.Unmarshal([]byte(in), &doc); err != nil {
		t.Fatal(err)
	}

	return convert(doc).(map[string]any)
}

func TestNullableTypeArrayBecomesNullable(t *testing.T) {
	got := run(t, `{"type":["string","null"],"description":"d"}`)
	want := map[string]any{"type": "string", "nullable": true, "description": "d"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOneOfWithNullBecomesTheRef(t *testing.T) {
	got := run(t, `{"oneOf":[{"$ref":"#/components/schemas/X"},{"type":"null"}],"description":"d"}`)
	want := map[string]any{"$ref": "#/components/schemas/X"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTrueUnionBecomesUntyped(t *testing.T) {
	got := run(t, `{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"d"}`)
	want := map[string]any{"description": "d"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestConstUUIDAndOpenObjectsAreSimplified(t *testing.T) {
	got := run(t, `{"type":"object","additionalProperties":true,"properties":{
		"a":{"const":"x"},"b":{"type":"string","format":"uuid"},"c":{"type":"integer","format":"int64"}}}`)
	want := map[string]any{"type": "object", "properties": map[string]any{
		"a": map[string]any{"enum": []any{"x"}},
		"b": map[string]any{"type": "string"},
		"c": map[string]any{"type": "integer", "format": "int64"},
	}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
