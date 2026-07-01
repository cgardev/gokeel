---
title: Gestor de transacciones
description: Construcción de un Manager, ejecución de una unidad de trabajo vinculada al contexto, resolución del querier, y las interfaces Querier y Transactor.
---

Un `Manager` es propietario del ciclo de vida de la transacción de la base de datos: inicia, realiza commit y realiza rollback de las transacciones en las que se ejecuta una unidad de trabajo. El trabajo se proporciona como una función vinculada a un `context.Context`, y los stores resuelven el ejecutor contra el que se ejecutan a partir de ese contexto en lugar de recibir un `*sql.Tx` a través de sus firmas. El paquete depende únicamente de `database/sql`.

```go
import "github.com/cgardev/gokeel/transaction"
```

## NewManager

`NewManager` construye un `Manager` sobre un `*sql.DB` abierto. Los listeners variádicos son opcionales y observan los pasos de inicio, commit y rollback de cada nueva transacción que dirige el `Manager`.

```go
func NewManager(database *sql.DB, listeners ...ExecutionListener) *Manager
```

```go
database, err := sql.Open("sqlite", "app.db")
if err != nil {
    return err
}

manager := transaction.NewManager(database)
```

Un `Manager` es inmutable después de su construcción y es seguro para su uso concurrente, por lo que se comparte una única instancia en toda la aplicación.

### Execution listeners

Un `ExecutionListener` observa el ciclo de vida de la transacción física. Cada hook es opcional: se omite cualquier campo que sea nil. Los listeners están diseñados para la observación sin estado — registro (logging), métricas, rastreo (tracing) — no para formar parte de la transacción. Sus campos son:

- `BeforeBegin(ctx, status)` — se ejecuta antes de que se inicie la transacción.
- `AfterBegin(ctx, status, beginErr)` — se ejecuta después del paso de inicio, con el error que produjo o nil en caso de éxito.
- `BeforeCommit(ctx, status)` — se ejecuta dentro de la transacción, justo antes del commit.
- `AfterCommit(ctx, status, commitErr)` — se ejecuta después del paso de commit.
- `BeforeRollback(ctx, status)` — se ejecuta justo antes del rollback.
- `AfterRollback(ctx, status, rollbackErr)` — se ejecuta después del paso de rollback.

```go
logging := transaction.ExecutionListener{
    AfterCommit: func(ctx context.Context, status transaction.TransactionStatus, commitErr error) {
        slog.Info("transaction committed", "name", status.Name(), "error", commitErr)
    },
}

manager := transaction.NewManager(database, logging)
```

Los hooks se activan únicamente alrededor del inicio, commit y rollback físicos de una nueva transacción; no se activan para un `Run` que se une a una transacción activa ni para las operaciones de savepoint de la propagación `Nested`. Cualquier pánico provocado por un hook es recuperado y registrado, nunca propagado.

## Manager.Run

`Run` ejecuta `work` como una unidad de trabajo configurada por `opts`.

```go
func (manager *Manager) Run(
    ctx context.Context,
    work func(ctx context.Context) error,
    opts ...Option,
) error
```

Sin opciones, utiliza la propagación `Required` con el aislamiento predeterminado de la base de datos: se une a una transacción activa o inicia una, realiza el commit cuando `work` devuelve nil y realiza el rollback ante un error o pánico.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    if err := accounts.Debit(ctx, from, amount); err != nil {
        return err
    }
    return accounts.Credit(ctx, to, amount)
})
```

Ambas llamadas al store se ejecutan en la misma transacción. Devolver un error de cualquiera de ellas realiza el rollback de toda la transacción; devolver nil realiza el commit.

Las opciones son el equivalente programático de los atributos de `@Transactional` de Spring. `WithPropagation` selecciona cómo se relaciona la llamada con una transacción activa, `WithIsolation` solicita un nivel de aislamiento, `ReadOnly` marca la transacción como de solo lectura, `WithTimeout` limita su duración y `WithName` la etiqueta para el monitoreo.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    return reports.Generate(ctx)
}, transaction.ReadOnly(), transaction.WithName("nightly-report"))
```

Un `Run` anidado se une a la transacción que ya está vinculada al contexto, por lo que un caso de uso que abarca varios stores los ejecuta en una sola transacción sin necesidad de pasarla manualmente. Para ejecutar dentro de un savepoint de la transacción activa en su lugar, pase `WithPropagation(Nested)`.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    return manager.Run(ctx, func(ctx context.Context) error {
        return audit.Record(ctx, entry)
    }, transaction.WithPropagation(transaction.Nested))
})
```

El conjunto completo de comportamientos de propagación, aislamiento, tiempo de espera (timeout) y reglas de rollback se cubre en [Propagación y opciones](/gokeel/es/reference/propagation-and-options/).

## RunResult

`RunResult` ejecuta `work` como una unidad de trabajo al igual que `Manager.Run`, pero permite que `work` devuelva un valor junto con su error. Es una función libre en lugar de un método porque los métodos de Go no pueden ser genéricos.

```go
func RunResult[T any](
    ctx context.Context,
    transactor Transactor,
    work func(ctx context.Context) (T, error),
    opts ...Option,
) (T, error)
```

El valor que produjo `work` se devuelve junto con el error que reporta `Run`. En una transacción fallida o con rollback realizado, el invocador debe consultar el error en lugar del valor.

```go
order, err := transaction.RunResult(ctx, manager, func(ctx context.Context) (Order, error) {
    return orders.Create(ctx, request)
})
if err != nil {
    return Order{}, err
}
```

El argumento `transactor` es cualquier `Transactor`, por lo que `*Manager` se pasa directamente.

## Manager.Querier

`Querier` resuelve el ejecutor para el contexto actual: la transacción activa cuando una unidad de trabajo está en progreso; de lo contrario, la base de datos para una sentencia de auto-commit.

```go
func (manager *Manager) Querier(ctx context.Context) Querier
```

Los stores pasan el resultado como el argumento querier final de sus llamadas terminales, de modo que el mismo método del store participa en una transacción cuando se llama dentro de `Run` y se ejecuta con auto-commit cuando se llama fuera de él.

```go
func (s *AccountStore) Balance(ctx context.Context, id int64) (int64, error) {
    return gooq.Select1(db.Account.Balance).
        From(db.Account).
        Where(db.Account.Id.EQ(id)).
        FetchSingle(ctx, s.manager.Querier(ctx))
}
```

## Querier

`Querier` es la superficie de ejecución contra la que los stores ejecutan sus sentencias. Está implementada por `*sql.DB`, `*sql.Tx` y `*sql.Conn`, y su conjunto de métodos coincide con el querier mínimo que acepta un generador de consultas (query builder), por lo que el valor resuelto se puede pasar directamente a uno.

```go
type Querier interface {
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
```

## Transactor

`Transactor` es la porción del `Manager` de la que dependen los stores: ejecuta una unidad de trabajo y resuelve el querier contra el que se ejecuta el store. Está implementada por `*Manager`.

```go
type Transactor interface {
    Run(ctx context.Context, work func(ctx context.Context) error, opts ...Option) error
    Querier(ctx context.Context) Querier
}
```

Depender de `Transactor` en lugar del `*Manager` concreto mantiene un store verificable de forma aislada y también es el tipo que acepta `RunResult`.

```go
type AccountStore struct {
    transactor transaction.Transactor
}

func NewAccountStore(transactor transaction.Transactor) *AccountStore {
    return &AccountStore{transactor: transactor}
}
```

Para inspeccionar el estado en vivo de la unidad de trabajo activa o para registrar callbacks de sincronización, consulte [Estado de la transacción y sincronizaciones](/gokeel/es/reference/synchronizations/).
