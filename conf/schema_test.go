package conf

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

type documentedConfiguration struct {
	Host    string        `json:"host"`
	Port    int           `json:"port"`
	Mode    string        `json:"mode,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
	Notes   string        `json:"notes,omitempty"`
}

// documentedDefinition is the schema definition that accompanies
// documentedConfiguration: the struct stores the values, the definition
// defines the schema.
func documentedDefinition() SchemaDefinition {
	return SchemaDefinition{
		Description: "Demonstration configuration.",
		Fields: map[string]FieldDefinition{
			"host":  {Description: "Interface to bind", Default: "localhost"},
			"port":  {Minimum: Pointer(1.0), Maximum: Pointer(65535.0), Default: 8080},
			"mode":  {Enum: []any{"development", "production"}},
			"notes": {Description: "Free text, commas included."},
		},
	}
}

func generate(t *testing.T, prototype any, definitions ...SchemaDefinition) map[string]any {
	t.Helper()
	document, err := GenerateSchema(prototype, definitions...)
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
	// The library renders the prohibition of additional properties as the
	// false schema {"not": {}}; either form makes editors flag unknown keys
	// the way Load does.
	forbidden, ok := schema["additionalProperties"].(map[string]any)
	if schema["additionalProperties"] != false && (!ok || len(forbidden) != 1 || forbidden["not"] == nil) {
		t.Errorf("additionalProperties = %v, want the false schema so unknown keys are flagged by editors as they are by Load", schema["additionalProperties"])
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
	slices.Sort(names)
	if !slices.Equal(names, []string{"host", "port"}) {
		t.Errorf("required = %q, want exactly the fields without omitempty", names)
	}
}

func TestTheSchemaKeyIsAllowedAtTheRoot(t *testing.T) {
	schema := generate(t, documentedConfiguration{})

	patterns, ok := schema["patternProperties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no patternProperties: %v", schema)
	}
	association, ok := patterns[`^\$schema$`].(map[string]any)
	if !ok {
		t.Fatal("the $schema key is not admitted, so a document carrying the association would fail validation")
	}
	if association["type"] != "string" {
		t.Errorf("$schema type = %v, want string", association["type"])
	}
	if association["format"] != "uri-reference" {
		t.Errorf("$schema format = %v, want uri-reference so a relative path such as ./application.schema.json validates", association["format"])
	}

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties: %v", schema)
	}
	if _, present := properties["$schema"]; present {
		t.Error("$schema is declared under properties, where editors misreport the object-form declaration as a type error")
	}
}

func TestGenerateSchemaRejectsANonObjectPrototype(t *testing.T) {
	if _, err := GenerateSchema(time.Time{}); err == nil {
		t.Fatal("a struct with a non-object special-case schema was accepted")
	}
}

func TestDefinitionsContributeDescriptionsAndConstraints(t *testing.T) {
	schema := generate(t, documentedConfiguration{}, documentedDefinition())

	if schema["description"] != "Demonstration configuration." {
		t.Errorf("root description = %v, want the definition's root description", schema["description"])
	}

	host := property(t, schema, "host")
	if host["description"] != "Interface to bind" || host["default"] != "localhost" {
		t.Errorf("host schema = %v, want the defined description and default", host)
	}

	port := property(t, schema, "port")
	if port["minimum"] != float64(1) || port["maximum"] != float64(65535) || port["default"] != float64(8080) {
		t.Errorf("port schema = %v, want numeric bounds and default", port)
	}

	mode := property(t, schema, "mode")
	enum, ok := mode["enum"].([]any)
	if !ok || len(enum) != 2 || enum[0] != "development" || enum[1] != "production" {
		t.Errorf("mode enum = %v, want the two defined values", mode["enum"])
	}

	notes := property(t, schema, "notes")
	if notes["description"] != "Free text, commas included." {
		t.Errorf("notes description = %v, want free text unconstrained by any tag grammar", notes["description"])
	}
}

func TestEnumOnAnArrayFieldConstrainsTheElements(t *testing.T) {
	type routed struct {
		Tags []string `json:"tags"`
	}
	schema := generate(t, routed{}, SchemaDefinition{
		Fields: map[string]FieldDefinition{
			"tags": {Enum: []any{"orders", "billing"}},
		},
	})

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

func TestANestedFieldIsDefinedByItsDottedPath(t *testing.T) {
	type listener struct {
		Address string `json:"address"`
	}
	type application struct {
		Server listener `json:"server"`
	}
	schema := generate(t, application{}, SchemaDefinition{
		Fields: map[string]FieldDefinition{
			"server":         {Description: "Listener parameters."},
			"server.address": {Description: "TCP address, in host:port form.", Default: ":8080"},
		},
	})

	server := property(t, schema, "server")
	if server["description"] != "Listener parameters." {
		t.Errorf("server description = %v, want the defined description", server["description"])
	}
	properties, ok := server["properties"].(map[string]any)
	if !ok {
		t.Fatalf("server schema has no properties: %v", server)
	}
	address, ok := properties["address"].(map[string]any)
	if !ok {
		t.Fatalf("server schema has no address property: %v", properties)
	}
	if address["description"] != "TCP address, in host:port form." || address["default"] != ":8080" {
		t.Errorf("address schema = %v, want the defined description and default", address)
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

func TestAPatternKeepsItsCommasWithoutEscaping(t *testing.T) {
	type coded struct {
		Code string `json:"code"`
	}
	schema := generate(t, coded{}, SchemaDefinition{
		Fields: map[string]FieldDefinition{
			"code": {Pattern: `^[a-z]{2,4}$`},
		},
	})

	if got := property(t, schema, "code")["pattern"]; got != "^[a-z]{2,4}$" {
		t.Errorf("pattern = %v, want the literal pattern, commas included", got)
	}
}

func TestASchemaMetadataTagIsRejected(t *testing.T) {
	type tagged struct {
		Host string `json:"host" jsonschema:"the interface to bind"`
	}
	if _, err := GenerateSchema(tagged{}); err == nil {
		t.Error("a jsonschema tag was accepted; schema metadata lives in a SchemaDefinition")
	}

	type described struct {
		Host string `json:"host" desc:"the interface to bind"`
	}
	if _, err := GenerateSchema(described{}); err == nil {
		t.Error("a desc tag was accepted; schema metadata lives in a SchemaDefinition")
	}
}

func TestAnUnknownDefinitionPathIsRejected(t *testing.T) {
	definition := SchemaDefinition{
		Fields: map[string]FieldDefinition{
			"prot": {Description: "misspelled"},
		},
	}
	if _, err := GenerateSchema(documentedConfiguration{}, definition); err == nil {
		t.Fatal("a definition path designating no declared field was accepted")
	}
}

func TestAMistypedDefaultIsRejected(t *testing.T) {
	definition := SchemaDefinition{
		Fields: map[string]FieldDefinition{
			"port": {Default: "eighty"},
		},
	}
	if _, err := GenerateSchema(documentedConfiguration{}, definition); err == nil {
		t.Fatal("a default that does not satisfy the schema of its field was accepted")
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
	first, err := GenerateSchema(documentedConfiguration{}, documentedDefinition())
	if err != nil {
		t.Fatalf("generate schema: %v", err)
	}
	second, err := GenerateSchema(documentedConfiguration{}, documentedDefinition())
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
