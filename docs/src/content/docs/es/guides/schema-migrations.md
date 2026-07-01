---
title: Migraciones de esquema
description: Cómo la outbox pone al día su esquema a través de un Migrator acoplable, la opción por defecto NativeMigrator sin dependencias y el adaptador opcional respaldado por goway.
---

La outbox necesita dos tablas antes de poder persistir una publicación. Las crea a través de una única costura (seam), la interfaz `Migrator`, por lo que la elección del motor de migración se mantiene fuera del código de consulta. La implementación por defecto utiliza únicamente `database/sql`, que es lo que mantiene al núcleo de la outbox libre de cualquier dependencia de un motor de migración.

## La interfaz Migrator

Un `Migrator` pone al día el esquema de la outbox contra una base de datos abierta. El store lo llama exactamente una vez, desde `Store.Initialize`:

```go
type Migrator interface {
	// Migrate applies schema to db for the given dialect.
	Migrate(ctx context.Context, db *sql.DB, dialect outbox.Dialect, schema fs.FS) error
}
```

Cada argumento es suministrado por el store, por lo que una implementación nunca tiene que saber dónde reside el esquema:

| Argumento | Significado |
| --- | --- |
| `db` | El `*sql.DB` abierto, un handle concreto, porque algunos motores requieren uno. |
| `dialect` | La base de datos de destino, ya sea `outbox.DialectSQLite` o `outbox.DialectPostgres`. |
| `schema` | Los scripts de migración embebidos, con raíz en el directorio que contiene los archivos SQL. |

Las implementaciones deben ser idempotentes. `Initialize` puede ejecutarse en cada inicio, por lo que llamar a `Migrate` contra una base de datos ya migrada debe ser un no-op.

## El valor predeterminado nativo

El núcleo incluye `NativeMigrator`, una implementación exclusiva de `database/sql` que es el valor predeterminado sin configuración. Ambos constructores de store lo conectan automáticamente, de modo que el caso común no necesita ningún `Migrator` y el `go.mod` del núcleo no conlleva ninguna dependencia de un motor de migración:

```go
store := outbox.NewPostgresStore(db, outbox.CompletionModeUpdate)

if err := store.Initialize(ctx); err != nil {
	return err
}
```

`NativeMigrator` ejecuta cada script `*.sql` embebido en orden ascendente de nombre de archivo, dividiendo cada uno en sentencias individuales en el límite del punto y coma. Los scripts utilizan `CREATE TABLE` / `CREATE INDEX IF NOT EXISTS`, por lo que volver a ejecutarlos es un no-op y la ruta nativa no mantiene ninguna tabla de historial de esquemas propia. El mismo DDL es portable entre SQLite y PostgreSQL, razón por la cual se acepta el dialecto por simetría de la interfaz pero nunca se inspecciona aquí.

También puede nombrarlo explícitamente, aunque el resultado es idéntico al valor predeterminado:

```go
store := outbox.NewSQLiteStore(db, outbox.CompletionModeUpdate,
	outbox.WithMigrator(outbox.NativeMigrator{}))
```

## El esquema embebido

Los scripts exactos que aplican tanto el migrador nativo como cualquier adaptador están expuestos por `outbox.Schema`, un sistema de archivos de solo lectura con raíz en el directorio de migración:

```go
schema := outbox.Schema()
// Entries are "V1__create_event_publication_tables.sql" and any successors,
// not "migration/V1__...".
```

El primer script crea las tablas de publicación y de archivo junto con sus índices:

```sql
CREATE TABLE IF NOT EXISTS event_publication
(
    id                     TEXT    NOT NULL PRIMARY KEY,
    listener_id            TEXT    NOT NULL,
    event_type             TEXT    NOT NULL,
    serialized_event       TEXT    NOT NULL,
    publication_date       TEXT    NOT NULL,
    completion_date        TEXT,
    status                 TEXT    NOT NULL,
    completion_attempts    INTEGER NOT NULL DEFAULT 0,
    last_resubmission_date TEXT
);
-- ... indexes and the event_publication_archive table
```

Dado que cada migrador lee estos scripts exactos, el esquema tiene una sola fuente: un adaptador nunca duplica el SQL.

## Optar por goway

Un `Migrator` respaldado por un motor registra las migraciones aplicadas en una tabla de historial de esquemas, el contrato al estilo Flyway en el que ya confían algunos equipos. La outbox proporciona un adaptador respaldado por goway para esto, en su propio módulo de modo que solo los clientes que opten por él incorporen goway a su compilación:

```sh
go get github.com/cgardev/gokeel/outbox/gowaymigrator
```

Pase el adaptador a `outbox.WithMigrator`:

```go
import (
	"github.com/cgardev/gokeel/outbox"
	"github.com/cgardev/gokeel/outbox/gowaymigrator"
)

store := outbox.NewPostgresStore(db, outbox.CompletionModeUpdate,
	outbox.WithMigrator(gowaymigrator.New()))
```

El adaptador mapea el dialecto del núcleo al propio dialecto de goway y ejecuta el esquema embebido como una migración con versión, registrando el historial en la tabla `outbox.SchemaHistoryTable`:

```go
const SchemaHistoryTable = "event_publication_schema_history"
```

Escribir en esa tabla exacta preserva el contrato de historial de migración en disco para las bases de datos que fueron migradas previamente por goway. Importar el paquete `gowaymigrator` es la única acción que incorpora goway a una compilación; el propio núcleo de la outbox continúa dependiendo únicamente de `database/sql`.

## Un Migrator personalizado

Cualquier función con la firma correcta es un `Migrator` a través de `MigratorFunc`, por lo que una estrategia única no necesita un tipo nuevo. El siguiente adaptador registra en el log antes de delegar en la implementación nativa:

```go
logging := outbox.MigratorFunc(func(
	ctx context.Context, db *sql.DB, dialect outbox.Dialect, schema fs.FS,
) error {
	slog.Info("applying outbox schema", "dialect", dialect)
	return outbox.NativeMigrator{}.Migrate(ctx, db, dialect, schema)
})

store := outbox.NewSQLiteStore(db, outbox.CompletionModeUpdate,
	outbox.WithMigrator(logging))
```

Un `Migrator` personalizado también puede ignorar por completo el esquema suministrado y aplicar las tablas a través de un sistema de migración que ya ejecute, siempre y cuando las tablas `event_publication` y `event_publication_archive` resultantes coincidan con las columnas que lee el store. Mantenga la implementación idempotente, ya que `Initialize` puede ejecutarse en cada inicio.

## Dónde rinde frutos la costura

El valor predeterminado nativo significa que un proyecto nuevo obtiene una outbox en funcionamiento sin módulos adicionales que instalar, mientras que `WithMigrator` permite a un equipo integrar el esquema de la outbox en el motor de migración que ya opera. Ambas rutas leen el mismo `outbox.Schema`, por lo que el SQL nunca diverge. Para conocer toda la superficie, consulte la [referencia del Schema Migrator](/gokeel/es/reference/migrator/); para ver el store que soportan estas migraciones, consulte [La outbox transaccional](/gokeel/es/guides/transactional-outbox/).
