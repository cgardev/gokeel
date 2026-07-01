---
title: Schema Migrator
description: Actualización del esquema del outbox con el Migrator nativo o el adaptador goway opcional.
---

Los almacenes de outbox escriben sus publicaciones en un esquema pequeño y fijo, y un `Migrator` es el único punto de unión que cada almacén utiliza para actualizar ese esquema. El almacén lo llama una vez, desde `Store.Initialize`, por lo que la elección del motor de migración queda fuera del código de consulta. El núcleo incluye una implementación nativa de `database/sql` como la opción predeterminada sin configuración, por lo que el caso común no necesita ningún `Migrator` y el `go.mod` del núcleo no lleva ninguna dependencia de motor de migración.

```go
import "github.com/cgardev/gokeel/outbox"
```

## La interfaz Migrator

Un `Migrator` aplica el esquema incrustado a una base de datos abierta para un dialecto dado.

```go
type Migrator interface {
    Migrate(ctx context.Context, db *sql.DB, dialect Dialect, schema fs.FS) error
}
```

Las implementaciones reciben el `*sql.DB` abierto (un manejador concreto, porque algunos motores requieren uno en lugar de una interfaz más estrecha), el `Dialect` de destino y los scripts de esquema incrustados como un `fs.FS` con raíz en el directorio que contiene los archivos SQL, de modo que un adaptador nunca duplica el SQL. Las implementaciones deben ser idempotentes: `Initialize` puede ejecutarse en cada inicio, por lo que llamar a `Migrate` contra una base de datos ya migrada debe ser una operación sin efecto (no-op).

## MigratorFunc

`MigratorFunc` adapta una función ordinaria a la interfaz `Migrator`, por lo que un llamador puede proporcionar una estrategia de migración única sin declarar un tipo.

```go
type MigratorFunc func(ctx context.Context, db *sql.DB, dialect Dialect, schema fs.FS) error
```

```go
var logging outbox.Migrator = outbox.MigratorFunc(
    func(ctx context.Context, db *sql.DB, dialect outbox.Dialect, schema fs.FS) error {
        log.Printf("migrating outbox schema for %s", dialect)
        return outbox.NativeMigrator{}.Migrate(ctx, db, dialect, schema)
    },
)
```

## Dialect

`Dialect` identifica la base de datos de destino sin filtrar ningún tipo de terceros en el núcleo del outbox. Es un enum de tipo string cerrado: el núcleo solo produce las dos constantes a continuación, cada una establecida por el constructor del almacén correspondiente.

```go
type Dialect string

const (
    DialectSQLite   Dialect = "sqlite"
    DialectPostgres Dialect = "postgres"
)
```

Un adaptador de `Migrator` realiza un switch sobre el `Dialect` para elegir su propia representación del dialecto. La ruta nativa lo ignora, porque su DDL es portable.

## NativeMigrator

`NativeMigrator` aplica el esquema del outbox usando únicamente `database/sql`. Es el `Migrator` predeterminado conectado por los constructores del almacén, por lo que el caso común no arrastra ningún motor de migración de terceros.

```go
type NativeMigrator struct{}

func (NativeMigrator) Migrate(ctx context.Context, db *sql.DB, _ Dialect, schema fs.FS) error
```

Ejecuta cada script `*.sql` incrustado en orden ascendente de nombre de archivo, dividiendo cada script en sentencias individuales en el límite del punto y coma. Los scripts utilizan `CREATE TABLE` / `CREATE INDEX IF NOT EXISTS`, por lo que volver a ejecutarlos es una operación sin efecto (no-op); la ruta nativa, por lo tanto, no mantiene ninguna tabla de historial de esquemas propia. El dialecto se acepta por simetría de la interfaz pero no es necesario, porque el DDL `IF NOT EXISTS` es portable entre SQLite y PostgreSQL.

Debido a que es el predeterminado, un almacén creado sin ninguna opción ya lo utiliza:

```go
store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)

// Initialize applies the schema through NativeMigrator.
if err := store.Initialize(ctx); err != nil {
    return err
}
```

## Schema y SchemaHistoryTable

`Schema` devuelve los scripts de migración del outbox incrustados como un sistema de archivos de solo lectura con raíz en el directorio de migración, por lo que sus entradas son `V1__create_event_publication_tables.sql` y sus sucesores, no `migration/V1__...`. Tanto el `Migrator` nativo como cualquier adaptador externo leen estos scripts exactos, por lo que el esquema tiene una única fuente. El valor devuelto es una vista inmutable de los archivos incrustados del paquete; los llamadores no pueden mutarlos.

```go
func Schema() fs.FS
```

`SchemaHistoryTable` es el nombre de la tabla de historial de esquemas que los adaptadores basados en motores utilizan para registrar las migraciones aplicadas. Se exporta para que un adaptador escriba en la misma tabla que el outbox siempre ha utilizado, preservando el contrato de historial de migración en disco para las bases de datos que fueron migradas previamente por un motor.

```go
const SchemaHistoryTable = "event_publication_schema_history"
```

El almacén proporciona `Schema()` al propio `Migrator`; el código de la aplicación rara vez llama a cualquiera de los símbolos directamente, pero ambos están exportados para los adaptadores que manejan el esquema a través de su propio motor.

## Sobrescribir el migrador

`WithMigrator` es la opción en tiempo de construcción que reemplaza al `NativeMigrator` predeterminado. Se pasa a cualquiera de los constructores del almacén, y si el migrador es `nil`, se ignora, por lo que el predeterminado permanece en su lugar.

```go
func WithMigrator(m Migrator) Option
```

```go
store := outbox.NewPostgresStore(
    database,
    outbox.CompletionModeUpdate,
    outbox.WithMigrator(myMigrator),
)
```

## El adaptador goway

Cuando un proyecto ya administra su esquema con un motor de migración versionado al estilo de Flyway, el adaptador opcional en `github.com/cgardev/gokeel/outbox/gowaymigrator` proporciona un `Migrator` respaldado por goway. Importar este paquete es la única acción que incorpora goway en una compilación; el núcleo del outbox en sí solo depende de `database/sql`.

```go
import "github.com/cgardev/gokeel/outbox/gowaymigrator"
```

`New` devuelve el adaptador. Aplica el esquema incrustado propiedad de outbox como una migración versionada, registrando el historial en `SchemaHistoryTable`. Pase el resultado a `WithMigrator`:

```go
func New() outbox.Migrator
```

```go
store := outbox.NewPostgresStore(
    database,
    outbox.CompletionModeUpdate,
    outbox.WithMigrator(gowaymigrator.New()),
)

if err := store.Initialize(ctx); err != nil {
    return err
}
```

El adaptador asigna el `Dialect` del núcleo al propio tipo de dialecto de goway, ejecuta el esquema proporcionado por el almacén y nunca permite que un tipo de goway pase al núcleo del outbox ni que un tipo del outbox pase a goway.

Consulte [Primeros pasos](/gokeel/es/getting-started/) para ver la configuración completa, y la referencia del [Gestor de transacciones](/gokeel/es/reference/transaction-manager/) para conocer la unidad de trabajo dentro de la cual se ejecutan los almacenes.
