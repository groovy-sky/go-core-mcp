package jsonschema

import (
	"encoding/json"
	"testing"
)

var schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"path":  map[string]any{"type": "string", "maxLength": 8},
		"lines": map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
		"mode":  map[string]any{"enum": []any{"a", "b"}},
		"flag":  map[string]any{"type": "boolean"},
		"fields": map[string]any{
			"type":     "array",
			"items":    map[string]any{"type": "integer", "minimum": 1},
			"maxItems": 2,
		},
	},
	"required":             []any{"path"},
	"additionalProperties": false,
}

func TestValidateRawAcceptsValidArguments(t *testing.T) {
	arguments, err := ValidateRaw(schema, json.RawMessage(`{"path":"a.txt","lines":5,"mode":"a","flag":true,"fields":[1,2]}`))
	if err != nil {
		t.Fatalf("expected valid arguments: %v", err)
	}
	if arguments["path"] != "a.txt" {
		t.Fatalf("unexpected arguments %+v", arguments)
	}
	if lines, ok := Number(arguments["lines"]); !ok || lines != 5 {
		t.Fatalf("unexpected numeric argument %v", arguments["lines"])
	}
}

func TestValidateRawRejectsInvalidArguments(t *testing.T) {
	cases := map[string]string{
		"missing required": `{}`,
		"unknown property": `{"path":"a.txt","shell":"rm"}`,
		"wrong type":       `{"path":123}`,
		"too long":         `{"path":"aaaaaaaaaaaa"}`,
		"below minimum":    `{"path":"a.txt","lines":0}`,
		"above maximum":    `{"path":"a.txt","lines":10000}`,
		"non integer":      `{"path":"a.txt","lines":1.5}`,
		"invalid enum":     `{"path":"a.txt","mode":"c"}`,
		"non boolean":      `{"path":"a.txt","flag":"yes"}`,
		"too many items":   `{"path":"a.txt","fields":[1,2,3]}`,
		"invalid item":     `{"path":"a.txt","fields":["x"]}`,
		"not an object":    `"a.txt"`,
		"malformed json":   `{`,
	}
	for name, arguments := range cases {
		if _, err := ValidateRaw(schema, json.RawMessage(arguments)); err == nil {
			t.Errorf("%s: expected validation to fail for %s", name, arguments)
		}
	}
}

func TestValidateRejectsUnsupportedSchemaTypes(t *testing.T) {
	if err := Validate(map[string]any{"type": "null"}, nil); err == nil {
		t.Fatal("expected an unsupported schema type to be rejected")
	}
}

func TestValidateRawAnyOfObjectBranching(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{"enum": []any{"click"}},
					"path": map[string]any{"type": "string", "minLength": 1},
				},
				"required":             []any{"type", "path"},
				"additionalProperties": false,
			},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":  map[string]any{"enum": []any{"type"}},
					"path":  map[string]any{"type": "string", "minLength": 1},
					"value": map[string]any{"type": "string", "minLength": 1},
				},
				"required":             []any{"type", "path", "value"},
				"additionalProperties": false,
			},
		},
	}

	if _, err := ValidateRaw(schema, json.RawMessage(`{"type":"click","path":"#ok"}`)); err != nil {
		t.Fatalf("expected click branch to validate: %v", err)
	}
	if _, err := ValidateRaw(schema, json.RawMessage(`{"type":"type","path":"#ok","value":"hello"}`)); err != nil {
		t.Fatalf("expected type branch to validate: %v", err)
	}
	if _, err := ValidateRaw(schema, json.RawMessage(`{"type":"type","path":"#ok"}`)); err == nil || err.Error() != `arguments is missing required property "value"` {
		t.Fatalf("expected missing value error, got %v", err)
	}
	if _, err := ValidateRaw(schema, json.RawMessage(`{"type":"submit","path":"#ok"}`)); err == nil || err.Error() != "type is not one of the allowed values" {
		t.Fatalf("expected enum error for unsupported type, got %v", err)
	}
}
