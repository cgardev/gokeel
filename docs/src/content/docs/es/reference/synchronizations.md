---
title: Sincronizaciones y Listeners
description: Registro de sincronizaciones de commit, lectura del estado de la transacción y observación del ciclo de vida de la transacción con execution listeners en gokeel.
---

Una unidad de trabajo a menudo necesita hacer más que leer y escribir filas: puede necesitar ejecutar
un callback una vez que la transacción hace commit, vetar un commit que no debería ocurrir, u
observar el inicio y el commit de la transacción para el registro de logs. Esta página cubre los
callbacks de sincronización que el trabajo registra desde dentro de una transacción, el estado que
puede leer del contexto y los execution listeners que un `Manager` dispara alrededor de
la transacción física.

## Registro de sincronizaciones

Las funciones `Register*` programan un callback para una fase de la transacción activa.
Cada una acepta el contexto, el callback y valores opcionales de
`RegisterOption`. Los callbacks pertenecen a la transacción más externa, por lo que un
`Run` anidado o que se une se registra contra la misma unidad de trabajo.

Cada función devuelve un `bool`: `true` cuando una unidad de trabajo estaba activa y el
callback fue programado, `false` en una ruta no transaccional (por ejemplo
`Supports` o `Never` sin una transacción activa). El resultado `false` permite al
llamante recurrir a ejecutar el trabajo inmediatamente.

```go
import "github.com/cgardev/gokeel/transaction"

scheduled := transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
    metrics.Increment("order.placed")
})
if !scheduled {
    // No transaction was active, so there is nothing to wait for.
    metrics.Increment("order.placed")
}
```

El registro preserva la multiplicidad: el mismo callback registrado dos veces se ejecuta
dos veces.

## Las fases de sincronización

`RegisterBeforeCommit` programa un callback que se ejecuta dentro de la transacción,
justo antes del commit más externo. Devolver un error veta el commit y
fuerza un rollback. El callback recibe el contexto dentro de la transacción, por lo que cualquier
trabajo de base de datos que realice se ejecuta en la transacción.

```go
transaction.RegisterBeforeCommit(ctx, func(ctx context.Context) error {
    return validateInvariants(ctx)
})
```

`RegisterBeforeCompletion` programa un callback que se ejecuta justo antes de que la
transacción más externa haga commit o rollback, mientras la transacción todavía está
vinculada. No tiene resultado de error, por lo que no puede vetar devolviendo uno, pero un panic fuerza
un rollback y se vuelve a lanzar al llamante una vez que la transacción se ha resuelto.

```go
transaction.RegisterBeforeCompletion(ctx, func(ctx context.Context) {
    releaseAdvisoryLock(ctx)
})
```

`RegisterAfterCommit` programa un callback que se ejecuta después de que la transacción más
externa hace commit con éxito. El callback recibe un contexto cuya
transacción ha sido desvinculada, por lo que cualquier trabajo de base de datos que realice se ejecuta en la
base de datos, nunca en la transacción cerrada. Un panic del callback se recupera
y se registra en los logs, no se propaga: la transacción ya ha hecho commit de forma duradera.

```go
transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
    eventBus.Publish(ctx, OrderPlaced{ID: orderID})
})
```

`RegisterAfterCompletion` programa un callback que se ejecuta después de que la transacción más
externa hace commit o rollback, recibiendo el `Status` final para que pueda ramificar
según el resultado.

```go
transaction.RegisterAfterCompletion(ctx, func(ctx context.Context, status transaction.Status) {
    if status == transaction.StatusRolledBack {
        log.Warn("order placement rolled back")
    }
})
```

## Sincronizaciones de savepoint

`RegisterSavepoint` programa un callback que se ejecuta justo después de que se crea
un savepoint, es decir, cuando un `Run` `Nested` toma un savepoint de la transacción
activa. `RegisterSavepointRollback` programa un callback que se ejecuta justo
antes de un rollback a un savepoint. Ambos callbacks reciben el nombre del savepoint y
se ejecutan dentro de la transacción abierta, por lo que un panic revierte toda la transacción.

```go
transaction.RegisterSavepoint(ctx, func(ctx context.Context, savepoint string) {
    log.Debug("savepoint taken", "name", savepoint)
})

transaction.RegisterSavepointRollback(ctx, func(ctx context.Context, savepoint string) {
    log.Debug("rolling back to savepoint", "name", savepoint)
})
```

## Ordenación de callbacks

`WithOrder` es la `RegisterOption` que establece el orden de un callback dentro de su
fase. Los órdenes más bajos se ejecutan primero; los callbacks con órdenes iguales se ejecutan en orden
de registro. El orden por defecto es cero, por lo que un callback sin orden se ejecuta antes de cualquier
callback con orden positivo y después de cualquier callback con orden negativo.

```go
transaction.RegisterAfterCommit(ctx, flushOutbox, transaction.WithOrder(-10))
transaction.RegisterAfterCommit(ctx, notifyMetrics)            // order 0
transaction.RegisterAfterCommit(ctx, sendEmail, transaction.WithOrder(10))
// Runs flushOutbox, then notifyMetrics, then sendEmail.
```

## Lectura del estado de la transacción

`TransactionStatus` expone el estado en vivo de la unidad de trabajo vinculada al
contexto actual. Se obtiene a través de `StatusFromContext`, que devuelve el
estado y `true`, o un estado cero y `false` cuando no hay ninguna transacción activa.

```go
status, active := transaction.StatusFromContext(ctx)
if active && status.IsNewTransaction() {
    log.Debug("running in a freshly begun transaction", "name", status.Name())
}
```

El estado informa sobre la transacción sin mutarla:

- `Name()` devuelve la etiqueta establecida a través de `WithName`, o la cadena vacía.
- `IsNewTransaction()` informa si este `Run` inició la transacción en lugar de
  unirse o anidarse dentro de una existente.
- `HasSavepoint()` informa si este `Run` se ejecuta dentro de un savepoint, es decir,
  bajo la propagación `Nested`.
- `IsReadOnly()` informa si la transacción se inició como de solo lectura.
- `IsCompleted()` informa si la transacción se ha resuelto.
- `IsRollbackOnly()` informa si la transacción ha sido marcada como de solo rollback.

`SetRollbackOnly` marca la transacción para que el `Run` más externo la revierta incluso
cuando el trabajo devuelva nil. No tiene efecto una vez que la transacción se ha completado.

```go
status, _ := transaction.StatusFromContext(ctx)
status.SetRollbackOnly()
```

## Estados de finalización

El `Status` que recibe un callback de after-completion es uno de tres valores:

- `StatusCommitted` — la transacción hizo commit.
- `StatusRolledBack` — la transacción hizo rollback.
- `StatusUnknown` — el resultado no se pudo determinar, como un commit que
  falló a mitad de camino.

`Status` satisface a `fmt.Stringer`, por lo que se representa como `Committed`, `RolledBack`, o
`Unknown` en los logs.

## Funciones auxiliares libres

Dos funciones auxiliares leen o marcan la transacción activa sin mantener un
`TransactionStatus`, lo cual resulta conveniente en el código de registro de logs o monitoreo que está alejado
de la llamada a `Run`.

`CurrentTransactionName` devuelve el nombre de la transacción activa y `true`,
o la cadena vacía y `false` cuando no hay ninguna transacción activa.

```go
if name, active := transaction.CurrentTransactionName(ctx); active {
    log.Info("handling request", "transaction", name)
}
```

`MarkRollbackOnly` marca la transacción activa para que el `Run` más externo la revierta incluso
cuando el trabajo devuelva nil. Informa `false` cuando no hay ninguna transacción
activa. Es la forma de función libre de `TransactionStatus.SetRollbackOnly`.

```go
if !transaction.MarkRollbackOnly(ctx) {
    return errors.New("no transaction to abort")
}
```

## Execution listeners

`ExecutionListener` observa el ciclo de vida de la transacción física de la base de datos que
conduce un `Manager`: el inicio, commit y rollback de una transacción recién iniciada.
Está pensado para la observación sin estado — registros de logs, métricas, tracing — no para
tomar parte en la transacción; use las fases de sincronización de arriba para eso.
Los listeners se pasan a `NewManager` y se aplican a cada transacción que el `Manager`
conduce.

```go
listener := transaction.ExecutionListener{
    BeforeBegin: func(ctx context.Context, status transaction.TransactionStatus) {
        log.Debug("beginning transaction", "name", status.Name())
    },
    AfterCommit: func(ctx context.Context, status transaction.TransactionStatus, commitErr error) {
        if commitErr != nil {
            log.Error("commit failed", "error", commitErr)
        }
    },
}

manager := transaction.NewManager(database, listener)
```

Cada campo es un hook, y cada hook es opcional: un campo nil se omite. Los
hooks `Before*` se ejecutan justo antes de su paso; los hooks `After*` se ejecutan justo después de este,
recibiendo el error que produjo el paso o nil en caso de éxito.

- `BeforeBegin` / `AfterBegin` se disparan alrededor del inicio físico. En un inicio fallido,
  ninguna transacción está activa y el `Run` devuelve ese mismo error.
- `BeforeCommit` / `AfterCommit` se disparan alrededor del commit. El hook after se ejecuta
  después de las sincronizaciones de after-commit y de after-completion.
- `BeforeRollback` / `AfterRollback` se disparan alrededor del rollback. El hook after se ejecuta
  después de las sincronizaciones de after-completion.

Los hooks se disparan solo alrededor del inicio, commit y rollback físico de una nueva
transacción. No se disparan para un `Run` que se une a una transacción activa, ni
para las operaciones de savepoint de la propagación `Nested`. Un panic lanzado por un hook es
recuperado y registrado en los logs, nunca propagado, por lo que un callback de observación nunca puede
alterar el ciclo de vida de la transacción.

Para los modos de propagación y las opciones que dan forma a cada `Run`, consulte
[Propagación y Opciones](/gokeel/es/reference/propagation-and-options/). Para una
guía paso a paso de extremo a extremo, consulte la guía de
[Propagación y Sincronizaciones](/gokeel/es/guides/propagation-and-synchronizations/).
