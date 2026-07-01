package configuration

import (
	"errors"
	"strings"
	"testing"
)

func TestAPlaceholderReadsAnEnvironmentVariable(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{"name": "shop", "server": {"host": "${SHOP_HOST}", "port": 1}}`)),
		WithLookup(mapLookup(map[string]string{"SHOP_HOST": "shop.internal"})),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Server.Host != "shop.internal" {
		t.Errorf("host = %q, want the environment value", target.Server.Host)
	}
}

func TestADefaultValueAppliesWhenTheVariableIsUnset(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{"name": "${SHOP_NAME:shop}", "server": {"host": "h", "port": "${SHOP_PORT:8080}"}}`)),
		WithLookup(emptyLookup),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Name != "shop" {
		t.Errorf("name = %q, want the default after the colon", target.Name)
	}
	if target.Server.Port != 8080 {
		t.Errorf("port = %d, want the default converted to the field type", target.Server.Port)
	}
}

func TestAnEmptyDefaultIsAllowed(t *testing.T) {
	resolved, err := resolveText("${UNSET:}", emptyLookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "" {
		t.Errorf("resolved = %q, want the empty default", resolved)
	}
}

func TestAnUnresolvedPlaceholderFailsTheLoad(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{"name": "${SHOP_NAME}", "server": {"host": "h", "port": 1}}`)),
		WithLookup(emptyLookup),
	)

	var target applicationConfiguration
	err := loader.Load(&target)
	if !errors.Is(err, ErrUnresolvedPlaceholder) {
		t.Fatalf("load error = %v, want ErrUnresolvedPlaceholder", err)
	}
	if !strings.Contains(err.Error(), "SHOP_NAME") {
		t.Errorf("error %q does not name the unresolved placeholder", err)
	}
}

func TestAnEscapedPlaceholderStaysLiteral(t *testing.T) {
	resolved, err := resolveText(`literal \${name} kept`, emptyLookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "literal ${name} kept" {
		t.Errorf("resolved = %q, want the literal placeholder with the escape removed", resolved)
	}
}

func TestPlaceholdersComposeInsideALargerValue(t *testing.T) {
	lookup := mapLookup(map[string]string{"HOST": "db.internal", "PORT": "5432"})
	resolved, err := resolveText("postgres://${HOST}:${PORT}/shop", lookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "postgres://db.internal:5432/shop" {
		t.Errorf("resolved = %q, want both placeholders substituted in place", resolved)
	}
}

func TestPlaceholdersNestInKeysAndDefaults(t *testing.T) {
	lookup := mapLookup(map[string]string{
		"STAGE":           "production",
		"production_HOST": "prod.internal",
		"FALLBACK":        "standby.internal",
	})

	nestedKey, err := resolveText("${${STAGE}_HOST}", lookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve nested key: %v", err)
	}
	if nestedKey != "prod.internal" {
		t.Errorf("nested key resolved %q, want the composed key's value", nestedKey)
	}

	nestedDefault, err := resolveText("${MISSING:${FALLBACK}}", lookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve nested default: %v", err)
	}
	if nestedDefault != "standby.internal" {
		t.Errorf("nested default resolved %q, want the fallback variable's value", nestedDefault)
	}
}

func TestAResolvedValueIsItselfResolved(t *testing.T) {
	lookup := mapLookup(map[string]string{"URL": "https://${HOST}/", "HOST": "shop.internal"})
	resolved, err := resolveText("${URL}", lookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "https://shop.internal/" {
		t.Errorf("resolved = %q, want the placeholder inside the value resolved recursively", resolved)
	}
}

func TestCircularReferencesAreDetected(t *testing.T) {
	lookup := mapLookup(map[string]string{"A": "${B}", "B": "${A}"})
	_, err := resolveText("${A}", lookup, map[string]bool{})
	if !errors.Is(err, ErrCircularPlaceholder) {
		t.Fatalf("resolve error = %v, want ErrCircularPlaceholder", err)
	}
}

func TestAPlaceholderResolvesAgainstTheDocumentItself(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{
			"name": "shop",
			"server": {"host": "shop.internal", "port": 8080},
			"tags": ["${server.host}:${server.port}"]
		}`)),
		WithLookup(emptyLookup),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Tags[0] != "shop.internal:8080" {
		t.Errorf("tag = %q, want the document's own values, with the number rendered as text", target.Tags[0])
	}
}

func TestTheEnvironmentWinsOverTheDocument(t *testing.T) {
	loader := NewLoader(
		WithDocument([]byte(`{"name": "${server.host}", "server": {"host": "from-document", "port": 1}}`)),
		WithLookup(mapLookup(map[string]string{"SERVER_HOST": "from-environment"})),
	)

	var target applicationConfiguration
	if err := loader.Load(&target); err != nil {
		t.Fatalf("load: %v", err)
	}
	if target.Name != "from-environment" {
		t.Errorf("name = %q, want the relaxed environment variable to win over the document value", target.Name)
	}
}

func TestAnEscapedSeparatorStaysPartOfTheKey(t *testing.T) {
	lookup := mapLookup(map[string]string{"key:with:colons": "resolved"})
	resolved, err := resolveText(`${key\:with\:colons}`, lookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "resolved" {
		t.Errorf("resolved = %q, want the escaped colons kept inside the key", resolved)
	}
}

func TestAnEscapedSeparatorInsideANestedPlaceholderIsPreserved(t *testing.T) {
	lookup := mapLookup(map[string]string{"x:y": "TARGET", "TARGET": "resolved"})
	resolved, err := resolveText(`${${x\:y}:d}`, lookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "resolved" {
		t.Errorf("resolved = %q, want the nested key's escape to survive the outer split", resolved)
	}
}

func TestAnUnterminatedPrefixIsLiteralText(t *testing.T) {
	resolved, err := resolveText("broken ${tail", emptyLookup, map[string]bool{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != "broken ${tail" {
		t.Errorf("resolved = %q, want the unterminated prefix left as literal text", resolved)
	}
}
