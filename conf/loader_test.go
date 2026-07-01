package conf

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

type serverConfiguration struct {
	Host    string        `json:"host"`
	Port    int           `json:"port"`
	Verbose bool          `json:"verbose,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

type applicationConfiguration struct {
	Name   string              `json:"name"`
	Server serverConfiguration `json:"server"`
	Tags   []string            `json:"tags,omitempty"`
}

// emptyLookup resolves nothing, so tests that need no environment are
// hermetic regardless of the variables the test process inherits.
func emptyLookup(string) (string, bool) { return "", false }

// mapLookup builds a lookup over a fixed set of variables.
func mapLookup(variables map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := variables[name]
		return value, ok
	}
}

func TestLoadBindsADocumentOntoTheTargetStruct(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{
			"name": "shop",
			"server": {"host": "localhost", "port": 8080, "verbose": true, "timeout": "1m30s"},
			"tags": ["orders", "billing"]
		}`)),
		WithLookup(emptyLookup),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}

	if target.Name != "shop" || target.Server.Host != "localhost" || target.Server.Port != 8080 {
		t.Errorf("bound configuration = %+v, want the document values", target)
	}
	if !target.Server.Verbose || target.Server.Timeout != 90*time.Second {
		t.Errorf("bound flags = %+v, want verbose true and a 90s timeout", target.Server)
	}
	if len(target.Tags) != 2 || target.Tags[0] != "orders" {
		t.Errorf("bound tags = %q, want the document array", target.Tags)
	}
}

func TestALaterSourceOverridesAnEarlierOneDeeply(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{"name": "shop", "server": {"host": "localhost", "port": 8080}}`)),
		WithDocument([]byte(`{"server": {"port": 9090}}`)),
		WithLookup(emptyLookup),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}

	if target.Server.Port != 9090 {
		t.Errorf("port = %d, want the later document's 9090", target.Server.Port)
	}
	if target.Server.Host != "localhost" {
		t.Errorf("host = %q, want the earlier document's value to survive the deep merge", target.Server.Host)
	}
}

func TestNamesAbsentFromTheDocumentsKeepTheTargetDefaults(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{"name": "shop"}`)),
		WithLookup(emptyLookup),
	)

	target := applicationConfiguration{Server: serverConfiguration{Host: "fallback", Port: 3000}}
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}

	if target.Server.Host != "fallback" || target.Server.Port != 3000 {
		t.Errorf("server = %+v, want the code-level defaults untouched", target.Server)
	}
}

func TestAMissingRequiredFileFailsTheLoad(t *testing.T) {
	loader := NewLoader(WithFile(filepath.Join(t.TempDir(), "absent.json")), WithLookup(emptyLookup))

	var target applicationConfiguration
	if err := loader.Load(&target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("load error = %v, want fs.ErrNotExist", err)
	}
}

func TestAMissingOptionalFileIsSkipped(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{"name": "shop", "server": {"host": "localhost", "port": 1}}`)),
		WithOptionalFile(filepath.Join(t.TempDir(), "absent.json")),
		WithLookup(emptyLookup),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Name != "shop" {
		t.Errorf("name = %q, want the inline document to have been applied", target.Name)
	}
}

func TestAPresentFileOverridesTheEmbeddedDefaults(t *testing.T) {
	embedded := fstest.MapFS{
		"defaults.json": &fstest.MapFile{Data: []byte(`{"name": "shop", "server": {"host": "localhost", "port": 8080}}`)},
	}
	external := filepath.Join(t.TempDir(), "application.json")
	if err := os.WriteFile(external, []byte(`{"server": {"port": 9090}}`), 0o600); err != nil {
		t.Fatalf("write external file: %v", err)
	}

	loader := NewLoader(
		WithFilesystemFile(embedded, "defaults.json"),
		WithFile(external),
		WithLookup(emptyLookup),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Server.Port != 9090 || target.Server.Host != "localhost" {
		t.Errorf("server = %+v, want the external file layered over the embedded defaults", target.Server)
	}
}

func TestTheSchemaKeyOfADocumentIsTolerated(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{"$schema": "./application.schema.json", "name": "shop", "server": {"host": "h", "port": 1}}`)),
		WithLookup(emptyLookup),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}
}

func TestLoadRejectsATargetThatIsNotAStructPointer(t *testing.T) {
	loader := NewLoader(WithDocument([]byte(`{}`)), WithLookup(emptyLookup))

	if err := loader.Load(applicationConfiguration{}); err == nil {
		t.Error("a non-pointer target was accepted")
	}
	var number int
	if err := loader.Load(&number); err == nil {
		t.Error("a pointer to a non-struct was accepted")
	}
}

func TestADocumentMustHoldExactlyOneJSONObject(t *testing.T) {
	var target applicationConfiguration

	twoValues := NewLoader(WithDocument([]byte(`{"name": "a"} {"name": "b"}`)), WithLookup(emptyLookup))
	if err := twoValues.Load(&target); err == nil {
		t.Error("a document with a second JSON value was accepted")
	}

	array := NewLoader(WithDocument([]byte(`[1, 2]`)), WithLookup(emptyLookup))
	if err := array.Load(&target); err == nil {
		t.Error("a document whose root is not an object was accepted")
	}
}

func TestWithDocumentCopiesTheCallerBytes(t *testing.T) {
	document := []byte(`{"name": "shop"}`)
	loader := NewLoader(WithDocument(document), WithLookup(emptyLookup))
	document[2] = 'X'

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load after mutating the caller slice: %v", err)
	}
	if target.Name != "shop" {
		t.Errorf("name = %q, want the value captured before the mutation", target.Name)
	}
}

func TestEnvironmentNameAppliesTheRelaxedMapping(t *testing.T) {
	names := []struct {
		property    string
		environment string
	}{
		{"demo.item-price", "DEMO_ITEMPRICE"},
		{"server.port", "SERVER_PORT"},
		{"NAME", "NAME"},
	}
	for _, entry := range names {
		if got := environmentName(entry.property); got != entry.environment {
			t.Errorf("environmentName(%q) = %q, want %q", entry.property, got, entry.environment)
		}
	}
}
