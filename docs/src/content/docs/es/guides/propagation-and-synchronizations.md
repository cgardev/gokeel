---
title: Propagación y sincronizaciones
description: Seleccione cómo se relaciona un Run con una transacción activa, ajuste la transacción que inicia y conéctese a las fases de commit y rollback.
---

Una unidad de trabajo se abre con `Manager.Run` y su comportamiento es determinado por las funciones `Option` que le pase. La propagación responde a la pregunta "¿qué debería hacer este `Run` si ya hay una transacción vinculada al contexto?", las opciones de construcción ajustan la transacción que inicia y las fases de sincronización permiten que el trabajo se conecte al momento en que se resuelve. Todo en esta página reside en `github.com/cgardev/gokeel/transaction`.

## Propagación

`WithPropagation` selecciona el comportamiento de propagación. El valor predeterminado es `Required`:

```go
manager.Run(ctx, work, transaction.WithPropagation(transaction.Nested))
```

Hay cinco comportamientos, cada uno definido como una constante `Propagation`. Difieren únicamente en cómo reacciona un `Run` ante una transacción activa:

```go
transaction.Required  // join an active transaction, or begin one (default)
transaction.Supports  // join one if present, otherwise run without a transaction
transaction.Mandatory // join one, or fail with ErrTransactionRequired
transaction.Never     // fail with ErrTransactionNotAllowed when one is active
transaction.Nested    // take a savepoint of one, or begin a transaction
```

`Required` es la propagación que casi todos los llamadores quieren: un `Run` interno se une a la transacción que el `Run` externo vinculó al contexto, de modo que varios almacenes realizan commit como uno solo. `Mandatory` se adapta a un ayudante que nunca debe ejecutarse fuera de un caso de uso. `Supports` y `Never` se adaptan a una lectura que funciona con o sin una transacción ambiental.

`REQUIRES_NEW` y `NOT_SUPPORTED` están deliberadamente ausentes: necesitarían una segunda transacción concurrente, lo que causa un interbloqueo contra el único escritor de SQLite.

## Unirse frente a un savepoint

Bajo `Required`, `Supports` o `Mandatory`, un `Run` anidado *se une*: no inicia ni realiza commit, y un error fatal marca la unidad compartida como rollback únicamente (rollback only), por lo que el `Run` más externo aborte toda la transacción.

`Nested` es diferente. Abre un `SAVEPOINT` de la transacción activa, de modo que un error de rollback en su interior deshace hasta ese savepoint y la transacción externa sobrevive. El `Run` interno aún le devuelve el error a usted:

```go
err := manager.Run(ctx, func(ctx context.Context) error {
	return giftWrap(ctx) // returns ErrWrapUnavailable
}, transaction.WithPropagation(transaction.Nested))
// ROLLBACK TO SAVEPOINT undoes the nested work; the outer transaction lives on.
```

Cuando no hay ninguna transacción activa, `Nested` inicia una nueva, exactamente igual que `Required`.

## Opciones de construcción

Con la propagación como anclaje, los atributos restantes de `@Transactional` se presentan como un pequeño conjunto de funciones `Option` :

```go
transaction.WithIsolation(sql.LevelSerializable) // isolation of a new transaction
transaction.ReadOnly()                           // begin the transaction read only
transaction.WithTimeout(2 * time.Second)         // cancel its context after the duration
transaction.WithName("place-order")              // label it, surfaced on TransactionStatus
```

`WithIsolation`, `ReadOnly` y `WithTimeout` solo importan para una transacción completamente nueva: el `Run` más externo que realmente inicia una. No hacen nada cuando la llamada *se une* a una transacción existente. Si se une con una solicitud de aislamiento o de solo lectura que la transacción en ejecución no puede cumplir, `Run` falla con `ErrIncompatibleJoin`:

```go
return manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))
```

Un timeout de cero, el predeterminado, significa que no hay timeout; uno negativo falla rápidamente con `ErrInvalidTimeout` antes de que se ejecute cualquier trabajo. Cuando el timeout expira, el error que devuelve `Run` envuelve a `ErrTransactionTimedOut`.

## Reglas de rollback

Por defecto, cada error que devuelve el trabajo realiza un rollback de la transacción. Las reglas de rollback permiten que un error realice commit de todos modos, y el error aún se le devuelve a usted:

```go
return manager.Run(ctx, work,
	transaction.NoRollbackForError(ErrLoyaltyServiceDown), // commit despite this error
	transaction.RollbackForError(ErrLoyaltyFraud),         // but force a rollback for this
)
```

Una regla de rollback prevalece sobre una regla de no-rollback, por lo que un error que coincida con ambas aún realiza un rollback:

```go
// error returned by work
//   matches a RollbackForError / RollbackForFunc rule?   -> ROLLBACK (wins)
//   matches a NoRollbackForError / NoRollbackForFunc rule? -> COMMIT (error still returned)
//   otherwise                                            -> ROLLBACK (default)
```

`NoRollbackForError` y `RollbackForError` coinciden mediante `errors.Is`. Las formas de predicado `NoRollbackForFunc` y `RollbackForFunc` toman un `func(error) bool` cuando un centinela no es suficiente:

```go
transaction.RollbackForFunc(func(err error) bool {
	var conflict *ConflictError
	return errors.As(err, &conflict)
})
```

## Fases de sincronización

El trabajo registra callbacks para las fases del `Run` dentro del cual se ejecuta. Cada función `Register` devuelve `false` cuando no hay ninguna unidad de trabajo activa, de modo que la ruta de auto-commit puede recurrir a la ejecución inmediata. Se ejecutan en este orden:

```go
// work() -> [before-commit: may VETO] -> [before-completion]
//                                              |
//                                  COMMIT --------------- ROLLBACK
//                                     |                       |
//                              [after-commit]    (after-commit skipped)
//                                     |-----------------------|
//                                          [after-completion]   (always, gets the Status)
```

`RegisterBeforeCommit` se ejecuta dentro de la transacción aún abierta; devolver un error veta el commit y fuerza un rollback, exactamente igual que si el trabajo hubiera fallado:

```go
transaction.RegisterBeforeCommit(ctx, func(ctx context.Context) error {
	return orderTotalsBalance(ctx) // a non-nil error vetoes the commit
})
```

`RegisterAfterCommit` se ejecuta solo después de un commit duradero, en un contexto cuya transacción ha sido *desacoplada*, por lo que el trabajo de base de datos allí realiza auto-commit contra la base de datos. Cualquier pánico del mismo se recupera y se registra en el log, nunca se propaga:

```go
scheduled := transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
	_ = notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
})
if !scheduled {
	_ = notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID) // no transaction: do it now
}
```

`RegisterBeforeCompletion` se ejecuta justo antes del commit o rollback, mientras la transacción aún está vinculada. No puede vetar mediante un retorno, ya que no tiene un resultado de error, pero un pánico fuerza un rollback y se vuelve a lanzar una vez que la transacción se ha resuelto.

`RegisterAfterCompletion` siempre se ejecuta, tanto en commit como en rollback, recibiendo el `Status` final:

```go
transaction.RegisterAfterCompletion(ctx,
	func(ctx context.Context, status transaction.Status) {
		slog.Info("transaction settled", "outcome", status.String())
	})
```

El `Status` es `StatusCommitted`, `StatusRolledBack` o `StatusUnknown`.

## Callbacks de savepoint

La propagación `Nested` tiene su propio par. `RegisterSavepoint` se activa justo después de que se crea un savepoint, y `RegisterSavepointRollback` justo antes de un rollback a uno. Ambos reciben el nombre del savepoint, y un pánico de cualquiera de ellos realiza un rollback de toda la transacción:

```go
transaction.RegisterSavepoint(ctx, func(ctx context.Context, savepoint string) {
	slog.Info("savepoint taken", "savepoint", savepoint)
})
transaction.RegisterSavepointRollback(ctx, func(ctx context.Context, savepoint string) {
	slog.Info("rolling back to savepoint", "savepoint", savepoint)
})
```

## Ordenamiento dentro de una fase

Por defecto, los callbacks dentro de una fase se ejecutan en orden de registro. `WithOrder` establece un orden explícito: el valor más bajo se ejecuta primero, y los órdenes iguales mantienen el orden de registro. El orden predeterminado es cero, por lo que un callback sin orden se ejecuta después de cualquier callback con orden negativo y antes de cualquier callback con orden positivo:

```go
transaction.RegisterBeforeCommit(ctx, validateTotals, transaction.WithOrder(-1)) // runs first
transaction.RegisterBeforeCommit(ctx, recordAudit, transaction.WithOrder(1))     // runs later
```

`WithOrder` es una `RegisterOption` y funciona en cada función `Register` anterior.

Continúe en [El bus de eventos](/gokeel/es/guides/event-bus/) para publicar eventos en el proceso, o en [El outbox transaccional](/gokeel/es/guides/transactional-outbox/) para publicarlos solo después de que la transacción realice commit.
