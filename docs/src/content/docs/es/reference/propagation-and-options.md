---
title: Propagación y Opciones
description: Los comportamientos de propagación y las opciones que configuran una unidad de trabajo en gokeel.
---

Una unidad de trabajo se configura mediante las opciones pasadas a `Run`. Cada opción es un valor `Option`, el equivalente programático de un atributo de `@Transactional` de Spring: propagación, aislamiento, solo lectura, tiempo de espera, un nombre y las reglas de rollback. Esta página enumera las constantes de `Propagation` y cada función `Option` declarada en el paquete `transaction`.

```go
import "github.com/cgardev/gokeel/transaction"
```

## Propagación

La propagación selecciona cómo se relaciona un `Run` con una transacción que ya podría estar vinculada al contexto, reflejando los comportamientos de propagación de Spring. Hay cinco constantes; `Required` es la predeterminada y el valor al que `Run` resuelve cuando no se proporciona `WithPropagation`.

```go
transaction.Required  // join an active transaction or begin a new one
transaction.Supports  // join an active transaction, otherwise run with no transaction
transaction.Mandatory // join an active transaction, fail when none is active
transaction.Never     // run with no transaction, fail when one is active
transaction.Nested    // run within a savepoint of the active transaction
```

Los dos comportamientos de Spring que suspenden la transacción activa o abren una segunda transacción concurrente, `REQUIRES_NEW` y `NOT_SUPPORTED`, se omiten intencionadamente: en una base de datos SQLite de un solo escritor, una segunda transacción concurrente provocaría un deadlock contra el bloqueo de escritura que la primera ya mantiene.

`Propagation` implementa `String`, por lo que se representa como `Required`, `Supports`, `Mandatory`, `Never` o `Nested` en los logs y fallos de pruebas.

### Required

`Required` se une a una transacción activa o inicia una nueva. Es la propagación que la mayoría de los llamadores necesitan, y la seleccionada cuando no se proporciona la opción `WithPropagation`.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    return store.Save(ctx, order)
})
```

### Supports

`Supports` se une a una transacción activa cuando existe una, y de lo contrario ejecuta el trabajo sin transacción. Los callbacks de sincronización no se mantienen en la ruta no transaccional, por lo que las funciones `Register*` reportan `false` allí.

```go
err := manager.Run(ctx, work, transaction.WithPropagation(transaction.Supports))
```

### Mandatory

`Mandatory` se une a una transacción activa y falla con `ErrTransactionRequired` cuando ninguna está activa. Es la forma de asegurar que una función siempre debe ser llamada desde dentro de una unidad de trabajo existente.

```go
err := manager.Run(ctx, work, transaction.WithPropagation(transaction.Mandatory))
// err is ErrTransactionRequired when no transaction is active.
```

### Never

`Never` ejecuta el trabajo sin transacción y falla con `ErrTransactionNotAllowed` cuando una transacción ya está activa.

```go
err := manager.Run(ctx, work, transaction.WithPropagation(transaction.Never))
// err is ErrTransactionNotAllowed when a transaction is active.
```

### Nested

`Nested` se ejecuta dentro de un savepoint de la transacción activa, por lo que su trabajo puede hacer rollback al savepoint sin abortar la transacción externa. Inicia una nueva transacción cuando ninguna está activa.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    // SAVEPOINT transaction_savepoint_1
    return store.AttemptOptionalStep(ctx)
    // an error here rolls back to the savepoint, not the whole transaction
}, transaction.WithPropagation(transaction.Nested))
```

Consulte [Propagación y Sincronizaciones](/gokeel/es/guides/propagation-and-synchronizations/) para un recorrido de cada comportamiento en un gráfico de servicios.

## Las opciones

Cada opción a continuación es una función que devuelve un `Option`. Se pasan como la cola variádica de `Run`; las opciones posteriores anulan a las anteriores para el mismo campo. La unidad de trabajo predeterminada utiliza la propagación `Required` en el aislamiento predeterminado de la base de datos, sin indicación de solo lectura, sin tiempo de espera, sin nombre y con la regla de rollback predeterminada.

### WithPropagation

```go
func WithPropagation(propagation Propagation) Option
```

Selecciona el comportamiento de propagación. El valor predeterminado es `Required`.

```go
transaction.WithPropagation(transaction.Mandatory)
```

### WithIsolation

```go
func WithIsolation(level sql.IsolationLevel) Option
```

Establece el nivel de aislamiento que una transacción recién iniciada solicita al controlador (driver). No tiene efecto cuando la llamada se une a una transacción existente. Una llamada que se une a una transacción activa mientras solicita un nivel de aislamiento explícito diferente falla con `ErrIncompatibleJoin`.

```go
transaction.WithIsolation(sql.LevelSerializable)
```

### ReadOnly

```go
func ReadOnly() Option
```

Marca una transacción recién iniciada como de solo lectura, una sugerencia (hint) que el controlador (driver) puede utilizar para optimizar o rechazar escrituras. No tiene efecto cuando la llamada se une a una transacción existente, y una llamada de solo lectura que se une a una transacción de lectura y escritura falla con `ErrIncompatibleJoin`. La bandera (flag) es observable a través de `TransactionStatus.IsReadOnly`.

```go
err := manager.Run(ctx, report, transaction.ReadOnly())
```

### WithTimeout

```go
func WithTimeout(timeout time.Duration) Option
```

Limita la duración de una transacción recién iniciada: su contexto se cancela una vez que transcurre el tiempo de espera, por lo que una sentencia que se excede falla y la transacción hace rollback. No tiene efecto cuando la llamada se une a una transacción existente. Una duración de cero, la predeterminada, significa que no hay tiempo de espera; una duración negativa no es válida y hace que `Run` falle con `ErrInvalidTimeout`. Cuando transcurre el tiempo de espera, el error que devuelve `Run` envuelve a `ErrTransactionTimedOut`.

```go
err := manager.Run(ctx, work, transaction.WithTimeout(2*time.Second))
if errors.Is(err, transaction.ErrTransactionTimedOut) {
    // the transaction overran its own deadline and rolled back
}
```

Una cancelación del propio contexto del llamador no se reporta como un tiempo de espera.

### WithName

```go
func WithName(name string) Option
```

Etiqueta la unidad de trabajo, expuesta a través de `TransactionStatus.Name` y `CurrentTransactionName` para el registro (logging) y la monitorización. No tiene efecto sobre la transacción en sí.

```go
err := manager.Run(ctx, work, transaction.WithName("place-order"))
```

## Reglas de rollback

Por defecto, cada error no nulo (non-nil) hace rollback de la transacción. Las reglas de rollback reducen o restauran ese comportamiento. Una regla viene en dos formas: una forma `Error` que coincide con sentinelas a través de `errors.Is`, y una forma `Func` que toma un predicado. Una regla de rollback tiene precedencia sobre una regla de no-rollback, por lo que un error que coincide con ambas aún hace rollback.

### RollbackForError

```go
func RollbackForError(targets ...error) Option
```

Fuerza un rollback cuando el error del trabajo coincide, a través de `errors.Is`, con cualquiera de los sentinelas proporcionados, anulando cualquier regla de no-rollback que de otro modo lo eximiría. Es redundante con el comportamiento predeterminado, que hace rollback ante cualquier error, y solo es útil para volver a incluir un error que una regla más amplia `NoRollbackForError` o `NoRollbackForFunc` habría hecho commit.

```go
transaction.RollbackForError(ErrInventoryConflict)
```

### RollbackForFunc

```go
func RollbackForFunc(predicate func(error) bool) Option
```

Fuerza un rollback cuando `predicate` reporta `true` para el error del trabajo, anulando cualquier regla de no-rollback que de otro modo lo eximiría.

```go
transaction.RollbackForFunc(func(err error) bool {
    var conflict *ConflictError
    return errors.As(err, &conflict)
})
```

### NoRollbackForError

```go
func NoRollbackForError(targets ...error) Option
```

Mantiene la transacción como confirmable (committable) cuando el error del trabajo coincide, a través de `errors.Is`, con cualquiera de los sentinelas proporcionados, a menos que una regla `RollbackForError` o `RollbackForFunc` también coincida. El error todavía se devuelve al llamador.

```go
err := manager.Run(ctx, charge, transaction.NoRollbackForError(ErrReceiptDeferred))
// the transaction commits, yet err is ErrReceiptDeferred
```

### NoRollbackForFunc

```go
func NoRollbackForFunc(predicate func(error) bool) Option
```

Mantiene la transacción como confirmable (committable) cuando `predicate` reporta `true` para el error del trabajo. El error todavía se devuelve al llamador.

```go
transaction.NoRollbackForFunc(func(err error) bool {
    return errors.Is(err, ErrAlreadyProcessed)
})
```

## Combinación de opciones

Las opciones son independientes y se acumulan, por lo que un solo `Run` puede establecer la propagación, solicitar un nivel de aislamiento y agregar una regla de rollback a la vez. Las reglas de rollback en particular se añaden en lugar de reemplazarse, por lo que varias pueden aplicarse a una unidad de trabajo.

```go
err := manager.Run(ctx, placeOrder,
    transaction.WithName("place-order"),
    transaction.WithIsolation(sql.LevelSerializable),
    transaction.WithTimeout(5*time.Second),
    transaction.NoRollbackForError(ErrReceiptDeferred),
    transaction.RollbackForError(ErrInventoryConflict),
)
```

Las mismas opciones son aceptadas por `RunResult`, la función libre genérica que permite que el trabajo devuelva un valor junto con su error. Para el ciclo de vida que impulsa estas opciones, consulte el [Gestor de Transacciones](/gokeel/es/reference/transaction-manager/); para los callbacks que una unidad de trabajo puede registrar, consulte [Sincronizaciones y Listeners](/gokeel/es/reference/synchronizations/).
