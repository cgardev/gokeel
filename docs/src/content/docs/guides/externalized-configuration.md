---
title: Externalized Configuration
description: Load layered JSON documents, defer values to the environment with placeholders, bind the result onto a plain struct, and generate a JSON Schema for editors.
---

The `conf` module externalizes configuration the way Spring Boot's
`application.properties` and `application.yml` do, carried by JSON documents.
A `Loader` merges ordered document sources, resolves `${...}` placeholders
against the environment and the document itself, and binds the result onto a
plain Go struct with the relaxed conversions Spring's
`@ConfigurationProperties` binding performs. From that same struct,
`GenerateSchema` derives a JSON Schema, so editors and code assistants know
every key, its type, and its allowed values while the documents are edited.

The module depends only on the Go standard library. A `Loader` is immutable
after construction and safe for concurrent use, and its sources are read at
every `Load`, so a reload observes the files as they are at that moment.

## Declaring the configuration

Configuration is a plain struct with `json` tags. The values already present
in the target when `Load` runs are the code-level defaults — the analog of
field initializers on an `@ConfigurationProperties` class — because names the
documents do not mention keep whatever the struct holds:

```go
import "github.com/cgardev/gokeel/conf"

type serverConfiguration struct {
	Host    string        `json:"host"`
	Port    int           `json:"port"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

type shopConfiguration struct {
	Name   string              `json:"name"`
	Server serverConfiguration `json:"server"`
}
```

## Loading documents

`NewLoader` takes the sources in the order they override one another: a later
document wins key by key, with objects merged deeply, mirroring how later
Spring property sources override earlier ones. Defaults travel inside the
binary through an `fs.FS` — the counterpart of the `application.properties`
packaged in a Spring Boot jar — and an external file layers over them:

```go
//go:embed application.json
var defaults embed.FS

loader := conf.NewLoader(
	conf.WithFilesystemFile(defaults, "application.json"),
	conf.WithOptionalFile("/etc/shop/application.json"),
)

settings := shopConfiguration{Server: serverConfiguration{Port: 8080}}
if err := loader.Load(&settings); err != nil {
	return err
}
```

`WithOptionalFile` is the analog of the `optional:` prefix of
`spring.config.import`: a missing file is skipped silently. A file named with
`WithFile` must exist — the failure matches `fs.ErrNotExist`, the counterpart
of the `ConfigDataLocationNotFoundException` that aborts a Spring Boot
startup. `WithDocument` appends a literal document, copied defensively.

## Deferring to the environment

A string value may name an environment variable instead of carrying the
value, exactly as a Spring property value does. `${NAME}` reads the variable
and fails the `Load` with `conf.ErrUnresolvedPlaceholder` when it is unset;
`${NAME:default}` falls back to the default after the colon; and
placeholders compose inside larger strings:

```go
document := []byte(`{
	"name": "${SHOP_NAME:shop}",
	"server": {
		"host": "${SHOP_HOST}",
		"port": "${SHOP_PORT:8080}"
	},
	"database": "postgres://${DATABASE_HOST}:${DATABASE_PORT:5432}/shop"
}`)
```

The grammar is Spring's, including its corners: placeholders nest in keys and
defaults, a value obtained for a key is itself resolved recursively, a
backslash escapes a literal `${`, and a chain that returns to an earlier key
fails with `conf.ErrCircularPlaceholder` instead of recursing forever.

Resolution consults the environment first — under the exact name, then under
the relaxed form Spring reads, so `${demo.item-price}` also finds
`DEMO_ITEMPRICE` — and the merged document itself last, so a value can refer
back to a previously defined one, the way Spring filters property values
through the existing Environment:

```go
document := []byte(`{
	"server": {"host": "shop.internal", "port": 8080},
	"health":  "http://${server.host}:${server.port}/health"
}`)
```

## Relaxed binding

Values convert to the field type with the leniency of Spring's relaxed
binding. Strings convert to numbers and to booleans — with Spring's token
sets, so `"yes"`, `"on"`, and `"1"` are true — a `time.Duration` field reads
the Go notation `"1m30s"` (or a raw number of nanoseconds; Spring instead
reads a bare number as milliseconds), and any field whose type unmarshals
text, such as `time.Time` or `slog.Level`, binds from a string. Numbers keep
their full `int64` precision, and a JSON `null` leaves the current value in
place, so it never erases a default.

One rule is deliberately stricter than Spring, whose binding ignores unknown
properties by default: a key that matches no field fails the `Load` with
`conf.ErrUnknownKey` naming the full dotted path. A document meant to be
edited against a schema treats a stray key as a mistake worth failing loudly
on. The root-level `$schema` key is the documented exception: it associates
the schema in editors and is discarded before binding.

## The schema for editors

`GenerateSchema` derives a JSON Schema draft 2020-12 document from the same
struct the documents bind onto — the counterpart of the configuration
metadata Spring Boot generates from `@ConfigurationProperties` classes for
editor completion. Fields without `omitempty` are required, objects reject
unknown properties just as `Load` does, and the `jsonschema` field tag
contributes descriptions, defaults, enumerations, and bounds:

```go
type serverConfiguration struct {
	Host string `json:"host" jsonschema:"description=Interface to bind,default=localhost"`
	Port int    `json:"port" jsonschema:"minimum=1,maximum=65535"`
	Mode string `json:"mode,omitempty" jsonschema:"enum=development,enum=production"`
}

schema, err := conf.GenerateSchema(shopConfiguration{})
if err != nil {
	return err
}
err = os.WriteFile("application.schema.json", schema, 0o644)
```

A document then points its `$schema` key at the file —
`"$schema": "./application.schema.json"` — and the editor completes keys,
checks types, flags stray keys, and shows each description in place. The
generated root declares the `$schema` property itself, so the association
never trips the strict validation it enables.

## Where to go next

The [Configuration reference](/gokeel/reference/conf/) covers every exported
symbol — the options, the placeholder grammar, and the schema tags — and the
[cookbook](/gokeel/cookbook/conf/) collects copy-ready recipes, including how
to retune the [Log Levels](/gokeel/guides/log-levels/) of a running
application from the configuration document.
