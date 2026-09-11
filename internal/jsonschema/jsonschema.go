// Package jsonschema implements the small subset of JSON Schema that the
// coreutils MCP tools use. It is deliberately strict: anything the subset does
// not understand is rejected rather than silently accepted.
package jsonschema

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Validate checks value against schema. schema and value must both be decoded
// JSON values.
func Validate(schema map[string]any, value any) error {
	return validate(schema, value, "")
}

// ValidateRaw decodes raw JSON arguments and validates them against schema.
func ValidateRaw(schema map[string]any, raw json.RawMessage) (map[string]any, error) {
	arguments := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&arguments); err != nil {
			return nil, fmt.Errorf("arguments must be a JSON object")
		}
	}
	if err := Validate(schema, arguments); err != nil {
		return nil, err
	}
	return arguments, nil
}

func validate(schema map[string]any, value any, path string) error {
	where := path
	if where == "" {
		where = "arguments"
	}

	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, candidate := range enum {
			if equal(candidate, value) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s is not one of the allowed values", where)
		}
	}
	if branches, ok := schema["anyOf"].([]any); ok {
		if err := validateAnyOf(branches, value, path, where); err != nil {
			return err
		}
	}

	switch schemaType, _ := schema["type"].(string); schemaType {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", where)
		}
		properties, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				key, _ := name.(string)
				if _, present := object[key]; !present {
					return fmt.Errorf("%s is missing required property %q", where, key)
				}
			}
		}
		if allowed, ok := schema["additionalProperties"].(bool); ok && !allowed {
			for key := range object {
				if _, declared := properties[key]; !declared {
					return fmt.Errorf("%s has unknown property %q", where, key)
				}
			}
		}
		for key, item := range object {
			property, ok := properties[key].(map[string]any)
			if !ok {
				continue
			}
			if err := validate(property, item, join(path, key)); err != nil {
				return err
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", where)
		}
		if limit, ok := numberOf(schema["maxItems"]); ok && float64(len(array)) > limit {
			return fmt.Errorf("%s has too many items", where)
		}
		if limit, ok := numberOf(schema["minItems"]); ok && float64(len(array)) < limit {
			return fmt.Errorf("%s has too few items", where)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for index, item := range array {
				if err := validate(items, item, fmt.Sprintf("%s[%d]", where, index)); err != nil {
					return err
				}
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", where)
		}
		if limit, ok := numberOf(schema["maxLength"]); ok && float64(len(text)) > limit {
			return fmt.Errorf("%s is too long", where)
		}
		if limit, ok := numberOf(schema["minLength"]); ok && float64(len(text)) < limit {
			return fmt.Errorf("%s is too short", where)
		}
	case "integer":
		number, ok := numberOf(value)
		if !ok || number != float64(int64(number)) {
			return fmt.Errorf("%s must be an integer", where)
		}
		return validateRange(schema, number, where)
	case "number":
		number, ok := numberOf(value)
		if !ok {
			return fmt.Errorf("%s must be a number", where)
		}
		return validateRange(schema, number, where)
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", where)
		}
	case "":
		// No type constraint.
	default:
		return fmt.Errorf("%s uses an unsupported schema type", where)
	}
	return nil
}

func validateAnyOf(branches []any, value any, path, where string) error {
	if typed, ok := value.(map[string]any); ok {
		if branch, ok := selectSchemaBranch(branches, typed); ok {
			return validate(branch, value, path)
		}
	}
	var firstErr error
	for _, branch := range branches {
		schema, ok := branch.(map[string]any)
		if !ok {
			continue
		}
		if err := validate(schema, value, path); err == nil {
			return nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return fmt.Errorf("%s does not match any allowed schema", where)
}

func selectSchemaBranch(branches []any, value map[string]any) (map[string]any, bool) {
	actionType, _ := value["type"].(string)
	for _, branch := range branches {
		schema, ok := branch.(map[string]any)
		if !ok {
			continue
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			continue
		}
		typeSchema, ok := properties["type"].(map[string]any)
		if !ok {
			continue
		}
		enum, ok := typeSchema["enum"].([]any)
		if !ok {
			continue
		}
		for _, candidate := range enum {
			if candidate == actionType {
				return schema, true
			}
		}
	}
	return nil, false
}

func validateRange(schema map[string]any, number float64, where string) error {
	if minimum, ok := numberOf(schema["minimum"]); ok && number < minimum {
		return fmt.Errorf("%s is below the allowed minimum", where)
	}
	if maximum, ok := numberOf(schema["maximum"]); ok && number > maximum {
		return fmt.Errorf("%s is above the allowed maximum", where)
	}
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func equal(left, right any) bool {
	leftNumber, leftOK := numberOf(left)
	rightNumber, rightOK := numberOf(right)
	if leftOK && rightOK {
		return leftNumber == rightNumber
	}
	return left == right
}

// numberOf converts the numeric JSON representations used by encoding/json
// (float64 and json.Number) to float64.
func numberOf(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return number, true
	default:
		return 0, false
	}
}

// Number converts a decoded JSON number to int, reporting failure.
func Number(value any) (int, bool) {
	number, ok := numberOf(value)
	if !ok || number != float64(int64(number)) {
		return 0, false
	}
	return int(number), true
}
