// Package configuration provides externalized configuration for Go
// applications: the analog of the application.properties and application.yml
// story of Spring Boot, carried by JSON documents. A Loader merges ordered
// document sources, resolves ${...} placeholders against the environment and
// the document itself, and binds the result onto a plain Go struct with the
// relaxed conversions the @ConfigurationProperties binding of Spring
// performs.
//
// Values may defer to the environment the way Spring property values do: a
// string such as "${DATABASE_URL:postgres://localhost/application}" reads
// the variable and falls back to the default after the colon. Placeholders
// nest, compose inside larger strings, resolve recursively, and follow the
// Spring grammar, including the backslash escape for a literal ${.
//
// GenerateSchema derives a JSON Schema from the same struct the documents
// bind onto, the counterpart of the configuration metadata Spring Boot
// generates for editor completion: editors and code assistants associate the
// schema with a document through its $schema key, which the Loader
// accordingly tolerates and the generated schema declares.
//
// A Loader is immutable after construction and safe for concurrent use. The
// package depends only on the Go standard library.
package configuration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Loader reads one or more JSON documents, overlays them in order, resolves
// their placeholders, and binds the merged result onto a target struct. It
// plays the role of the Spring Environment: the single place where every
// configuration source meets and later sources override earlier ones.
type Loader struct {
	sources []documentSource
	lookup  func(name string) (string, bool)
}

// documentSource is one ordered provider of a JSON document. Sources are
// read at every Load, so a file source observes the file as it is at that
// moment.
type documentSource struct {
	name     string
	optional bool
	read     func() ([]byte, error)
}

// Option customizes a Loader at construction time.
type Option func(*Loader)

// WithFile appends a document read from the file at path. A missing file
// fails the Load with an error matching fs.ErrNotExist, the counterpart of
// the ConfigDataLocationNotFoundException that aborts a Spring Boot startup.
func WithFile(path string) Option {
	return func(loader *Loader) {
		loader.sources = append(loader.sources, documentSource{
			name: path,
			read: func() ([]byte, error) { return os.ReadFile(path) },
		})
	}
}

// WithOptionalFile appends a document read from the file at path, skipped
// silently when the file does not exist: the analog of the "optional:"
// prefix of spring.config.import.
func WithOptionalFile(path string) Option {
	return func(loader *Loader) {
		loader.sources = append(loader.sources, documentSource{
			name:     path,
			optional: true,
			read:     func() ([]byte, error) { return os.ReadFile(path) },
		})
	}
}

// WithFilesystemFile appends a document read from a file inside the given
// filesystem. An fs.FS built with go:embed carries defaults inside the
// binary the way application.properties travels inside a Spring Boot jar,
// with external files layered over it through later options.
func WithFilesystemFile(filesystem fs.FS, path string) Option {
	return func(loader *Loader) {
		if filesystem == nil {
			return
		}
		loader.sources = append(loader.sources, documentSource{
			name: path,
			read: func() ([]byte, error) { return fs.ReadFile(filesystem, path) },
		})
	}
}

// WithDocument appends a literal JSON document. The bytes are copied, so
// later mutation of the caller's slice cannot affect the Loader.
func WithDocument(document []byte) Option {
	return func(loader *Loader) {
		copied := bytes.Clone(document)
		loader.sources = append(loader.sources, documentSource{
			name: "inline document",
			read: func() ([]byte, error) { return copied, nil },
		})
	}
}

// WithLookup replaces the environment the placeholders resolve against. The
// default is os.LookupEnv; tests and processes that draw secrets from
// another store inject their own function here. A nil lookup is ignored.
func WithLookup(lookup func(name string) (string, bool)) Option {
	return func(loader *Loader) {
		if lookup != nil {
			loader.lookup = lookup
		}
	}
}

// NewLoader constructs a Loader over the given sources. Sources are overlaid
// in option order — a later document overrides an earlier one key by key,
// with objects merged deeply — mirroring how later Spring property sources
// override earlier ones.
func NewLoader(options ...Option) *Loader {
	loader := &Loader{lookup: os.LookupEnv}
	for _, option := range options {
		option(loader)
	}
	return loader
}

// Load reads every source, merges the documents in order, resolves the
// placeholders, and binds the result onto target, which must be a non-nil
// pointer to a struct. Names the merged document does not mention keep the
// values already present in target, so a caller sets code-level defaults by
// filling the struct before the call — the analog of field initializers on
// an @ConfigurationProperties class.
//
// A key that matches no field fails with ErrUnknownKey naming the full path.
// This diverges from Spring, whose binding ignores unknown properties by
// default: a JSON document is meant to be edited against the generated
// schema, so a stray key is a mistake worth failing loudly on. A root-level
// "$schema" key is the documented exception; it associates the schema in
// editors and is discarded before binding.
func (loader *Loader) Load(target any) error {
	merged := map[string]any{}
	for _, source := range loader.sources {
		document, err := source.read()
		if err != nil {
			if source.optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read configuration document %s: %w", source.name, err)
		}
		tree, err := decodeDocument(document)
		if err != nil {
			return fmt.Errorf("decode configuration document %s: %w", source.name, err)
		}
		mergeTree(merged, tree)
	}
	delete(merged, "$schema")

	resolved, err := resolveTree(merged, loader.chain(merged))
	if err != nil {
		return err
	}
	return bind(resolved.(map[string]any), target)
}

// chain builds the placeholder lookup: the environment first — under the
// exact name, then under the relaxed environment form — and the merged
// document itself last, so ${database.host} reaches both DATABASE_HOST and
// the document's own database.host. Environment wins over document, the
// order Spring gives operating-system variables over configuration files.
func (loader *Loader) chain(document map[string]any) func(name string) (string, bool) {
	return func(name string) (string, bool) {
		if value, ok := loader.lookup(name); ok {
			return value, true
		}
		if relaxed := environmentName(name); relaxed != name {
			if value, ok := loader.lookup(relaxed); ok {
				return value, true
			}
		}
		return documentValue(document, name)
	}
}

// decodeDocument parses one JSON document into a tree. Numbers are kept as
// json.Number so 64-bit integers survive without floating-point loss, and a
// document must hold exactly one JSON object.
func decodeDocument(document []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var tree map[string]any
	if err := decoder.Decode(&tree); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("document contains more than one JSON value")
	}
	if tree == nil {
		return nil, errors.New("document must be a JSON object")
	}
	return tree, nil
}

// mergeTree overlays overlay onto base in place: objects merge deeply, and
// every other value — including arrays — replaces the value beneath it,
// matching how a later Spring property source replaces a property wholesale.
func mergeTree(base, overlay map[string]any) {
	for key, overlayValue := range overlay {
		baseChild, baseIsMap := base[key].(map[string]any)
		overlayChild, overlayIsMap := overlayValue.(map[string]any)
		if baseIsMap && overlayIsMap {
			mergeTree(baseChild, overlayChild)
			continue
		}
		base[key] = overlayValue
	}
}

// environmentName converts a property-style name to the form the relaxed
// binding of Spring reads from the environment: dashes are removed, every
// other non-alphanumeric character becomes an underscore, and the result is
// uppercased, so demo.item-price is found under DEMO_ITEMPRICE.
func environmentName(name string) string {
	var builder bytes.Buffer
	for _, character := range name {
		switch {
		case character == '-':
		case character >= 'a' && character <= 'z':
			builder.WriteRune(character - 'a' + 'A')
		case (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9'):
			builder.WriteRune(character)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

// documentValue resolves a dotted path inside the merged document and
// renders the scalar found there as text, so a placeholder can refer back to
// a previously defined value the way Spring property values are filtered
// through the existing Environment. Objects, arrays, and JSON null report
// not found: only scalars can be embedded in a string.
func documentValue(document map[string]any, path string) (string, bool) {
	var current any = document
	for segment := range splitPath(path) {
		tree, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = tree[segment]
		if !ok {
			return "", false
		}
	}
	switch scalar := current.(type) {
	case string:
		return scalar, true
	case json.Number:
		return scalar.String(), true
	case bool:
		if scalar {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// splitPath yields the dot-separated segments of a document path.
func splitPath(path string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for index := 0; index <= len(path); index++ {
			if index == len(path) || path[index] == '.' {
				if !yield(path[start:index]) {
					return
				}
				start = index + 1
			}
		}
	}
}
