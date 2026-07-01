package configuration

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// schemaVersion is the canonical draft the generated schema declares. The
// generator deliberately sticks to keywords editors support across drafts.
const schemaVersion = "https://json-schema.org/draft/2020-12/schema"

// durationPattern validates the Go duration notation time.ParseDuration
// reads, such as "30s" or "1h30m". The JSON Schema built-in duration format
// means an ISO 8601 duration, which is a different notation, so a pattern is
// used instead.
const durationPattern = `^-?(0|(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+)$`

// GenerateSchema derives a JSON Schema draft 2020-12 document from the
// struct the configuration binds onto: the counterpart of the configuration
// metadata Spring Boot generates from @ConfigurationProperties classes for
// editor completion. Store the output next to the configuration documents
// and point their $schema key at it, so editors and code assistants offer
// completion, type checking, and the descriptions of every key.
//
// Field names follow the json tags. A field is required unless its json tag
// carries omitempty or omitzero. Objects reject unknown properties, matching
// the strict binding of Load; the root additionally allows the $schema key
// itself, so a document carrying the association validates cleanly.
//
// The jsonschema field tag contributes constraints as comma-separated
// key=value items, the grammar established by the Go schema-generation
// ecosystem: title, description, default, enum, example, pattern, format,
// minimum, maximum, minLength, and maxLength, with enum and example
// repeatable and a backslash escaping a literal comma. The
// jsonschema_description tag sets a description as one whole value, the
// comma-safe channel for free text:
//
//	type Server struct {
//	    Host string        `json:"host" jsonschema:"description=Interface to bind,default=localhost"`
//	    Port int           `json:"port" jsonschema:"minimum=1,maximum=65535"`
//	    Mode string        `json:"mode,omitempty" jsonschema:"enum=development,enum=production"`
//	    Timeout time.Duration `json:"timeout,omitempty" jsonschema_description:"Read timeout, as a Go duration."`
//	}
func GenerateSchema(prototype any) ([]byte, error) {
	prototypeType := reflect.TypeOf(prototype)
	if prototypeType == nil {
		return nil, errors.New("schema prototype must not be nil")
	}
	prototypeType = dereference(prototypeType)
	if prototypeType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("schema prototype must be a struct, not %s", prototypeType.Kind())
	}

	root, err := schemaOf(prototypeType, map[reflect.Type]bool{})
	if err != nil {
		return nil, err
	}
	properties, ok := root["properties"].(map[string]any)
	if !ok {
		// A struct with a special-case schema, such as time.Time, is
		// not a configuration document shape.
		return nil, fmt.Errorf("schema prototype %s does not describe a JSON object", prototypeType)
	}
	root["$schema"] = schemaVersion
	if prototypeType.Name() != "" {
		root["title"] = prototypeType.Name()
	}
	// The $schema key inside a document is how editors associate this
	// schema; without this property the association itself would violate
	// additionalProperties. A uri-reference admits the relative path the
	// association conventionally uses.
	properties["$schema"] = map[string]any{
		"type":   "string",
		"format": "uri-reference",
	}

	document, err := json.MarshalIndent(root, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("encode schema: %w", err)
	}
	return append(document, '\n'), nil
}

// schemaOf builds the schema of one type. The visiting set holds the struct
// types on the current descent, so a recursive type is reported instead of
// expanding forever.
func schemaOf(valueType reflect.Type, visiting map[reflect.Type]bool) (map[string]any, error) {
	valueType = dereference(valueType)

	switch valueType {
	case durationType:
		return map[string]any{
			"type":        []any{"string", "integer"},
			"pattern":     durationPattern,
			"description": "Go duration string, such as \"30s\" or \"1h30m\", or an integer nanosecond count.",
		}, nil
	case reflect.TypeFor[time.Time]():
		return map[string]any{"type": "string", "format": "date-time"}, nil
	}
	if valueType.Kind() != reflect.Struct && reflect.PointerTo(valueType).Implements(textUnmarshalerType) {
		return map[string]any{"type": "string"}, nil
	}

	switch valueType.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Slice, reflect.Array:
		items, err := schemaOf(valueType.Elem(), visiting)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if valueType.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map keys must be strings for a schema, not %s", valueType.Key())
		}
		values, err := schemaOf(valueType.Elem(), visiting)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": values}, nil
	case reflect.Interface:
		if valueType.NumMethod() == 0 {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("cannot derive a schema for interface %s", valueType)
	case reflect.Struct:
		return structSchema(valueType, visiting)
	default:
		return nil, fmt.Errorf("cannot derive a schema for %s", valueType)
	}
}

// structSchema builds the object schema of a struct: one property per
// bindable field, required fields without omitempty, and no additional
// properties, matching the strict binding of Load.
func structSchema(structType reflect.Type, visiting map[reflect.Type]bool) (map[string]any, error) {
	if visiting[structType] {
		return nil, fmt.Errorf("recursive type %s cannot be described by a schema", structType)
	}
	visiting[structType] = true
	defer delete(visiting, structType)

	properties := map[string]any{}
	var required []string
	for name, field := range structFields(structType) {
		property, err := schemaOf(field.Type, visiting)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}
		if err := applyFieldTags(property, field); err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}
		properties[name] = property
		if !omittable(field) {
			required = append(required, name)
		}
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		slices.Sort(required)
		schema["required"] = required
	}
	return schema, nil
}

// omittable reports whether the json tag marks the field optional.
func omittable(field reflect.StructField) bool {
	_, options, _ := strings.Cut(field.Tag.Get("json"), ",")
	for option := range strings.SplitSeq(options, ",") {
		if option == "omitempty" || option == "omitzero" {
			return true
		}
	}
	return false
}

// applyFieldTags overlays the jsonschema and jsonschema_description tags of
// a field onto its property schema.
func applyFieldTags(property map[string]any, field reflect.StructField) error {
	for item := range splitTagItems(field.Tag.Get("jsonschema")) {
		key, value, hasValue := strings.Cut(item, "=")
		if !hasValue {
			return fmt.Errorf("jsonschema tag item %q must have the form key=value", item)
		}
		if err := applyTagItem(property, key, value, field.Type); err != nil {
			return err
		}
	}
	if description, ok := field.Tag.Lookup("jsonschema_description"); ok {
		property["description"] = description
	}
	return nil
}

// applyTagItem records one key=value constraint. Values of default, enum,
// and example are coerced to the JSON type of the field, so a numeric field
// declares numeric constants. On an array field, enum and example constrain
// the elements: they are coerced to the element type and attached to the
// items schema, where a whole-array constant would make every instance
// invalid.
func applyTagItem(property map[string]any, key, value string, fieldType reflect.Type) error {
	elements := dereference(fieldType).Kind() == reflect.Slice || dereference(fieldType).Kind() == reflect.Array
	switch key {
	case "title", "description", "pattern", "format":
		property[key] = value
		return nil
	case "default":
		if elements {
			return fmt.Errorf("default %q: a default is not supported on an array field", value)
		}
		coerced, err := coerceTagValue(value, fieldType)
		if err != nil {
			return fmt.Errorf("default %q: %w", value, err)
		}
		property[key] = coerced
		return nil
	case "enum", "example":
		coerced, err := coerceTagValue(value, constraintType(fieldType))
		if err != nil {
			return fmt.Errorf("%s %q: %w", key, value, err)
		}
		plural := key
		if key == "example" {
			plural = "examples"
		}
		target := property
		if elements {
			items, ok := property["items"].(map[string]any)
			if !ok {
				return fmt.Errorf("%s %q: the array field carries no items schema", key, value)
			}
			target = items
		}
		values, _ := target[plural].([]any)
		target[plural] = append(values, coerced)
		return nil
	case "minimum", "maximum":
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("%s %q must be a number", key, value)
		}
		property[key] = number
		return nil
	case "minLength", "maxLength":
		length, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s %q must be an integer", key, value)
		}
		property[key] = length
		return nil
	default:
		return fmt.Errorf("jsonschema tag key %q is not supported", key)
	}
}

// constraintType is the type an enum or example value constrains: the
// element type for arrays and slices, the field type itself otherwise.
func constraintType(fieldType reflect.Type) reflect.Type {
	fieldType = dereference(fieldType)
	if fieldType.Kind() == reflect.Slice || fieldType.Kind() == reflect.Array {
		return fieldType.Elem()
	}
	return fieldType
}

// coerceTagValue converts a tag value to the JSON type the field carries, so
// enum=8080 on an integer field becomes the number 8080 while the same tag
// on a string field stays text.
func coerceTagValue(value string, fieldType reflect.Type) (any, error) {
	fieldType = dereference(fieldType)
	if fieldType == durationType {
		return value, nil
	}
	switch fieldType.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, errors.New("value must be an integer")
		}
		return parsed, nil
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, errors.New("value must be a number")
		}
		return parsed, nil
	case reflect.Bool:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return nil, errors.New("value must be a boolean")
		}
		return parsed, nil
	default:
		return value, nil
	}
}

// splitTagItems yields the comma-separated items of a jsonschema tag,
// honoring a backslash escape so a pattern or description may contain a
// literal comma.
func splitTagItems(tag string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		if tag == "" {
			return
		}
		var builder strings.Builder
		for index := 0; index < len(tag); index++ {
			switch {
			case tag[index] == escapeCharacter && index+1 < len(tag) && tag[index+1] == ',':
				builder.WriteByte(',')
				index++
			case tag[index] == ',':
				if !yield(builder.String()) {
					return
				}
				builder.Reset()
			default:
				builder.WriteByte(tag[index])
			}
		}
		yield(builder.String())
	}
}
