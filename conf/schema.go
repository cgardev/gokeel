package conf

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaVersion is the canonical draft the generated schema declares.
const schemaVersion = "https://json-schema.org/draft/2020-12/schema"

// durationPattern validates the Go duration notation time.ParseDuration
// reads, such as "30s" or "1h30m". The JSON Schema built-in duration format
// means an ISO 8601 duration, which is a different notation, so a pattern is
// used instead.
const durationPattern = `^-?(0|(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+)$`

// SchemaDefinition documents a schema apart from the struct the documents
// bind onto: the struct declares the shape and carries nothing but its json
// tags, while the definition carries the prose and the constraints. Keeping
// the two separate is deliberate — one structure stores the values, another
// defines the schema — so descriptions of any length never crowd a struct
// tag, and fields of imported types can be documented without tagging them.
type SchemaDefinition struct {
	// Description documents the document root.
	Description string

	// Fields documents and constrains declared fields, keyed by the dotted
	// document path of each field, such as "server.address". A path that
	// designates no declared field fails the generation, so a misspelled
	// path is caught immediately instead of silently annotating nothing.
	Fields map[string]FieldDefinition
}

// FieldDefinition carries the schema attributes of one field. A zero value
// leaves the corresponding keyword unset, so a later definition overlaid by
// GenerateSchema refines exactly the attributes it fills.
type FieldDefinition struct {
	// Title and Description annotate the field for editors.
	Title       string
	Description string

	// Default documents the value the application uses when the document
	// omits the field. It is validated against the field's schema, so a
	// default of the wrong type fails the generation.
	Default any

	// Enum constrains the permitted values. On an array field the values
	// constrain the elements, where a whole-array constant would make every
	// instance invalid.
	Enum []any

	// Examples suggests valid values to editors. On an array field the
	// values exemplify the elements, as Enum does.
	Examples []any

	// Pattern constrains a string field with a regular expression, and
	// Format declares a semantic format such as "uri-reference".
	Pattern string
	Format  string

	// Minimum and Maximum bound a numeric field, and MinLength and
	// MaxLength bound a string field; Pointer fills them in a literal.
	Minimum   *float64
	Maximum   *float64
	MinLength *int
	MaxLength *int
}

// Pointer returns a pointer to value: the shorthand for filling the optional
// bounds of a FieldDefinition inside a composite literal.
func Pointer[T any](value T) *T {
	return &value
}

// GenerateSchema derives a JSON Schema draft 2020-12 document from the
// struct the configuration binds onto, through the Google JSON Schema
// implementation github.com/google/jsonschema-go: the counterpart of the
// configuration metadata Spring Boot generates from @ConfigurationProperties
// classes for editor completion. Store the output next to the configuration
// documents and point their $schema key at it, so editors and code
// assistants offer completion, type checking, and the descriptions of every
// key.
//
// Field names follow the json tags, and the json tag is the only tag the
// struct carries: descriptions, defaults, and constraints live in the
// SchemaDefinition values, overlaid in argument order, so the structure that
// stores the values stays separate from the definition of its schema. A
// schema-metadata struct tag is rejected, and so is a definition path that
// designates no declared field.
//
// A field is required unless its json tag carries omitempty or omitzero.
// Objects reject unknown properties, matching the strict binding of Load;
// the root additionally admits the $schema key itself, so a document
// carrying the association validates cleanly. A time.Duration field accepts
// the Go duration notation or an integer nanosecond count, a time.Time field
// is a date-time string, and any other non-struct encoding.TextUnmarshaler
// type is a string, exactly as the binding reads them. Before the schema is
// returned it is resolved, which compiles every constraint and validates
// every documented default against the schema of its field.
func GenerateSchema(prototype any, definitions ...SchemaDefinition) ([]byte, error) {
	prototypeType := reflect.TypeOf(prototype)
	if prototypeType == nil {
		return nil, errors.New("schema prototype must not be nil")
	}
	prototypeType = dereference(prototypeType)
	if prototypeType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("schema prototype must be a struct, not %s", prototypeType.Kind())
	}

	overrides := map[reflect.Type]*jsonschema.Schema{}
	if err := analyzeType(prototypeType, map[reflect.Type]bool{}, overrides); err != nil {
		return nil, err
	}
	overrides[durationType] = &jsonschema.Schema{
		Types:       []string{"string", "integer"},
		Pattern:     durationPattern,
		Description: "Go duration string, such as \"30s\" or \"1h30m\", or an integer nanosecond count.",
	}
	overrides[reflect.TypeFor[time.Time]()] = &jsonschema.Schema{Type: "string", Format: "date-time"}

	root, err := jsonschema.ForType(prototypeType, &jsonschema.ForOptions{TypeSchemas: overrides})
	if err != nil {
		return nil, fmt.Errorf("derive schema of %s: %w", prototypeType, err)
	}
	if root.Type != "object" || root.Properties == nil {
		// A struct with a special-case schema, such as time.Time, is
		// not a configuration document shape.
		return nil, fmt.Errorf("schema prototype %s does not describe a JSON object", prototypeType)
	}

	root.Schema = schemaVersion
	if prototypeType.Name() != "" {
		root.Title = prototypeType.Name()
	}
	// The $schema key inside a document is how editors associate this
	// schema; without admitting it the association itself would violate the
	// prohibition of additional properties. It is declared under
	// patternProperties rather than properties because some editors
	// special-case a property literally named $schema and misreport its
	// object-form declaration as a type error. A uri-reference admits the
	// relative path the association conventionally uses.
	root.PatternProperties = map[string]*jsonschema.Schema{
		`^\$schema$`: {Type: "string", Format: "uri-reference"},
	}

	for _, definition := range definitions {
		if err := applyDefinition(root, definition); err != nil {
			return nil, err
		}
	}

	// Resolution hands the whole document to the library, which compiles
	// every constraint and validates every documented default against the
	// schema of its field.
	if _, err := root.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true}); err != nil {
		return nil, fmt.Errorf("validate generated schema: %w", err)
	}

	document, err := json.MarshalIndent(root, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("encode schema: %w", err)
	}
	return append(document, '\n'), nil
}

// analyzeType walks the prototype ahead of the library derivation. It
// enforces that no field smuggles schema metadata through a struct tag, and
// it records every non-struct encoding.TextUnmarshaler type as a string
// schema, matching the relaxed binding, which reads such values from JSON
// strings.
func analyzeType(valueType reflect.Type, visited map[reflect.Type]bool, overrides map[reflect.Type]*jsonschema.Schema) error {
	valueType = dereference(valueType)
	if visited[valueType] {
		return nil
	}
	visited[valueType] = true

	if valueType.Kind() != reflect.Struct && reflect.PointerTo(valueType).Implements(textUnmarshalerType) {
		overrides[valueType] = &jsonschema.Schema{Type: "string"}
		return nil
	}

	switch valueType.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return analyzeType(valueType.Elem(), visited, overrides)
	case reflect.Struct:
		fields := structFields(valueType)
		for _, name := range slices.Sorted(maps.Keys(fields)) {
			field := fields[name]
			for _, tag := range []string{"jsonschema", "desc"} {
				if _, present := field.Tag.Lookup(tag); present {
					return fmt.Errorf(
						"field %s.%s declares a %s tag: schema metadata lives in a SchemaDefinition, not in struct tags",
						valueType, field.Name, tag)
				}
			}
			if err := analyzeType(field.Type, visited, overrides); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyDefinition overlays one definition onto the derived schema, resolving
// each dotted path in lexicographic order so a failure is deterministic.
func applyDefinition(root *jsonschema.Schema, definition SchemaDefinition) error {
	if definition.Description != "" {
		root.Description = definition.Description
	}
	for _, path := range slices.Sorted(maps.Keys(definition.Fields)) {
		node, err := fieldNode(root, path)
		if err != nil {
			return err
		}
		if err := applyFieldDefinition(node, definition.Fields[path], path); err != nil {
			return err
		}
	}
	return nil
}

// fieldNode resolves a dotted document path to the schema of the declared
// field it designates, walking object properties only: a path into a map
// value or an array element designates no declared field and is rejected.
func fieldNode(root *jsonschema.Schema, path string) (*jsonschema.Schema, error) {
	node := root
	for segment := range splitPath(path) {
		child, ok := node.Properties[segment]
		if !ok {
			return nil, fmt.Errorf("schema definition path %q does not designate a declared field", path)
		}
		node = child
	}
	return node, nil
}

// applyFieldDefinition writes the filled attributes of a definition onto the
// schema of its field. Enum and Examples on an array field land on the items
// schema, where they constrain the elements.
func applyFieldDefinition(node *jsonschema.Schema, field FieldDefinition, path string) error {
	if field.Title != "" {
		node.Title = field.Title
	}
	if field.Description != "" {
		node.Description = field.Description
	}
	if field.Pattern != "" {
		node.Pattern = field.Pattern
	}
	if field.Format != "" {
		node.Format = field.Format
	}
	if field.Minimum != nil {
		node.Minimum = field.Minimum
	}
	if field.Maximum != nil {
		node.Maximum = field.Maximum
	}
	if field.MinLength != nil {
		node.MinLength = field.MinLength
	}
	if field.MaxLength != nil {
		node.MaxLength = field.MaxLength
	}
	if field.Default != nil {
		encoded, err := json.Marshal(field.Default)
		if err != nil {
			return fmt.Errorf("schema definition path %q: encode default: %w", path, err)
		}
		node.Default = encoded
	}

	// The library types a Go slice as ["null", "array"], because a nil
	// slice marshals to JSON null; a fixed-size array is plain "array".
	target := node
	if (node.Type == "array" || slices.Contains(node.Types, "array")) && node.Items != nil {
		target = node.Items
	}
	if len(field.Enum) != 0 {
		target.Enum = slices.Clone(field.Enum)
	}
	if len(field.Examples) != 0 {
		target.Examples = slices.Clone(field.Examples)
	}
	return nil
}
