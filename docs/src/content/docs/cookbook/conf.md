---
title: Configuration
description: Recipes for loading layered JSON configuration, environment placeholders, schema generation for editors, and retuning the log levels from the configuration document.
---

## Load a configuration file onto a struct

You want the application settings in a JSON file, bound onto a plain struct
with your defaults preserved.

```go
import "github.com/cgardev/gokeel/conf"

type shopConfiguration struct {
    Name   string `json:"name"`
    Server struct {
        Host string `json:"host"`
        Port int    `json:"port"`
    } `json:"server"`
}

settings := shopConfiguration{}
settings.Server.Port = 8080 // code-level default

loader := conf.NewLoader(conf.WithFile("application.json"))
if err := loader.Load(&settings); err != nil {
    return err
}
```

Gotcha: names the document does not mention keep the values already in the
struct, so defaults belong in the target, not in the file; a key the struct
does not declare fails with `conf.ErrUnknownKey` naming its dotted path.

## Ship defaults inside the binary and override them outside

You want the binary to run with built-in defaults and let each environment
layer its own file on top, like `application.properties` inside a Spring
Boot jar.

```go
//go:embed application.json
var defaults embed.FS

loader := conf.NewLoader(
    conf.WithFilesystemFile(defaults, "application.json"),
    conf.WithOptionalFile("/etc/shop/application.json"),
)
```

Gotcha: later sources win key by key with objects merged deeply;
`WithOptionalFile` skips a missing file like Spring's `optional:` prefix,
while a missing `WithFile` fails the load with `fs.ErrNotExist`.

## Read secrets from the environment

You want credentials out of the file, read from environment variables at
load time, with defaults for local development.

```go
document := []byte(`{
    "database": "postgres://shop:${DATABASE_PASSWORD}@${DATABASE_HOST:localhost}/shop"
}`)

loader := conf.NewLoader(conf.WithDocument(document))
```

Gotcha: a placeholder without a default fails the load with
`conf.ErrUnresolvedPlaceholder` naming the variable, so a missing secret
aborts startup instead of connecting with an empty password.

## Generate the schema for editor completion

You want the editor to complete keys, check types, and show descriptions
while the configuration file is edited.

```go
type serverConfiguration struct {
    Host string `json:"host" jsonschema:"description=Interface to bind,default=localhost"`
    Port int    `json:"port" jsonschema:"minimum=1,maximum=65535"`
}

schema, err := conf.GenerateSchema(shopConfiguration{})
if err != nil {
    return err
}
err = os.WriteFile("application.schema.json", schema, 0o644)
// application.json then starts with:
//   "$schema": "./application.schema.json"
```

Gotcha: the schema rejects unknown properties exactly as `Load` does, so the
editor flags the same mistakes the program would; regenerate the file
whenever the struct changes.

## Configure the log levels from the configuration document

You want the [logging](/gokeel/guides/log-levels/) module tuned from the same
externalized document as everything else, `logging.level` style.

```go
import (
    "github.com/cgardev/gokeel/conf"
    "github.com/cgardev/gokeel/logging"
)

type shopConfiguration struct {
    Name    string                `json:"name"`
    Logging logging.Configuration `json:"logging,omitempty"`
}

document := []byte(`{
    "name": "shop",
    "logging": {
        "levels": {
            "root": "${LOG_LEVEL:info}",
            "github.com/acme/shop/orders": "debug"
        }
    }
}`)

var settings shopConfiguration
if err := conf.NewLoader(conf.WithDocument(document)).Load(&settings); err != nil {
    return err
}

levels := logging.NewManager()
if err := levels.Apply(settings.Logging); err != nil {
    return err
}
```

Gotcha: `logging.Configuration` is plain maps with `json` tags, so it binds
like any other section and `Apply` still validates every level token; the
`${LOG_LEVEL:info}` placeholder lets an operator retune the root level
through an environment variable without touching the file.

## Catch a misspelled key before it hides a setting

You want a typo in the document to abort startup instead of silently leaving
the intended setting at its default.

```go
document := []byte(`{"server": {"prot": 9090}}`)

err := conf.NewLoader(conf.WithDocument(document)).Load(&settings)
if errors.Is(err, conf.ErrUnknownKey) {
    // the error names the full path: server.prot
}
```

Gotcha: Spring ignores unknown properties by default; `conf` fails loudly on
them, and the generated schema makes the editor flag the same typo while the
file is being written.

For the full surface — the options, the placeholder grammar, and the schema
tags — see [Externalized Configuration](/gokeel/guides/externalized-configuration/)
and the [Configuration reference](/gokeel/reference/conf/).
