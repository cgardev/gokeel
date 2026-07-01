---
title: Vista general de referencia
description: Una referencia sistemática para los bloques de construcción de gokeel, organizada por módulo.
---

Esta referencia documenta el conjunto completo de características de gokeel, la familia de bloques de construcción para un monolito modular en Go. Cada página cubre un área de la API en detalle, con ejemplos reales en Go y el comportamiento que impulsa cada llamada. Para fragmentos de código orientados a tareas, consulte el [Cookbook](/gokeel/es/cookbook/).

## Cómo está organizada la referencia

- [Gestor de transacciones](/gokeel/es/reference/transaction-manager/) — el `Manager`, `Run` y el `RunResult` genérico, el `Querier` resuelto a partir del contexto, y la interfaz `Transactor` de la que dependen los almacenes.
- [Propagación y opciones](/gokeel/es/reference/propagation-and-options/) — los modos de `Propagation` (`Required`, `Supports`, `Mandatory`, `Never`, `Nested`), savepoints y los valores de `Option`: `WithPropagation`, `WithIsolation`, `ReadOnly`, `WithTimeout`, `WithName` y las reglas de rollback.
- [Sincronizaciones y listeners](/gokeel/es/reference/synchronizations/) — los callbacks before-commit, before-completion, after-commit y after-completion registrados con `RegisterBeforeCommit`, `RegisterBeforeCompletion`, `RegisterAfterCommit` y `RegisterAfterCompletion`, y el punto de extensión (seam) `ExecutionListener` para logging, métricas y tracing.
- [Bus de eventos](/gokeel/es/reference/event-bus/) — el `Bus` síncrono y en el mismo proceso (in-process), el registro tipado con `SubscribeTo`, la entrega direccionada con `Deliver`, la publicación multicast con `Publish` y el aislamiento de pánicos.
- [Outbox](/gokeel/es/reference/outbox/) — la outbox transaccional: el `Store`, los constructores `SQLiteStore` y `PostgresStore`, el `Registry`, `Publisher`, `Resubmitter`, el `JSONSerializer` y el ciclo de vida de `Publication`.
- [Migrador de esquemas](/gokeel/es/reference/migrator/) — el punto de extensión (seam) `Migrator`, el valor por defecto `NativeMigrator` sin dependencias, el `Schema` y `SchemaHistoryTable` exportados, y el adaptador opcional basado en goway.
- [Logging](/gokeel/es/reference/logging/) — el `Manager` de niveles, la herencia jerárquica de nombres, el documento `Configuration` con `ParseConfiguration` y `ParseLevels`, `SetLevel` y `ResetLevel` en tiempo de ejecución, y el puente clásico para `log`.
- [Configuración](/gokeel/es/reference/conf/) — el `Loader` sobre fuentes JSON en capas, la gramática de marcadores de posición `${NAME:default}`, la vinculación flexible de estructuras (relaxed struct binding) y `GenerateSchema` para el autocompletado del editor.

## La configuración de ejemplo

Cada ejemplo comparte una misma configuración: un `*sql.DB` abierto llamado `database`, un `transaction.Manager` llamado `manager` construido sobre este, un `context.Context` llamado `ctx` y un `eventbus.Bus` llamado `bus` para las páginas basadas en eventos.

```go
import (
	"context"
	"database/sql"

	"github.com/cgardev/gokeel/eventbus"
	"github.com/cgardev/gokeel/transaction"
	_ "modernc.org/sqlite" // your own driver, blank-imported
)

database, _ := sql.Open("sqlite", ":memory:")
manager := transaction.NewManager(database)

bus := eventbus.NewBus()
ctx := context.Background()
```

Dentro de una unidad de trabajo, los almacenes resuelven el ejecutor contra el que se ejecutan a partir del contexto en lugar de recibir un `*sql.Tx` a través de sus firmas. `manager.Querier(ctx)` devuelve la transacción activa mientras una unidad de trabajo está en progreso, y recurre a `database` para una sentencia con confirmación automática (auto-commit), de modo que el mismo código del almacén funciona en cualquiera de los dos entornos:

```go
_ = manager.Run(ctx, func(ctx context.Context) error {
	querier := manager.Querier(ctx) // the active transaction, bound to ctx
	_, err := querier.ExecContext(ctx, `INSERT INTO widgets (id) VALUES (?)`, "w1")
	return err // returning an error rolls the whole unit back
})
```

A lo largo de la referencia, `manager` es un `*transaction.Manager`, `database` es el `*sql.DB` subyacente, `ctx` es un `context.Context` y `bus` es un `*eventbus.Bus`. Las páginas de la outbox construyen un `Store`, un `Registry` y un `Publisher` sobre estos. Se utiliza SQLite para los ejemplos ejecutables; el mismo código funciona contra PostgreSQL abriendo una `database` de PostgreSQL y eligiendo el constructor de almacén (store) de PostgreSQL. Si está comenzando desde cero, la página [Primeros pasos](/gokeel/es/getting-started/) explica detalladamente la primera unidad de trabajo de extremo a extremo.
