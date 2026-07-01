package configuration

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

type documentedConfiguration struct {
	Host    string        `json:"host" jsonschema:"description=Interface to bind,default=localhost"`
	Port    int           `json:"port" jsonschema:"minimum=1,maximum=65535,default=8080"`
	Mode    string        `json:"mode,omitempty" jsonschema:"enum=development,enum=production"`
	Timeout time.Duration `json:"timeout,omitempty"`
	Notes   string        `json:"notes,omitempty" jsonschema_description:"Free text, commas included."`
}

func generate(t *testing.T, prototype any) map[string]any {
	t.Helper()
	document, err := GenerateSchema(prototype)
	if err != nil {
		t.Fatalf("generate schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(document, &schema); err != nil {
		t.Fatalf("generated schema is not valid JSON: %v", err)
	}
	return schema
}

func property(t *testing.T, schema map[string]any, name string) map[string]any {
	t.Helper()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties object: %v", schema)
	}
	value, ok := properties[name].(map[string]any)
	if !ok {
		t.Fatalf("schema has no property %q", name)
	}
	return value
}

func TestGenerateSchemaDescribesTheStruct(t *testing.T) {
	schema := generate(t, documentedConfiguration{})

	if schema["$schema"] != schemaVersion {
		t.Errorf("$schema = %v, want the draft 2020-12 URL", schema["$schema"])
	}
	if schema["title"] != "documentedConfiguration" {
		t.Errorf("title = %v, want the struct name", schema["title"])
	}
	if schema["additionalProperties"] != false {
		t.Error("additionalProperties is not false: unknown keys must be flagged by editors as they are by Load")
	}
	if got := property(t, schema, "host")["type"]; got != "string" {
		t.Errorf("host type = %v, want string", got)
	}
	if got := property(t, schema, "port")["type"]; got != "integer" {
		t.Errorf("port type = %v, want integer", got)
	}
}

func TestRequiredListsFieldsWithoutOmitempty(t *testing.T) {
	schema := generate(t, documentedConfiguration{})

	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("schema has no required list: %v", schema)
	}
	names := make([]string, 0, len(required))
	for _, name := range required {
		names = append(names, name.(string))
	}
	if !slices.Equal(names, []string{"host", "port"}) {
		t.Errorf("required = %q, want exactly the fields without omitempty, sorted", names)
	}
}

func TestTheSchemaKeyIsAllowedAtTheRoot(t *testing.T) {
	schema := generate(t, documentedConfiguration{})

	association := property(t, schema, "$schema")
	if association["type"] != "string" {
		t.Error("the $schema property is not declared, so a document carrying the association would fail validation")
	}
	if association["format"] != "uri-reference" {
		t.Errorf("$schema format = %v, want uri-reference so a relative path such as ./application.schema.json validates", association["format"])
	}
}

func TestGenerateSchemaRejectsANonObjectPrototype(t *testing.T) {
	if _, err := GenerateSchema(time.Time{}); err == nil {
		t.Fatal("a struct with a non-object special-case schema was accepted")
	}
}

func TestEnumOnAnArrayFieldConstrainsTheElements(t *testing.T) {
	type tagged struct {
		Tags []string `json:"tags" jsonschema:"enum=orders,enum=billing"`
	}
	schema := generate(t, tagged{})

	tags := property(t, schema, "tags")
	if _, present := tags["enum"]; present {
		t.Error("enum sits on the array schema, where no instance could ever satisfy it")
	}
	items, ok := tags["items"].(map[string]any)
	if !ok {
		t.Fatalf("tags schema has no items object: %v", tags)
	}
	enum, ok := items["enum"].([]any)
	if !ok || len(enum) != 2 || enum[0] != "orders" || enum[1] != "billing" {
		t.Errorf("items enum = %v, want the element constraints", items["enum"])
	}
}

func TestTagsContributeConstraintsCoercedToTheFieldType(t *testing.T) {
	schema := generate(t, documentedConfiguration{})

	host := property(t, schema, "host")
	if host["description"] != "Interface to bind" || host["default"] != "localhost" {
		t.Errorf("host schema = %v, want the tag description and default", host)
	}

	port := property(t, schema, "port")
	if port["minimum"] != float64(1) || port["maximum"] != float64(65535) || port["default"] != float64(8080) {
		t.Errorf("port schema = %v, want numeric bounds and default", port)
	}

	mode := property(t, schema, "mode")
	enum, ok := mode["enum"].([]any)
	if !ok || len(enum) != 2 || enum[0] != "development" || enum[1] != "production" {
		t.Errorf("mode enum = %v, want the two repeated enum values", mode["enum"])
	}

	notes := property(t, schema, "notes")
	if notes["description"] != "Free text, commas included." {
		t.Errorf("notes description = %v, want the comma-safe whole-value tag", notes["description"])
	}
}

func TestDurationsAreStringOrIntegerWithAPattern(t *testing.T) {
	schema := generate(t, documentedConfiguration{})

	timeout := property(t, schema, "timeout")
	kinds, ok := timeout["type"].([]any)
	if !ok || len(kinds) != 2 || kinds[0] != "string" || kinds[1] != "integer" {
		t.Errorf("timeout type = %v, want the string-or-integer pair", timeout["type"])
	}
	if timeout["pattern"] != durationPattern {
		t.Errorf("timeout pattern = %v, want the Go duration pattern", timeout["pattern"])
	}
}

func TestAnEscapedCommaStaysInsideATagValue(t *testing.T) {
	type escaped struct {
		Code string `json:"code" jsonschema:"pattern=^[a-z]{2\\,4}$"`
	}
	schema := generate(t, escaped{})

	if got := property(t, schema, "code")["pattern"]; got != "^[a-z]{2,4}$" {
		t.Errorf("pattern = %v, want the escaped comma preserved", got)
	}
}

func TestAnUnsupportedTagKeyIsRejected(t *testing.T) {
	type misspelled struct {
		Host string `json:"host" jsonschema:"descriptoin=oops"`
	}
	if _, err := GenerateSchema(misspelled{}); err == nil {
		t.Fatal("a misspelled jsonschema tag key was accepted")
	}
}

func TestARecursiveTypeIsRejected(t *testing.T) {
	type node struct {
		Children []*node `json:"children,omitempty"`
	}
	if _, err := GenerateSchema(node{}); err == nil {
		t.Fatal("a recursive type was accepted")
	}
}

func TestGeneratedSchemasAreDeterministic(t *testing.T) {
	first, err := GenerateSchema(documentedConfiguration{})
	if err != nil {
		t.Fatalf("generate schema: %v", err)
	}
	second, err := GenerateSchema(documentedConfiguration{})
	if err != nil {
		t.Fatalf("generate schema: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two generations of the same schema differ")
	}
}

func TestAGeneratedSchemaRoundTripsWithLoad(t *testing.T) {
	document := []byte(`{
		"$schema": "./application.schema.json",
		"host": "shop.internal",
		"port": 9090
	}`)

	var target documentedConfiguration
	loader := NewLoader(WithDocument(document), WithLookup(emptyLookup))
	if err := loader.Load(&target); err != nil {
		t.Fatalf("a document shaped by the generated schema fails to load: %v", err)
	}
	if target.Host != "shop.internal" || target.Port != 9090 {
		t.Errorf("bound configuration = %+v, want the document values", target)
	}
}
