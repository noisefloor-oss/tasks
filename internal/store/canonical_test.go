package store

import (
	"strings"
	"testing"
)

func TestJSONNestingDepthBoundaries(t *testing.T) {
	for _, container := range []struct {
		name        string
		open, close string
	}{
		{name: "array", open: "[", close: "]"},
		{name: "object", open: `{"x":`, close: "}"},
	} {
		t.Run(container.name, func(t *testing.T) {
			for _, boundary := range []struct {
				name    string
				depth   int
				wantErr bool
			}{
				{name: "limit", depth: maxJSONNestingDepth},
				{name: "limit plus one", depth: maxJSONNestingDepth + 1, wantErr: true},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					raw := strings.Repeat(container.open, boundary.depth) + "null" + strings.Repeat(container.close, boundary.depth)
					check := func(name string, err error) {
						t.Helper()
						if boundary.wantErr {
							if err == nil || !strings.Contains(err.Error(), "nesting depth exceeds 10000") {
								t.Fatalf("%s error = %v, want nesting-depth refusal", name, err)
							}
						} else if err != nil {
							t.Fatalf("%s at limit: %v", name, err)
						}
					}

					value, err := decodeJSON([]byte(raw))
					check("decodeJSON", err)
					check("rejectDuplicateKeys", rejectDuplicateKeys([]byte(raw)))
					if boundary.wantErr {
						value = nestedJSONValue(container.name, boundary.depth)
					}
					encoded, err := canonicalJSON(value)
					check("canonicalJSON", err)
					if !boundary.wantErr && string(encoded) != raw {
						t.Fatalf("canonicalJSON changed valid nested value")
					}
				})
			}
		})
	}
}

func nestedJSONValue(container string, depth int) any {
	var value any = nil
	for range depth {
		if container == "array" {
			value = []any{value}
		} else {
			value = map[string]any{"x": value}
		}
	}
	return value
}
