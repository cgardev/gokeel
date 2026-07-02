---
title: Configuration
description: Constructing a Loader over layered JSON sources, resolving environment placeholders, binding onto structs, and generating a JSON Schema with GenerateSchema.
---

The `conf` package provides externalized configuration for Go applications:
the analog of the `application.properties` and `application.yml` story of
Spring Boot, carried by JSON documents. A `Loader` merges ordered document
sources, resolves `${...}` placeholders against the environment and the
document itself, and binds the result onto a plain Go struct with relaxed
conversions. A `Loader` is immutable after construction and safe for
concurrent use.

```go
import "github.com/cgardev/gokeel/conf"
```

## NewLoader

`NewLoader` constructs a `Loader` over the given sources. Sources are
overlaid in option order — a later document overrides an earlier one key by
key, with objects merged deeply — mirroring how later Spring property sources
override earlier ones. Sources are read at every `Load`, so a file source
observes the file as it is at that moment.

```go
func NewLoader(options ...Option) *Loader
```

```go
loader := conf.NewLoader(
    conf.WithFilesystemFile(defaults, "application.json"),
    conf.WithOptionalFile("/etc/shop/application.json"),
)
```

Throughout this page, `loader` is the `*Loader` constructed here and
`settings` is the target struct being bound.

## The options

An `Option` customizes a `Loader` at construction time.

### WithFile

```go
func WithFile(path string) Option
```

`WithFile` appends a document read from the file at the given path. A missing
file fails the `Load` with an error matching `fs.ErrNotExist`, the counterpart of
the `ConfigDataLocationNotFoundException` that aborts a Spring Boot startup.

```go
conf.WithFile("/etc/shop/application.json")
```

### WithOptionalFile

```go
func WithOptionalFile(path string) Option
```

`WithOptionalFile` appends a document read from the file at the given path,
skipped silently when the file does not exist: the analog of the `optional:` prefix
of `spring.config.import`.

```go
conf.WithOptionalFile("./application.local.json")
```

### WithFilesystemFile

```go
func WithFilesystemFile(filesystem fs.FS, path string) Option
```

`WithFilesystemFile` appends a document read from a file inside the given
filesystem. An `fs.FS` built with `go:embed` carries defaults inside the
binary the way `application.properties` travels inside a Spring Boot jar,
with external files layered over it through later options. A nil filesystem
is ignored.

```go
//go:embed application.json
var defaults embed.FS

conf.WithFilesystemFile(defaults, "application.json")
```

### WithDocument

```go
func WithDocument(document []byte) Option
```

`WithDocument` appends a literal JSON document. The bytes are copied, so
later mutation of the caller's slice cannot affect the `Loader`.

```go
conf.WithDocument([]byte(`{"server": {"port": 9090}}`))
```

### WithLookup

```go
func WithLookup(lookup func(name string) (string, bool)) Option
```

`WithLookup` replaces the environment the placeholders resolve against. The
default is `os.LookupEnv`; tests and processes that draw secrets from
another store inject their own function here. A nil lookup is ignored.

```go
conf.WithLookup(func(name string) (string, bool) { return vault.Read(name) })
```

## Loader.Load

`Load` reads every source, merges the documents in order, resolves the
placeholders, and binds the result onto the target, which must be a non-nil
pointer to a struct. Names the merged document does not mention keep the
values already present in the target, so a caller sets code-level defaults by
filling the struct before the call — the analog of field initializers on an
`@ConfigurationProperties` class.

```go
func (loader *Loader) Load(target any) error
```

```go
settings := shopConfiguration{Server: serverConfiguration{Port: 8080}}
if err := loader.Load(&settings); err != nil {
    return err
}
```

A key that matches no field fails with `ErrUnknownKey` naming the full
dotted path. This diverges from Spring, whose binding ignores unknown
properties by default: a JSON document is meant to be edited against the
generated schema, so a stray key is a mistake worth failing loudly on. A
root-level `$schema` key is the documented exception; it associates the
schema in editors and is discarded before binding.

Binding converts with the leniency of Spring's relaxed binding: strings
convert to numbers and to booleans with the Spring token sets (`true`, `on`,
`yes`, `1` and `false`, `off`, `no`, `0`, in any case), a `time.Duration`
reads the Go notation `"1m30s"` or a raw number of nanoseconds (Spring
instead reads a bare number as milliseconds), and a field whose type
implements `encoding.TextUnmarshaler`, such as `time.Time` or `slog.Level`,
binds from a string. Numbers keep their full `int64` precision, a JSON
`null` leaves the current value in place, and embedded structs are promoted
the way `encoding/json` promotes them.

## Placeholders

Every string in the merged document may defer to the environment with the
Spring placeholder grammar: `${NAME}` requires a value, `${NAME:default}`
falls back after the first unescaped colon (an empty default is allowed),
placeholders compose inside larger strings and nest in keys and defaults, a
value obtained for a key is itself resolved recursively, and a backslash
escapes a literal `${` — written `\\${` inside a JSON string.

```go
document := []byte(`{
    "name": "${SHOP_NAME:shop}",
    "database": "postgres://${DATABASE_HOST}:${DATABASE_PORT:5432}/shop",
    "health": "http://${server.host}:${server.port}/health"
}`)
```

Resolution consults the environment first — under the exact name, then under
the relaxed form Spring's binding reads, in which dashes are removed, other
separators become underscores, and the result is uppercased, so
`${demo.item-price}` also finds `DEMO_ITEMPRICE` — and the merged document
itself last, by dotted path, so a value refers back to a previously defined
one. The environment wins over the document, the order Spring gives
operating-system variables over configuration files.

A placeholder with no value and no default fails with
`ErrUnresolvedPlaceholder` naming the key, the analog of the
`PlaceholderResolutionException` that aborts a Spring Boot startup; a chain
of references that returns to an earlier key fails with
`ErrCircularPlaceholder`. An unterminated `${` is literal text, as it is for
the Spring parser.

## GenerateSchema

`GenerateSchema` derives a JSON Schema draft 2020-12 document from the
struct the configuration binds onto, through the Google JSON Schema
implementation `github.com/google/jsonschema-go`: the counterpart of the
configuration metadata Spring Boot generates from `@ConfigurationProperties`
classes for editor completion. Field names follow the `json` tags, a field
is required unless its tag carries `omitempty` or `omitzero`, and objects
reject unknown properties, matching the strict binding of `Load`; the root
additionally admits the `$schema` key itself, typed as a `uri-reference`,
so the association a document carries validates cleanly.

```go
func GenerateSchema(prototype any, definitions ...SchemaDefinition) ([]byte, error)
```

The `json` tag is the only tag the struct carries. Descriptions, defaults,
and constraints live in `SchemaDefinition` values, keyed by the dotted
document path of each field, so the structure that stores the values stays
separate from the definition of its schema — and fields of imported types
can be documented without tagging them:

```go
type serverConfiguration struct {
    Host string `json:"host"`
    Port int    `json:"port"`
    Mode string `json:"mode,omitempty"`
}

schema, err := conf.GenerateSchema(serverConfiguration{}, conf.SchemaDefinition{
    Description: "Server configuration.",
    Fields: map[string]conf.FieldDefinition{
        "host": {Description: "Interface to bind", Default: "localhost"},
        "port": {Minimum: conf.Pointer(1.0), Maximum: conf.Pointer(65535.0)},
        "mode": {Enum: []any{"development", "production"}},
    },
})
```

A `FieldDefinition` carries `Title`, `Description`, `Default`, `Enum`,
`Examples`, `Pattern`, `Format`, `Minimum`, `Maximum`, `MinLength`, and
`MaxLength`; `conf.Pointer` fills the optional bounds inside a literal.
Definitions are overlaid in argument order, on an array field `Enum` and
`Examples` constrain the elements, and every documented default is
validated against the schema of its field. A `time.Duration` field is
declared as a string matching the Go duration notation or an integer
nanosecond count, and any `encoding.TextUnmarshaler` type as a string,
joined by its direct integer, number, or boolean form when the underlying
kind binds one.

The schema describes the canonical form of a document while the binding
stays deliberately laxer: the relaxed conversions read numbers and booleans
out of strings, a `${...}` placeholder may stand in a non-string field, and
JSON null keeps a field's default — all of which bind even though the
schema flags them. Required keys are likewise a schema-level contract
`Load` never demands, so a field whose zero value or code-level default is
acceptable should carry `omitempty` or `omitzero`. A schema-valid document
always binds; a binding document is not always schema-valid.

A schema-metadata struct tag, an embedded field the schema cannot describe
faithfully (one carrying a json name, of a non-struct type, or of a type
with a special-case schema), a definition path that designates no declared
field, a mistyped default, a recursive type, and a prototype that does not
describe a JSON object are reported as errors.

## Errors

The package exports four sentinel errors, each matchable with `errors.Is`:

- `ErrUnresolvedPlaceholder` reports a `${name}` with no value in the
  environment or the document and no default after a colon. The placeholder
  name is attached to the returned error.
- `ErrCircularPlaceholder` reports a placeholder whose resolution leads back
  to itself, directly or through other placeholders.
- `ErrUnknownKey` reports a document key that matches no field of the target
  struct, with the full dotted path attached.
- `ErrTypeMismatch` reports a document value that cannot be converted to the
  type of the field it binds to — the analog of the `BindException` a Spring
  Boot binding failure raises — with the path and both types attached.

```go
if err := loader.Load(&settings); errors.Is(err, conf.ErrUnknownKey) {
    // the document names a key the struct does not declare
}
```

A missing required file surfaces as an error matching `fs.ErrNotExist`.

For the guided tour of layering, placeholders, and the editor workflow, see
[Externalized Configuration](/gokeel/guides/externalized-configuration/); for
copy-ready snippets, including retuning the log levels from the
configuration document, see the [cookbook](/gokeel/cookbook/conf/).
