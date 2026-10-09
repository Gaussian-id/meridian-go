// Command downconvert rewrites an OpenAPI 3.1 document into the OpenAPI 3.0 subset that
// oapi-codegen turns into plain standard-library Go. It is a development tool; nothing in
// the SDK imports it.
//
// Usage: go run ./internal/cmd/downconvert <in.json> <out.json>
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: downconvert <in.json> <out.json>")
		os.Exit(2)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	root := convert(doc).(map[string]any)
	root["openapi"] = "3.0.3"
	delete(root, "jsonSchemaDialect")
	delete(root, "webhooks")

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := os.WriteFile(os.Args[2], append(out, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// convert rewrites every map bottom-up.
func convert(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			x[k] = convert(child)
		}

		return rewrite(x)
	case []any:
		for i := range x {
			x[i] = convert(x[i])
		}

		return x
	default:
		return v
	}
}

// rewrite turns one 3.1 schema into its 3.0 equivalent.
func rewrite(m map[string]any) map[string]any {
	// [1] type: [T, "null"] → type T, nullable; several non-null types → untyped.
	if types, ok := m["type"].([]any); ok {
		var keep []string

		null := false

		for _, t := range types {
			if s, _ := t.(string); s == "null" {
				null = true
			} else if s != "" {
				keep = append(keep, s)
			}
		}

		if len(keep) == 1 {
			m["type"] = keep[0]
		} else {
			delete(m, "type")
		}

		if null {
			m["nullable"] = true
		}
	}

	// [2] A bare {"type": "null"} is an empty, nullable schema.
	if m["type"] == "null" {
		delete(m, "type")
		m["nullable"] = true
	}

	// [3] oneOf with a null branch and one other branch → that branch; a true union → untyped.
	if alts, ok := m["oneOf"].([]any); ok {
		var keep []map[string]any

		for _, a := range alts {
			if am, ok := a.(map[string]any); ok && !isNull(am) {
				keep = append(keep, am)
			}
		}

		delete(m, "oneOf")

		if len(keep) == 1 {
			if ref, ok := keep[0]["$ref"]; ok {
				return map[string]any{"$ref": ref}
			}

			for k, v := range keep[0] {
				m[k] = v
			}
		}
	}

	// [4] const → enum; drop 3.1-only and non-standard-library details.
	if c, ok := m["const"]; ok {
		delete(m, "const")
		m["enum"] = []any{c}
	}

	if m["format"] == "uuid" {
		delete(m, "format")
	}

	if m["additionalProperties"] == true {
		delete(m, "additionalProperties")
	}

	for _, k := range []string{"examples", "contentMediaType", "$schema"} {
		delete(m, k)
	}

	for _, k := range []string{"exclusiveMinimum", "exclusiveMaximum"} {
		if _, isBool := m[k].(bool); !isBool {
			delete(m, k)
		}
	}

	return m
}

// isNull reports whether a converted schema only permits null.
func isNull(m map[string]any) bool {
	if m["type"] == "null" {
		return true
	}

	_, hasNullable := m["nullable"]

	return hasNullable && len(m) == 1
}
